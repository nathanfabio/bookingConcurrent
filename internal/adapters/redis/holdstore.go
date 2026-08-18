package redis

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	appbooking "github.com/nathanfabio/bookingConcurrent/internal/application/booking"
	domain "github.com/nathanfabio/bookingConcurrent/internal/domain/booking"

	goredis "github.com/redis/go-redis/v9"
)

// Compile-time proof the adapter satisfies the port.
var _ appbooking.HoldStore = (*HoldStore)(nil)

// Key schema (ADR 0002):
//
//	seat:{screeningID}:{row}:{num}  -> holdToken            (SET NX EX)
//	session:{sessionID}              -> hash{screening_id, row, num,
//	                                      user_id, hold_token, expires_at}
//	user:{userID}:holds              -> ZSET member=sessionID score=expiryUnix
//	holds:expiring                   -> global ZSET, same shape
//
// The seat key's VALUE is the hold token, not "1" or a user ID: release and
// the expiry sweeper compare it before deleting, which is what keeps a late
// operation on an old hold from destroying a newer hold of the same seat.
const (
	globalHoldsKey  = "holds:expiring"
	sessionPrefix   = "session:"
	userHoldsPrefix = "user:"
)

func seatKey(screeningID string, seat domain.Seat) string {
	return "seat:" + screeningID + ":" + seat.Row + ":" + strconv.Itoa(seat.Number)
}

func sessionKey(sessionID string) string { return sessionPrefix + sessionID }

func userHoldsKey(userID string) string { return userHoldsPrefix + userID + ":holds" }

// holdScript claims a seat atomically. It is ONE round trip and ONE atomic
// unit: limit check, seat claim, session write, and bookkeeping either all
// apply or none do (see ADR 0002 for why MULTI/pipeline cannot express
// this).
//
// KEYS[1] seat key        KEYS[2] session key
// KEYS[3] user holds ZSET KEYS[4] global holds ZSET
// ARGV[1] hold token      ARGV[2] ttl seconds      ARGV[3] expiry unix ts
// ARGV[4] max holds       ARGV[5] session ID       ARGV[6] screening ID
// ARGV[7] row             ARGV[8] seat number      ARGV[9] user ID
//
// Returns: "OK" | "LIMIT" | "TAKEN"
const holdScript = `
if redis.call('ZCARD', KEYS[3]) >= tonumber(ARGV[4]) then
  return 'LIMIT'
end
if redis.call('SET', KEYS[1], ARGV[1], 'NX', 'EX', tonumber(ARGV[2])) then
  redis.call('HSET', KEYS[2],
    'screening_id', ARGV[6],
    'row', ARGV[7],
    'num', ARGV[8],
    'user_id', ARGV[9],
    'hold_token', ARGV[1],
    'expires_at', ARGV[3])
  redis.call('EXPIRE', KEYS[2], tonumber(ARGV[2]))
  redis.call('ZADD', KEYS[3], ARGV[3], ARGV[5])
  redis.call('ZADD', KEYS[4], ARGV[3], ARGV[5])
  return 'OK'
end
return 'TAKEN'
`

// releaseScript gives up a hold atomically, with an ownership check and a
// compare-and-delete on the hold token.
//
// KEYS[1] session key     KEYS[2] user holds ZSET   KEYS[3] global holds ZSET
// ARGV[1] user ID         ARGV[2] session ID
//
// The seat key name is rebuilt from the session hash inside the script.
// It is only deleted if its value still equals this hold's token: between
// this hold's expiry and this (late) release, another hold may have
// acquired the seat, and deleting that seat key would corrupt the other
// user's claim.
//
// Returns: "OK" | "NOT_FOUND" | "NOT_OWNER"
const releaseScript = `
local owner = redis.call('HGET', KEYS[1], 'user_id')
if not owner then
  return 'NOT_FOUND'
end
if owner ~= ARGV[1] then
  return 'NOT_OWNER'
end
local token = redis.call('HGET', KEYS[1], 'hold_token')
local seatKey = 'seat:' .. redis.call('HGET', KEYS[1], 'screening_id') ..
  ':' .. redis.call('HGET', KEYS[1], 'row') ..
  ':' .. redis.call('HGET', KEYS[1], 'num')
if redis.call('GET', seatKey) == token then
  redis.call('DEL', seatKey)
end
redis.call('DEL', KEYS[1])
redis.call('ZREM', KEYS[2], ARGV[2])
redis.call('ZREM', KEYS[3], ARGV[2])
return 'OK'
`

// HoldStore is the Redis-backed implementation of booking.HoldStore.
type HoldStore struct {
	client        *goredis.Client
	holdTTL       time.Duration
	maxHolds      int
	holdScript    *goredis.Script
	releaseScript *goredis.Script
}

// NewHoldStore wires the adapter. holdTTL and maxHolds come from validated
// config; tests construct stores with short TTLs to exercise expiry.
func NewHoldStore(client *goredis.Client, holdTTL time.Duration, maxHolds int) *HoldStore {
	return &HoldStore{
		client:        client,
		holdTTL:       holdTTL,
		maxHolds:      maxHolds,
		holdScript:    goredis.NewScript(holdScript),
		releaseScript: goredis.NewScript(releaseScript),
	}
}

// Hold atomically claims the seat. The hold's lifetime is taken from
// hold.ExpiresAt (single source of truth — the use case computes it from
// the configured TTL), never from a second clock inside the adapter.
func (s *HoldStore) Hold(ctx context.Context, hold domain.Hold) error {
	if err := hold.Validate(time.Now()); err != nil {
		return err
	}

	ttl := time.Until(hold.ExpiresAt)
	// Round UP to whole seconds. The direction is deliberate: truncating
	// would free the seat up to a second BEFORE the domain expiry, so a
	// confirm inside the user's legitimate window could fail on a key that
	// vanished early. Ceiling keeps the key alive at most a second past
	// ExpiresAt instead — and the pure domain rule (Hold.CanBeConfirmed)
	// still refuses anything at or after ExpiresAt, so business expiry
	// stays exact while Redis errs on the conservative side.
	ttlSeconds := int64(ttl.Seconds())
	if ttl > time.Duration(ttlSeconds)*time.Second {
		ttlSeconds++
	}
	if ttlSeconds < 1 {
		// ttl <= 0 despite Validate: the clock moved between validation and
		// here. Refuse rather than write a key that is born expired.
		return errors.New("booking: hold expired before it could be written")
	}

	keys := []string{
		seatKey(hold.ScreeningID, hold.Seat),
		sessionKey(hold.SessionID),
		userHoldsKey(hold.UserID),
		globalHoldsKey,
	}
	args := []any{
		hold.HoldToken,
		ttlSeconds,
		hold.ExpiresAt.Unix(),
		s.maxHolds,
		hold.SessionID,
		hold.ScreeningID,
		hold.Seat.Row,
		hold.Seat.Number,
		hold.UserID,
	}

	res, err := s.holdScript.Run(ctx, s.client, keys, args...).Text()
	if err != nil {
		return fmt.Errorf("booking: hold script: %w", err)
	}
	switch res {
	case "OK":
		return nil
	case "LIMIT":
		return domain.ErrHoldLimitExceeded
	case "TAKEN":
		return domain.ErrSeatAlreadyHeld
	default:
		return fmt.Errorf("booking: hold script returned unexpected result %q", res)
	}
}

// Release atomically releases the hold owned by userID, or fails with
// ErrHoldNotFound / ErrNotHoldOwner. See releaseScript for the
// token-comparison guarantee.
func (s *HoldStore) Release(ctx context.Context, sessionID, userID string) error {
	keys := []string{
		sessionKey(sessionID),
		userHoldsKey(userID),
		globalHoldsKey,
	}
	res, err := s.releaseScript.Run(ctx, s.client, keys, userID, sessionID).Text()
	if err != nil {
		return fmt.Errorf("booking: release script: %w", err)
	}
	switch res {
	case "OK":
		return nil
	case "NOT_FOUND":
		return domain.ErrHoldNotFound
	case "NOT_OWNER":
		return domain.ErrNotHoldOwner
	default:
		return fmt.Errorf("booking: release script returned unexpected result %q", res)
	}
}

// Get loads the live hold for a session. HGETALL is a single atomic read;
// an expired session is simply gone (Redis TTL), which maps to
// ErrHoldNotFound.
func (s *HoldStore) Get(ctx context.Context, sessionID string) (*domain.Hold, error) {
	fields, err := s.client.HGetAll(ctx, sessionKey(sessionID)).Result()
	if err != nil {
		return nil, fmt.Errorf("booking: read session: %w", err)
	}
	if len(fields) == 0 {
		return nil, domain.ErrHoldNotFound
	}

	num, err := strconv.Atoi(fields["num"])
	if err != nil {
		return nil, fmt.Errorf("booking: corrupt session payload (num=%q): %w", fields["num"], err)
	}
	expiresUnix, err := strconv.ParseInt(fields["expires_at"], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("booking: corrupt session payload (expires_at=%q): %w", fields["expires_at"], err)
	}
	seat, err := domain.NewSeat(fields["row"], num)
	if err != nil {
		return nil, fmt.Errorf("booking: corrupt session payload: %w", err)
	}

	return &domain.Hold{
		SessionID:   sessionID,
		ScreeningID: fields["screening_id"],
		Seat:        seat,
		UserID:      fields["user_id"],
		HoldToken:   fields["hold_token"],
		ExpiresAt:   time.Unix(expiresUnix, 0),
	}, nil
}

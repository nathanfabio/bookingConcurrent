package redis

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	appbooking "github.com/nathanfabio/bookingConcurrent/internal/application/booking"
	appholdexpiry "github.com/nathanfabio/bookingConcurrent/internal/application/holdexpiry"
	domain "github.com/nathanfabio/bookingConcurrent/internal/domain/booking"

	goredis "github.com/redis/go-redis/v9"
)

// Compile-time proof the adapter satisfies BOTH ports over the same data:
// the booking service's HoldStore and the sweeper's ExpiredHoldStore (two
// consumers, two small interfaces — ADR 0001's shape, same as PaymentRepo).
var (
	_ appbooking.HoldStore           = (*HoldStore)(nil)
	_ appholdexpiry.ExpiredHoldStore = (*HoldStore)(nil)
)

// Key schema (ADR 0002, global member refined by ADR 0010):
//
//	seat:{screeningID}:{row}:{num}  -> holdToken            (SET NX EX)
//	session:{sessionID}              -> hash{screening_id, row, num,
//	                                      user_id, hold_token, expires_at}
//	user:{userID}:holds              -> ZSET member=sessionID score=expiryUnix
//	holds:expiring                   -> ZSET member="userID:sessionID",
//	                                      score=expiryUnix
//
// The seat key's VALUE is the hold token, not "1" or a user ID: release and
// the expiry sweeper compare it before deleting, which is what keeps a late
// operation on an old hold from destroying a newer hold of the same seat.
//
// The GLOBAL ZSET's member is composite because the sweeper must reclaim
// the per-USER ZSET entry too, and by sweep time the session hash — the
// only other place the userID lives — has usually TTL-expired (ADR 0010).
// The user ZSET's member stays the bare sessionID: its KEY already carries
// the userID, and release/sweep both know which user they are cleaning.
const (
	globalHoldsKey  = "holds:expiring"
	seatPrefix      = "seat:"
	sessionPrefix   = "session:"
	userHoldsPrefix = "user:"
)

func seatKey(screeningID string, seat domain.Seat) string {
	return seatPrefix + screeningID + ":" + seat.Row + ":" + strconv.Itoa(seat.Number)
}

// parseSeatKey is the inverse of seatKey for one screening. It returns the
// seat named by a seat:{screeningID}:{row}:{num} key that is already known
// to belong to screeningID, and false if the key is not a valid seat key.
// Keeping the parse strict (and the caller skip-and-log on false) means one
// corrupt key can never fail a whole seat-map render.
func parseSeatKey(screeningID, key string) (domain.Seat, bool) {
	prefix := seatPrefix + screeningID + ":"
	if !strings.HasPrefix(key, prefix) {
		return domain.Seat{}, false
	}
	rest := strings.TrimPrefix(key, prefix)
	row, numStr, found := strings.Cut(rest, ":")
	if !found {
		return domain.Seat{}, false
	}
	num, err := strconv.Atoi(numStr)
	if err != nil {
		return domain.Seat{}, false
	}
	seat, err := domain.NewSeat(row, num)
	if err != nil {
		return domain.Seat{}, false
	}
	return seat, true
}

func sessionKey(sessionID string) string { return sessionPrefix + sessionID }

func userHoldsKey(userID string) string { return userHoldsPrefix + userID + ":holds" }

// CountActiveHolds supplies the scrape-time gauge. ZCARD would include expired
// bookkeeping waiting for the sweeper; only scores STRICTLY after now count.
// The second-resolution boundary matches Get/SweepExpired (ADR 0010).
func (s *HoldStore) CountActiveHolds(ctx context.Context, now time.Time) (int64, error) {
	return s.client.ZCount(ctx, globalHoldsKey, "("+strconv.FormatInt(now.Unix(), 10), "+inf").Result()
}

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
// Optional KEYS[5] is the per-user request binding. ARGV[10..12] carry
// the original deadline, binding retention in seconds and last permitted
// creation time in Unix milliseconds. The reply is an array with a status
// and, on REPLAY, the original session, token and deadline.
const holdScript = `
if KEYS[5] then
  local saved = redis.call('HMGET', KEYS[5], 'seat', 'session', 'token', 'deadline')
  if saved[1] then
    if saved[1] ~= KEYS[1] then return {'CONFLICT'} end
    local sessionKey = 'session:' .. saved[2]
    local expiry = redis.call('HGET', sessionKey, 'expires_at')
    if not expiry or tonumber(expiry) <= tonumber(redis.call('TIME')[1])
       or redis.call('GET', KEYS[1]) ~= saved[3] then return {'CLOSED'} end
    return {'REPLAY', saved[2], saved[3], saved[4]}
  end
  local clock = redis.call('TIME')
  if tonumber(clock[1])*1000 + math.floor(tonumber(clock[2])/1000) >= tonumber(ARGV[12]) then
    return {'CLOSED'}
  end
end
if redis.call('ZCARD', KEYS[3]) >= tonumber(ARGV[4]) then
  return {'LIMIT'}
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
  redis.call('ZADD', KEYS[4], ARGV[3], ARGV[9] .. ':' .. ARGV[5])
  if KEYS[5] then
    redis.call('HSET', KEYS[5], 'seat', KEYS[1], 'session', ARGV[5], 'token', ARGV[1], 'deadline', ARGV[10])
    redis.call('EXPIRE', KEYS[5], ARGV[11])
  end
  return {'OK'}
end
return {'TAKEN'}
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
redis.call('ZREM', KEYS[3], ARGV[1] .. ':' .. ARGV[2])
return 'OK'
`

// sweepScript reclaims ONE expired hold, token-checked — the sweeper half
// of ADR 0002's teardown rule ("the hold-expiry sweeper follows the same
// rule"), landing with cmd/worker as promised (ADR 0010).
//
// KEYS[1] session key      KEYS[2] user holds ZSET   KEYS[3] global holds ZSET
// ARGV[1] session ID       ARGV[2] composite member  ARGV[3] now (unix secs)
//
// Three outcomes:
//
//	GONE  — the session hash already TTL-expired (the normal case: hash,
//	        seat key, and score all die at the same wall-clock moment).
//	        Only bookkeeping remains, so only bookkeeping is removed. The
//	        seat key is deliberately NOT touched here: it either expired
//	        too, or it belongs to a NEWER hold — and a newer hold's key must
//	        never die by an older hold's sweep.
//	SWEPT — the hash outlived its expiry score (the ceiling-rounded TTL
//	        keeps raw keys alive up to a second past ExpiresAt, ADR 0002).
//	        Full teardown, with the same compare-and-delete as release: the
//	        seat key dies ONLY if its value still equals this hold's token.
//	ALIVE — score and hash disagree (unreachable via holdScript, which
//	        writes both from the same value; only hand-tampered data gets
//	        here). Defensive skip: never mutate on inconsistent state.
//
// Returns: "GONE" | "SWEPT" | "ALIVE"
const sweepScript = `
local expiresAt = redis.call('HGET', KEYS[1], 'expires_at')
if not expiresAt then
  redis.call('ZREM', KEYS[2], ARGV[1])
  redis.call('ZREM', KEYS[3], ARGV[2])
  return 'GONE'
end
if tonumber(expiresAt) > tonumber(ARGV[3]) then
  return 'ALIVE'
end
local token = redis.call('HGET', KEYS[1], 'hold_token')
local seatKey = 'seat:' .. redis.call('HGET', KEYS[1], 'screening_id') ..
  ':' .. redis.call('HGET', KEYS[1], 'row') ..
  ':' .. redis.call('HGET', KEYS[1], 'num')
if redis.call('GET', seatKey) == token then
  redis.call('DEL', seatKey)
end
redis.call('DEL', KEYS[1])
redis.call('ZREM', KEYS[2], ARGV[1])
redis.call('ZREM', KEYS[3], ARGV[2])
return 'SWEPT'
`

// HoldStore is the Redis-backed implementation of booking.HoldStore and
// holdexpiry.ExpiredHoldStore.
type HoldStore struct {
	client        *goredis.Client
	holdTTL       time.Duration
	maxHolds      int
	holdScript    *goredis.Script
	releaseScript *goredis.Script
	sweepScript   *goredis.Script
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
		sweepScript:   goredis.NewScript(sweepScript),
	}
}

// Hold atomically claims the seat. The hold's lifetime is taken from
// hold.ExpiresAt (single source of truth — the use case computes it from
// the configured TTL), never from a second clock inside the adapter.
func (s *HoldStore) Hold(ctx context.Context, hold domain.Hold) error {
	_, err := s.Claim(ctx, hold, appbooking.HoldKey{})
	return err
}

func (s *HoldStore) Claim(ctx context.Context, hold domain.Hold, key appbooking.HoldKey) (domain.Hold, error) {
	if err := hold.Validate(time.Now()); err != nil {
		return domain.Hold{}, err
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
		return domain.Hold{}, errors.New("booking: hold expired before it could be written")
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

	if key.Value != "" {
		keys = append(keys, "hold-request:"+hold.UserID+":"+key.Value)
		args = append(args, hold.ExpiresAt.Format(time.RFC3339Nano), max(ttlSeconds, int64(appbooking.HoldKeyRetention/time.Second)), key.CreateBefore.UnixMilli())
	}
	result, err := s.holdScript.Run(ctx, s.client, keys, args...).StringSlice()
	if err != nil {
		return domain.Hold{}, fmt.Errorf("booking: hold script: %w", err)
	}
	if len(result) == 0 {
		return domain.Hold{}, errors.New("booking: empty hold script result")
	}
	switch result[0] {
	case "OK":
		return hold, nil
	case "REPLAY":
		if len(result) != 4 {
			return domain.Hold{}, errors.New("booking: invalid replay result")
		}
		expiry, err := time.Parse(time.RFC3339Nano, result[3])
		if err != nil {
			return domain.Hold{}, fmt.Errorf("booking: replay deadline: %w", err)
		}
		hold.SessionID, hold.HoldToken, hold.ExpiresAt = result[1], result[2], expiry
		return hold, nil
	case "CONFLICT":
		return domain.Hold{}, domain.ErrHoldKeyConflict
	case "CLOSED":
		return domain.Hold{}, domain.ErrHoldKeyClosed
	case "LIMIT":
		return domain.Hold{}, domain.ErrHoldLimitExceeded
	case "TAKEN":
		return domain.Hold{}, domain.ErrSeatAlreadyHeld
	default:
		return domain.Hold{}, fmt.Errorf("booking: unexpected hold script status %q", result[0])
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

// HeldSeats enumerates the seats currently held for a screening — the seat
// map's "held" layer (ADR 0006).
//
// It SCANs seat:{screeningID}:* rather than walking the holds:expiring
// ZSET: the ZSETs deliberately retain stale members after silent TTL expiry
// (that bookkeeping outlives the keys by design, ADR 0002), so they would
// over-report. A seat key, by contrast, exists if and only if its hold is
// live — Redis only returns it while the TTL has not run out — so no
// separate liveness re-check is needed. SCAN is a cursor walk, not an atomic
// snapshot: the port contract documents the advisory semantics, and a
// concurrent hold/release may or may not appear in the result.
//
// Screening IDs are fixed-width UUIDs, so the seat:{screeningID}: prefix
// cannot accidentally match another screening's keys.
func (s *HoldStore) HeldSeats(ctx context.Context, screeningID string) ([]domain.Seat, error) {
	seats := make([]domain.Seat, 0)
	iter := s.client.Scan(ctx, 0, seatPrefix+screeningID+":*", 0).Iterator()
	for iter.Next(ctx) {
		seat, ok := parseSeatKey(screeningID, iter.Val())
		if !ok {
			// One corrupt key must not sink the whole map. Skip it and keep
			// rendering; the detail is operator material, not client material.
			slog.WarnContext(ctx, "skipping unparseable hold seat key",
				slog.String("key", iter.Val()))
			continue
		}
		seats = append(seats, seat)
	}
	if err := iter.Err(); err != nil {
		return nil, fmt.Errorf("booking: scan held seats: %w", err)
	}
	return seats, nil
}

// SweepExpired implements holdexpiry.ExpiredHoldStore: it walks the global
// holds:expiring ZSET by score — the bounded, time-ordered work queue ADR
// 0002 built it to be — and reclaims every hold expired at or before now,
// up to limit members per call.
//
// The boundary is INCLUSIVE (score <= now.Unix()) on purpose: the domain
// treats a hold as expired AT ExpiresAt (Hold.IsExpired: !now.Before(...)),
// and the ZSET score is the same truncated-Unix value the session hash
// stores and Get hands back — so sweep timing and CanBeConfirmed agree to
// the second, and the ceiling-rounded TTL padding (ADR 0002) only ever
// decides whether teardown arrives as SWEPT (hash still there) or GONE
// (TTL beat us to it). Both are correct; neither frees a seat the domain
// would still confirm.
//
// Per-member failures never fail the batch (the HeldSeats philosophy): one
// corrupt member is logged and skipped — or, when unparseable, discarded,
// because a poison member would otherwise re-enter every LIMIT window
// forever. That discard path doubles as the upgrade route for pre-M6
// bare-sessionID members, whose per-user entries are unrecoverable
// (ADR 0010's documented consequence).
func (s *HoldStore) SweepExpired(ctx context.Context, now time.Time, limit int) (int, error) {
	// ZRangeArgs (not the deprecated ZRangeByScore): ByScore + Offset/Count
	// is the same bounded score window, expressed in the Redis 6.2+ form.
	members, err := s.client.ZRangeArgs(ctx, goredis.ZRangeArgs{
		Key:     globalHoldsKey,
		Start:   "-inf",
		Stop:    strconv.FormatInt(now.Unix(), 10),
		ByScore: true,
		Offset:  0,
		Count:   int64(limit),
	}).Result()
	if err != nil {
		return 0, fmt.Errorf("booking: sweep: enumerate expired holds: %w", err)
	}

	swept := 0
	for _, member := range members {
		userID, sessionID, ok := splitGlobalMember(member)
		if !ok {
			slog.ErrorContext(ctx, "discarding unparseable global holds member (corrupt or pre-M6 legacy)",
				slog.String("member", member))
			if err := s.client.ZRem(ctx, globalHoldsKey, member).Err(); err != nil {
				slog.WarnContext(ctx, "could not discard unparseable global holds member; it will retry next sweep",
					slog.String("error", err.Error()))
			}
			continue
		}

		keys := []string{sessionKey(sessionID), userHoldsKey(userID), globalHoldsKey}
		res, err := s.sweepScript.Run(ctx, s.client, keys, sessionID, member, now.Unix()).Text()
		if err != nil {
			// Script-level failure (Redis hiccup mid-batch): log and move
			// on — the member stays queued for the next tick.
			slog.ErrorContext(ctx, "sweep script failed for member; skipping",
				slog.String("session_id", sessionID),
				slog.String("error", err.Error()))
			continue
		}
		switch res {
		case "GONE", "SWEPT":
			swept++
		case "ALIVE":
			// Score and hash disagree — only hand-tampered data gets here.
			// Leave it alone (never mutate on inconsistent state); it stops
			// matching the score window the moment its real expiry passes.
			slog.WarnContext(ctx, "sweep skipped a hold whose hash disagrees with its score",
				slog.String("session_id", sessionID))
		default:
			slog.ErrorContext(ctx, "sweep script returned unexpected result",
				slog.String("result", res),
				slog.String("session_id", sessionID))
		}
	}
	return swept, nil
}

// splitGlobalMember parses a composite "userID:sessionID" global-ZSET
// member. The split is on the LAST colon: the sessionID is a server-minted
// UUID and therefore guaranteed colon-free, while a userID in principle is
// not — so the rightmost segment is the only unambiguous sessionID.
func splitGlobalMember(member string) (userID, sessionID string, ok bool) {
	i := strings.LastIndex(member, ":")
	if i <= 0 || i == len(member)-1 {
		return "", "", false
	}
	return member[:i], member[i+1:], true
}

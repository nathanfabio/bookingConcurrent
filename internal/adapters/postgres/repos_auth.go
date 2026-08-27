package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nathanfabio/bookingConcurrent/internal/adapters/postgres/sqlcgen"
	domainuser "github.com/nathanfabio/bookingConcurrent/internal/domain/user"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// UserRepo maps between generated rows and the user domain model
// (migrations/00001). Thin like the other repos: translation only, no
// decisions.
type UserRepo struct {
	q *sqlcgen.Queries
}

// NewUserRepo builds the repository over the shared pool.
func NewUserRepo(pool *pgxpool.Pool) *UserRepo {
	return &UserRepo{q: sqlcgen.New(pool)}
}

// Create inserts a new user; the database assigns id and created_at. A
// losing race against the unique lower(email) index surfaces as
// domainuser.ErrEmailTaken.
func (r *UserRepo) Create(ctx context.Context, u domainuser.User) (domainuser.User, error) {
	row, err := r.q.CreateUser(ctx, sqlcgen.CreateUserParams{
		Email:        u.Email,
		PasswordHash: u.PasswordHash,
		DisplayName:  u.DisplayName,
	})
	if isUniqueViolation(err) {
		return domainuser.User{}, domainuser.ErrEmailTaken
	}
	if err != nil {
		return domainuser.User{}, fmt.Errorf("user: create: %w", err)
	}
	return userFromRow(row), nil
}

// GetByEmail matches case-insensitively (the query lowercases both sides).
func (r *UserRepo) GetByEmail(ctx context.Context, email string) (domainuser.User, error) {
	row, err := r.q.GetUserByEmail(ctx, email)
	if errors.Is(err, pgx.ErrNoRows) {
		return domainuser.User{}, domainuser.ErrUserNotFound
	}
	if err != nil {
		return domainuser.User{}, fmt.Errorf("user: get by email: %w", err)
	}
	return userFromRow(row), nil
}

// GetByID loads one user by primary key.
func (r *UserRepo) GetByID(ctx context.Context, id string) (domainuser.User, error) {
	row, err := r.q.GetUserByID(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return domainuser.User{}, domainuser.ErrUserNotFound
	}
	if err != nil {
		return domainuser.User{}, fmt.Errorf("user: get by id: %w", err)
	}
	return userFromRow(row), nil
}

func userFromRow(row sqlcgen.User) domainuser.User {
	return domainuser.User{
		ID:           row.ID,
		Email:        row.Email,
		PasswordHash: row.PasswordHash,
		DisplayName:  row.DisplayName,
		Role:         domainuser.Role(row.Role),
		CreatedAt:    ts(row.CreatedAt),
	}
}

// RefreshTokenRepo implements the rotating refresh-token ledger
// (migrations/00006, ADR 0004). Unlike the other repos it holds the pool
// directly, because Rotate composes several queries inside one
// transaction.
type RefreshTokenRepo struct {
	pool *pgxpool.Pool
	q    *sqlcgen.Queries
}

// NewRefreshTokenRepo builds the repository over the shared pool.
func NewRefreshTokenRepo(pool *pgxpool.Pool) *RefreshTokenRepo {
	return &RefreshTokenRepo{pool: pool, q: sqlcgen.New(pool)}
}

// Create inserts a token row. The ID is supplied by the caller (the auth
// Service), matching the port contract that the application mints
// identity.
func (r *RefreshTokenRepo) Create(ctx context.Context, t domainuser.RefreshToken) (domainuser.RefreshToken, error) {
	row, err := r.q.CreateRefreshToken(ctx, sqlcgen.CreateRefreshTokenParams{
		ID:        t.ID,
		UserID:    t.UserID,
		FamilyID:  t.FamilyID,
		TokenHash: t.TokenHash,
		ExpiresAt: pgtype.Timestamptz{Time: t.ExpiresAt, Valid: true},
	})
	if err != nil {
		return domainuser.RefreshToken{}, fmt.Errorf("refresh token: create: %w", err)
	}
	return refreshTokenFromRow(row), nil
}

// GetByHash loads one token row by its SHA-256 hash.
func (r *RefreshTokenRepo) GetByHash(ctx context.Context, tokenHash string) (domainuser.RefreshToken, error) {
	row, err := r.q.GetRefreshTokenByHash(ctx, tokenHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return domainuser.RefreshToken{}, domainuser.ErrRefreshTokenUnknown
	}
	if err != nil {
		return domainuser.RefreshToken{}, fmt.Errorf("refresh token: get: %w", err)
	}
	return refreshTokenFromRow(row), nil
}

// RevokeFamily marks every live token of the family revoked.
func (r *RefreshTokenRepo) RevokeFamily(ctx context.Context, familyID string) error {
	if err := r.q.RevokeRefreshTokenFamily(ctx, familyID); err != nil {
		return fmt.Errorf("refresh token: revoke family: %w", err)
	}
	return nil
}

// Rotate implements auth.RefreshTokenStore. The whole read-decide-write is
// one transaction anchored on a SELECT ... FOR UPDATE row lock — the
// Postgres mirror of the atomicity the Redis hold script gives holds
// (ADR 0002, ADR 0004). Concurrent Rotates for the same token serialize on
// the lock; exactly one wins, and every other caller observes the token
// already consumed and reports theft.
func (r *RefreshTokenRepo) Rotate(ctx context.Context, tokenHash string, next domainuser.RefreshToken) (domainuser.RefreshToken, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return domainuser.RefreshToken{}, fmt.Errorf("refresh token: rotate: begin: %w", err)
	}
	// Commit is the explicit success path; anything else rolls back. The
	// theft branch is the exception that proves the rule — it commits,
	// because persisting the family revocation is the point of that
	// transaction (see below).
	defer func() { _ = tx.Rollback(ctx) }()

	q := r.q.WithTx(tx)

	old, err := q.GetRefreshTokenByHashForUpdate(ctx, tokenHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return domainuser.RefreshToken{}, domainuser.ErrRefreshTokenUnknown
	}
	if err != nil {
		return domainuser.RefreshToken{}, fmt.Errorf("refresh token: rotate: lock: %w", err)
	}
	oldToken := refreshTokenFromRow(old)

	// Reuse (revoked or already replaced) is checked BEFORE expiry: a
	// replaced token that has also lapsed is still theft (ADR 0004).
	if oldToken.ReuseIsTheft() {
		// This is the theft branch. The family revocation is the WORK of
		// this transaction, so it must be COMMITTED, not rolled back.
		if err := q.RevokeRefreshTokenFamily(ctx, old.FamilyID); err != nil {
			return domainuser.RefreshToken{}, fmt.Errorf("refresh token: rotate: revoke family: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return domainuser.RefreshToken{}, fmt.Errorf("refresh token: rotate: commit revocation: %w", err)
		}
		return domainuser.RefreshToken{}, domainuser.ErrRefreshTokenReused
	}

	if oldToken.IsExpired(time.Now()) {
		// Normal lifecycle: no revocation, nothing to persist.
		return domainuser.RefreshToken{}, domainuser.ErrRefreshTokenExpired
	}

	// Active: stamp the successor with the locked row's identity, insert
	// it, and mark the old token consumed — all inside the transaction.
	next.UserID = old.UserID
	next.FamilyID = old.FamilyID
	created, err := q.CreateRefreshToken(ctx, sqlcgen.CreateRefreshTokenParams{
		ID:        next.ID,
		UserID:    next.UserID,
		FamilyID:  next.FamilyID,
		TokenHash: next.TokenHash,
		ExpiresAt: pgtype.Timestamptz{Time: next.ExpiresAt, Valid: true},
	})
	if err != nil {
		return domainuser.RefreshToken{}, fmt.Errorf("refresh token: rotate: insert successor: %w", err)
	}
	replacedBy, err := uuidToPgtype(created.ID)
	if err != nil {
		return domainuser.RefreshToken{}, fmt.Errorf("refresh token: rotate: %w", err)
	}
	if err := q.MarkRefreshTokenReplaced(ctx, sqlcgen.MarkRefreshTokenReplacedParams{
		ID:         old.ID,
		ReplacedBy: replacedBy,
	}); err != nil {
		return domainuser.RefreshToken{}, fmt.Errorf("refresh token: rotate: mark replaced: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return domainuser.RefreshToken{}, fmt.Errorf("refresh token: rotate: commit: %w", err)
	}
	return refreshTokenFromRow(created), nil
}

func refreshTokenFromRow(row sqlcgen.RefreshToken) domainuser.RefreshToken {
	return domainuser.RefreshToken{
		ID:         row.ID,
		UserID:     row.UserID,
		FamilyID:   row.FamilyID,
		TokenHash:  row.TokenHash,
		ExpiresAt:  ts(row.ExpiresAt),
		CreatedAt:  ts(row.CreatedAt),
		RevokedAt:  tsNull(row.RevokedAt),
		ReplacedBy: uuidFromPgtype(row.ReplacedBy),
	}
}

// tsNull unwraps a NULLABLE timestamptz. Unlike ts (which assumes NOT
// NULL), a NULL here is meaningful — it means "not revoked" — so it maps
// to a nil pointer rather than a zero time.
func tsNull(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	return &t.Time
}

// uuidToPgtype converts a string UUID to the nullable pgtype form sqlc
// uses for the replaced_by column.
func uuidToPgtype(id string) (pgtype.UUID, error) {
	parsed, err := uuid.Parse(id)
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("parse uuid %q: %w", id, err)
	}
	return pgtype.UUID{Bytes: parsed, Valid: true}, nil
}

// uuidFromPgtype converts the nullable pgtype UUID back to a string,
// mapping NULL to "" (the domain's "not replaced" sentinel).
func uuidFromPgtype(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	return uuid.UUID(u.Bytes).String()
}

// isUniqueViolation reports whether err is a Postgres unique_violation
// (SQLSTATE 23505) — the signal that a registration lost a race to the
// unique index.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

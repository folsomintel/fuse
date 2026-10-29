package apikeys

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// apiKeyPrefix is the human-visible prefix on every raw API key. It makes
// keys greppable in logs and secret scanners and unambiguous when an
// operator is looking at one.
const apiKeyPrefix = "fuse_sk_"

// apiKeyIDPrefix is the prefix on the public key id (the handle used to
// list and revoke a key without holding the secret).
const apiKeyIDPrefix = "ak_"

// ErrAPIKeyNotFound is returned by Revoke when no key has the given id.
var ErrAPIKeyNotFound = errors.New("api key not found")

// APIKeyRecord is the metadata for a single API key. It never carries the
// raw key or its hash — those never leave the store.
type APIKeyRecord struct {
	ID         string
	Label      string
	CreatedAt  time.Time
	LastUsedAt *time.Time
	RevokedAt  *time.Time

	// Scopes nil is a full-access key. A non-nil list (possibly empty)
	// limits the key to what the scopes grant.
	Scopes []string
	// EnvID limits a scoped key to one environment and its forks. Empty
	// means any environment.
	EnvID string
	// ExpiresAt is when the key stops authenticating. Nil means never.
	ExpiresAt *time.Time
	// ActorID and ActorName pin the actor every request on the key is
	// recorded as. Empty ActorID means no pinned actor.
	ActorID   string
	ActorName string
	// CreatedBy is the principal that minted the key: "master" or a key id.
	CreatedBy string

	// FirstUse is set by Authenticate on the request that used the key for
	// the first time. It is not stored.
	FirstUse bool
}

// NewKey is what Create mints.
type NewKey struct {
	Label     string
	Scopes    []string
	EnvID     string
	ExpiresAt *time.Time
	ActorID   string
	ActorName string
	CreatedBy string
}

// APIKeyStore persists revocable API keys in Postgres. Keys are stored as
// SHA-256 hashes; the raw key is shown once at creation and never again.
type APIKeyStore struct {
	db *sql.DB
}

// NewAPIKeyStore creates a Postgres-backed API key store. It shares the
// same *sql.DB (and therefore the same migrations) as the state store.
func NewAPIKeyStore(db *sql.DB) *APIKeyStore {
	return &APIKeyStore{db: db}
}

// hashAPIKey returns the SHA-256 of a raw key. A fast hash is correct here
// because keys are 256-bit random — there is no low-entropy secret to
// protect against brute force, so bcrypt/argon2 would buy nothing.
func hashAPIKey(rawKey string) []byte {
	sum := sha256.Sum256([]byte(rawKey))
	return sum[:]
}

// GenerateAPIKey returns a new (id, rawKey) pair. The rawKey is the secret
// the caller presents as a bearer token; the id is the public handle. Only
// the hash of rawKey is ever persisted.
func GenerateAPIKey() (id, rawKey string, err error) {
	secretBytes := make([]byte, 32)
	if _, err := rand.Read(secretBytes); err != nil {
		return "", "", fmt.Errorf("generate api key: %w", err)
	}
	idBytes := make([]byte, 8)
	if _, err := rand.Read(idBytes); err != nil {
		return "", "", fmt.Errorf("generate api key id: %w", err)
	}
	rawKey = apiKeyPrefix + base64.RawURLEncoding.EncodeToString(secretBytes)
	id = apiKeyIDPrefix + hex.EncodeToString(idBytes)
	return id, rawKey, nil
}

// Create generates a new API key, stores its hash + metadata, and returns
// the record together with the raw key. The raw key is returned exactly
// once and cannot be recovered afterward.
func (s *APIKeyStore) Create(ctx context.Context, k NewKey, now time.Time) (APIKeyRecord, string, error) {
	id, rawKey, err := GenerateAPIKey()
	if err != nil {
		return APIKeyRecord{}, "", err
	}

	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO orchestrator_api_keys
			(id, key_hash, label, created_at, scopes, env_id, expires_at, actor_id, actor_name, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	`, id, hashAPIKey(rawKey), k.Label, now, k.Scopes, nullString(k.EnvID), k.ExpiresAt,
		nullString(k.ActorID), nullString(k.ActorName), k.CreatedBy); err != nil {
		return APIKeyRecord{}, "", fmt.Errorf("insert api key: %w", err)
	}

	return APIKeyRecord{
		ID: id, Label: k.Label, CreatedAt: now,
		Scopes: k.Scopes, EnvID: k.EnvID, ExpiresAt: k.ExpiresAt,
		ActorID: k.ActorID, ActorName: k.ActorName, CreatedBy: k.CreatedBy,
	}, rawKey, nil
}

const keyColumns = `id, label, created_at, last_used_at, revoked_at,
	scopes, env_id, expires_at, actor_id, actor_name, created_by`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanKey(row rowScanner, extra ...any) (APIKeyRecord, error) {
	var (
		rec                     APIKeyRecord
		scopes                  []string
		envID, actorID, actorNm sql.NullString
	)
	m := pgtype.NewMap()
	dest := append([]any{&rec.ID, &rec.Label, &rec.CreatedAt, &rec.LastUsedAt, &rec.RevokedAt,
		m.SQLScanner(&scopes), &envID, &rec.ExpiresAt, &actorID, &actorNm, &rec.CreatedBy}, extra...)
	if err := row.Scan(dest...); err != nil {
		return rec, err
	}
	rec.Scopes = scopes
	rec.EnvID = envID.String
	rec.ActorID = actorID.String
	rec.ActorName = actorNm.String
	return rec, nil
}

// Authenticate reports whether rawToken matches a live key: not revoked and
// not past its expiry. On success it returns the key's record and bumps
// last_used_at on a best-effort basis; a failed bump never fails
// authentication. Implements the APIKeyAuthenticator interface consumed by
// the api package's BearerAuth.
func (s *APIKeyStore) Authenticate(ctx context.Context, rawToken string) (APIKeyRecord, bool) {
	var keyHash []byte
	rec, err := scanKey(s.db.QueryRowContext(ctx, `
		SELECT `+keyColumns+`, key_hash
		FROM orchestrator_api_keys
		WHERE key_hash = $1
	`, hashAPIKey(rawToken)), &keyHash)
	if err != nil {
		// sql.ErrNoRows (unknown key) and any DB error both deny.
		return APIKeyRecord{}, false
	}
	now := time.Now().UTC()
	if rec.RevokedAt != nil {
		return APIKeyRecord{}, false
	}
	if rec.ExpiresAt != nil && !now.Before(*rec.ExpiresAt) {
		return APIKeyRecord{}, false
	}
	// Defense in depth: confirm the stored hash matches in constant time.
	// The unique index already guarantees at most one row, but this guards
	// against any future non-unique lookup path.
	if subtle.ConstantTimeCompare(keyHash, hashAPIKey(rawToken)) != 1 {
		return APIKeyRecord{}, false
	}

	// Best-effort last_used_at bump; ignore errors. The first bump is
	// conditional so exactly one request sees FirstUse.
	if rec.LastUsedAt == nil {
		res, err := s.db.ExecContext(ctx, `
			UPDATE orchestrator_api_keys SET last_used_at = $1 WHERE id = $2 AND last_used_at IS NULL
		`, now, rec.ID)
		if err == nil {
			if n, _ := res.RowsAffected(); n == 1 {
				rec.FirstUse = true
				return rec, true
			}
		}
	}
	_, _ = s.db.ExecContext(ctx, `
		UPDATE orchestrator_api_keys SET last_used_at = $1 WHERE id = $2
	`, now, rec.ID)

	return rec, true
}

// List returns metadata for all keys (live and revoked), newest first. It
// never returns key hashes or raw keys.
func (s *APIKeyStore) List(ctx context.Context) ([]APIKeyRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+keyColumns+`
		FROM orchestrator_api_keys
		ORDER BY created_at DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("list api keys: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []APIKeyRecord
	for rows.Next() {
		rec, err := scanKey(rows)
		if err != nil {
			return nil, fmt.Errorf("scan api key: %w", err)
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// RevokeEnv revokes every live key scoped to envID. Destroying an
// environment calls it, so an invite never outlives the sandbox it was for.
func (s *APIKeyStore) RevokeEnv(ctx context.Context, envID string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE orchestrator_api_keys SET revoked_at = $1
		WHERE env_id = $2 AND revoked_at IS NULL
	`, now, envID)
	if err != nil {
		return fmt.Errorf("revoke env api keys: %w", err)
	}
	return nil
}

// RebindEnv moves keys scoped to one environment id to another. A live
// migration gives the same logical environment a new id, and its invites
// follow it.
func (s *APIKeyStore) RebindEnv(ctx context.Context, fromID, toID string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE orchestrator_api_keys SET env_id = $2 WHERE env_id = $1
	`, fromID, toID)
	if err != nil {
		return fmt.Errorf("rebind env api keys: %w", err)
	}
	return nil
}

func nullString(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}

// Revoke marks the key with the given id revoked. Revoking an
// already-revoked key is idempotent (no error). Returns ErrAPIKeyNotFound
// if no key has that id.
func (s *APIKeyStore) Revoke(ctx context.Context, id string, now time.Time) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE orchestrator_api_keys
		SET revoked_at = COALESCE(revoked_at, $1)
		WHERE id = $2
	`, now, id)
	if err != nil {
		return fmt.Errorf("revoke api key: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("revoke api key rows: %w", err)
	}
	if n == 0 {
		return ErrAPIKeyNotFound
	}
	return nil
}

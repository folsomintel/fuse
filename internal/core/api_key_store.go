package orchestrator

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
func (s *APIKeyStore) Create(ctx context.Context, label string, now time.Time) (APIKeyRecord, string, error) {
	id, rawKey, err := GenerateAPIKey()
	if err != nil {
		return APIKeyRecord{}, "", err
	}

	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO orchestrator_api_keys (id, key_hash, label, created_at)
		VALUES ($1, $2, $3, $4)
	`, id, hashAPIKey(rawKey), label, now); err != nil {
		return APIKeyRecord{}, "", fmt.Errorf("insert api key: %w", err)
	}

	return APIKeyRecord{ID: id, Label: label, CreatedAt: now}, rawKey, nil
}

// Authenticate reports whether rawToken matches a live (non-revoked) key.
// On success it returns the key id and bumps last_used_at on a best-effort
// basis — a failed bump never fails authentication. Implements the
// APIKeyAuthenticator interface consumed by the api package's BearerAuth.
func (s *APIKeyStore) Authenticate(ctx context.Context, rawToken string) (string, bool) {
	var (
		id      string
		keyHash []byte
		revoked *time.Time
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT id, key_hash, revoked_at
		FROM orchestrator_api_keys
		WHERE key_hash = $1
	`, hashAPIKey(rawToken)).Scan(&id, &keyHash, &revoked)
	if err != nil {
		// sql.ErrNoRows (unknown key) and any DB error both deny.
		return "", false
	}
	if revoked != nil {
		return "", false
	}
	// Defense in depth: confirm the stored hash matches in constant time.
	// The unique index already guarantees at most one row, but this guards
	// against any future non-unique lookup path.
	if subtle.ConstantTimeCompare(keyHash, hashAPIKey(rawToken)) != 1 {
		return "", false
	}

	// Best-effort last_used_at bump; ignore errors.
	_, _ = s.db.ExecContext(ctx, `
		UPDATE orchestrator_api_keys SET last_used_at = $1 WHERE id = $2
	`, time.Now().UTC(), id)

	return id, true
}

// List returns metadata for all keys (live and revoked), newest first. It
// never returns key hashes or raw keys.
func (s *APIKeyStore) List(ctx context.Context) ([]APIKeyRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, label, created_at, last_used_at, revoked_at
		FROM orchestrator_api_keys
		ORDER BY created_at DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("list api keys: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []APIKeyRecord
	for rows.Next() {
		var rec APIKeyRecord
		if err := rows.Scan(&rec.ID, &rec.Label, &rec.CreatedAt, &rec.LastUsedAt, &rec.RevokedAt); err != nil {
			return nil, fmt.Errorf("scan api key: %w", err)
		}
		out = append(out, rec)
	}
	return out, rows.Err()
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

# API Keys — Design

Date: 2026-06-09
Status: approved, ready for implementation

## Problem

The orchestrator API authenticates callers with a single static master token
(`ORCH_AUTH_TOKEN`, constant-time compared in `api.BearerAuth`). That is the
right model for the operator and the operator's own server-to-server clients
(e.g. the Elixir frontend, which holds the master token in deploy-time config).

It is the wrong model once **third parties** call the API directly (customers,
their CI, their scripts). With one shared secret you cannot:

- revoke one holder's access without rotating the master token and breaking
  everyone;
- tell holders apart in audit logs;
- label a credential by what it is for.

This spec adds **revocable, labeled API keys** as a second credential type
alongside the master token.

## Scope

In scope:

- A new `orchestrator_api_keys` table (forward-only migration `0002`).
- A second accept path in `BearerAuth`: master token **or** a live API key.
- Three management endpoints to mint, list, and revoke keys.

Explicitly **out of scope** (deferred to their own spec):

- **Tenant isolation / multi-tenancy.** Every valid key is full-access, exactly
  like the master token. The existing `tenant_id` column is dormant/derived
  today (snapshots derive it from `taskID`; VMs take it as an optional
  caller-supplied field with no credential binding). Real isolation — binding a
  credential to a tenant and filtering every VM/snapshot/host read+write and the
  SSE stream — is a from-scratch rework and gets its own design cycle.
- **Scopes / permissions** (read-only vs full). All-or-nothing only.
- **User accounts, sessions, roles.** None of that exists and none is added.

## Data model

New migration `internal/core/migrations/0002_api_keys.sql`, following the
existing embedded, version-tracked, forward-only pattern (`go:embed`, applied at
startup by `ApplyMigrations()`, tracked in `orchestrator_schema_migrations`).

```sql
CREATE TABLE IF NOT EXISTS orchestrator_api_keys (
    id           TEXT PRIMARY KEY,          -- public handle, safe to log (e.g. "ak_7f3c...")
    key_hash     BYTEA NOT NULL,            -- SHA-256 of the raw key; never the raw key
    label        TEXT NOT NULL DEFAULT '',  -- human memory aid: "ci", "partner-acme"
    created_at   TIMESTAMPTZ NOT NULL,
    last_used_at TIMESTAMPTZ,               -- NULL until first use; bumped on auth
    revoked_at   TIMESTAMPTZ               -- NULL = live; non-NULL = revoked
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_api_keys_hash ON orchestrator_api_keys (key_hash);
```

Key decisions:

- **Hash, not encrypt.** The rest of the codebase *encrypts* tokens
  (`auth_token_encrypted`, AES-GCM) because it must read them back. API keys are
  the opposite: only ever compared, never recovered. So store a SHA-256 hash;
  show the raw key once at creation. A DB leak exposes hashes, not live keys.
  This is a deliberate departure from the encryption pattern.
- **SHA-256 is sufficient** (not bcrypt/argon2). Those defend low-entropy
  passwords against brute force. API keys are 256-bit random, so a fast hash is
  correct and a unique index on `key_hash` makes lookup one indexed query.
- **`id` vs key.** The raw key (`fuse_sk_` + 32 random bytes, base64url) is the
  secret. `id` (`ak_` + short random) is a public handle used for list/revoke,
  so managing a key never requires the secret.

## Auth path change

`BearerAuth` today: `BearerAuth(expectedToken string, onFailure AuthFailureFunc)`,
one constant-time compare, plus a `fuse_session` cookie read (untouched here).

New behavior — master token **or** live API key:

```
present token (from Authorization header or fuse_session cookie) →
  1. constant-time match against ORCH_AUTH_TOKEN?  → accept as MASTER
  2. else SHA-256(token), look up live key in DB?  → accept as KEY (bump last_used_at)
  3. else → 401 + onFailure(remoteAddr, method, path, requestID)   [unchanged]
```

- Empty master token = no-op pass-through (insecure/dev mode) — **preserved**.
- Master path stays `subtle.ConstantTimeCompare`. Key path is hash-then-lookup,
  naturally timing-safe (comparing a hash of high-entropy input).
- `last_used_at` bump is best-effort: a failed update must not fail auth.
- The authenticated principal (master vs key, and key id) is recorded in the
  request context so management endpoints can enforce master-only and so audit
  logs can attribute calls.

New dependency, injected as an interface (not `*sql.DB`) to keep middleware
testable:

```go
type APIKeyAuthenticator interface {
    // Authenticate returns (keyID, true) for a live key; ("", false) if the
    // token matches no key or the key is revoked.
    Authenticate(ctx context.Context, rawToken string) (string, bool)
}

BearerAuth(expectedToken string, keys APIKeyAuthenticator, onFailure AuthFailureFunc)
```

`keys == nil` is allowed (key auth disabled) — only the master path runs.

## Management endpoints

Three handlers on the existing `*Handler`, registered **inside** the
`BearerAuth` group but guarded **master-token-only**: a caller authenticated *as
an API key* must not mint, list, or revoke keys. The guard reads the principal
from request context; a key-authed caller gets 403 (`CodeUnauthorized`).

| Endpoint | Action | Response |
|---|---|---|
| `POST /v1/api-keys` | generate key, hash, store `id` + `label` | **raw key once** + `{id, label, created_at}` |
| `GET /v1/api-keys` | list metadata | array of `{id, label, created_at, last_used_at, revoked_at}` — never the key |
| `DELETE /v1/api-keys/{id}` | set `revoked_at` (idempotent) | 204; 404 if unknown id |

Request/response bodies use the existing JSON envelope and `writeJSON` /
`writeError` helpers. `POST` accepts `{"label": "..."}` (label optional, defaults
to empty). The raw key is returned **only** in the `POST` response and never
again.

## Storage

New `APIKeyStore` on the Postgres store, mirroring `PostgresStateStore` —
hand-written SQL via `ExecContext` / `QueryRowContext`, no ORM:

- `Create(ctx, id, keyHash, label, createdAt) error`
- `Authenticate(ctx, rawToken) (keyID string, ok bool)` — hashes, selects
  `WHERE key_hash = $1 AND revoked_at IS NULL`, bumps `last_used_at` best-effort.
- `List(ctx) ([]APIKeyRecord, error)`
- `Revoke(ctx, id, revokedAt) (found bool, err error)`

The store implements `APIKeyAuthenticator` (via `Authenticate`) so it plugs
directly into `BearerAuth`.

## Config / wiring

- No new env vars required. Key auth is enabled whenever the Postgres store is
  available (`DATABASE_URL` set); without a DB, only the master token path runs.
- `server/main.go` constructs the `APIKeyStore` from the existing DB handle and
  passes it into `Handler` / `BearerAuth` alongside `AuthToken`.

## Errors

Reuse existing codes (`api/types.go`):

- Missing/invalid credential → `CodeUnauthorized` (401), via existing
  `BearerAuth` failure path.
- Key-authed caller hitting a management endpoint → `CodeUnauthorized` (403).
- Unknown id on `DELETE` → `CodeNotFound` (404).
- Bad JSON on `POST` → `CodeInvalidArgument` (400).

## Testing

- **Store** (against a test DB, existing pattern): create→authenticate
  round-trip; authenticate rejects revoked and unknown; `last_used_at` is set;
  `Revoke` returns found/not-found correctly.
- **`BearerAuth` table tests**: master accept; valid-key accept; revoked-key
  reject; unknown-token reject; empty-master no-op; cookie path still works.
- **Management endpoints**: master-authed caller can mint/list/revoke; key-authed
  caller gets 403; `POST` returns the raw key exactly once; `GET` never leaks a
  key; `DELETE` of unknown id → 404.
- **Generation**: keys are high-entropy, prefixed `fuse_sk_`, and ids `ak_`.

## Out-of-scope follow-ups (noted, not built)

- Tenant isolation / multi-tenancy (its own spec).
- Key scopes (read-only).
- Rate limiting per key.
- Optional master-token retirement once keys cover all clients.

package fuse

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// Build states.
const (
	BuildRunning   = "running"
	BuildSucceeded = "succeeded"
	BuildFailed    = "failed"
)

// CreateBuildRequest runs a Fusefile's setup phase on the orchestrator. The
// caller plans the setup: it renders the scripts and derives each step's
// layer key, as `fuse build` does from a Fusefile.
type CreateBuildRequest struct {
	Name           string            `json:"name"`
	Spec           Spec              `json:"spec"`
	ManifestInline string            `json:"manifest_inline,omitempty"`
	Secrets        map[string]string `json:"secrets,omitempty"`
	// Arch is the arch layers are looked up and filed under. Empty lets
	// the server derive it.
	Arch string `json:"arch,omitempty"`
	// Keep leaves the builder running instead of destroying it.
	Keep  bool       `json:"keep,omitempty"`
	Setup BuildSetup `json:"setup"`
}

// BuildSetup is the setup phase cut into what the builder execs: the prelude
// every exec starts with, the file block a cold build writes first, and one
// script per step.
type BuildSetup struct {
	Prelude string      `json:"prelude"`
	Files   string      `json:"files,omitempty"`
	Steps   []BuildStep `json:"steps"`
}

// BuildStep is one setup step. Key is its layer cache key; a step without
// one, and every step after it, is not cached.
type BuildStep struct {
	Script string `json:"script"`
	Key    string `json:"key,omitempty"`
}

// Build is a fusefile build.
type Build struct {
	ID             string       `json:"id"`
	Name           string       `json:"name"`
	State          string       `json:"state"`
	Step           string       `json:"step,omitempty"`
	BuilderID      string       `json:"builder_id,omitempty"`
	Arch           string       `json:"arch,omitempty"`
	CacheHits      int          `json:"cache_hits"`
	SeedSnapshotID string       `json:"seed_snapshot_id,omitempty"`
	Layers         []BuildLayer `json:"layers"`
	// SnapshotID is the build artifact, once taken.
	SnapshotID string `json:"snapshot_id,omitempty"`
	Error      string `json:"error,omitempty"`
	// Output is the redacted tail of the setup exec that failed.
	Output     string     `json:"output,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// Done reports whether the build finished.
func (b *Build) Done() bool { return b != nil && b.State != BuildRunning }

// BuildLayer is one setup step of a build: Status is "hit" (the seed covers
// it), "miss" (it runs and is snapshotted) or "skip" (it cannot be cached).
type BuildLayer struct {
	Index      int    `json:"index"`
	Key        string `json:"key,omitempty"`
	Status     string `json:"status"`
	SnapshotID string `json:"snapshot_id,omitempty"`
}

// BuildsService runs fusefile builds on the orchestrator.
type BuildsService struct {
	t *transport
}

// Create starts a build and returns it at once, still running. It keeps
// running if the caller goes away. Without WithIdempotencyKey every call
// sends a fresh random key, so a retry with the same key returns the build
// the first call started.
func (s *BuildsService) Create(ctx context.Context, req CreateBuildRequest, opts ...CallOption) (*Build, error) {
	if s == nil || s.t == nil {
		return nil, errors.New("builds service is not configured")
	}
	var o callOptions
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	if o.idempotencyKey == "" {
		o.idempotencyKey = newIdempotencyKey()
	}
	hreq, err := s.t.newRequest(ctx, http.MethodPost, "/v1/builds", nil, req)
	if err != nil {
		return nil, err
	}
	hreq.Header.Set(idempotencyKeyHeader, o.idempotencyKey)
	resp, err := s.t.retryUnavailable(ctx, hreq, s.t.do)
	if err != nil {
		return nil, err
	}
	return decodeBuild(resp)
}

// GetBuildOptions tunes Get.
type GetBuildOptions struct {
	// Wait attaches until the build finishes, for at most this long
	// (capped by the server). Zero answers at once.
	Wait time.Duration
}

// Get reports a build, finished or still running.
func (s *BuildsService) Get(ctx context.Context, id string, opt GetBuildOptions) (*Build, error) {
	if s == nil || s.t == nil {
		return nil, errors.New("builds service is not configured")
	}
	if id == "" {
		return nil, errors.New("build id is required")
	}
	var q url.Values
	if opt.Wait > 0 {
		q = url.Values{"wait": {opt.Wait.String()}}
	}
	req, err := s.t.newRequest(ctx, http.MethodGet, "/v1/builds/"+url.PathEscape(id), q, nil)
	if err != nil {
		return nil, err
	}
	// the no-timeout client: a wait can run as long as the client's own
	// timeout. ctx bounds it.
	resp, err := s.t.doStream(req)
	if err != nil {
		return nil, err
	}
	return decodeBuild(resp)
}

// Wait blocks until the build finishes and returns it. ctx bounds the wait;
// the build keeps running if ctx ends first. A failed build is returned, not
// an error: its Error and Output say why.
func (s *BuildsService) Wait(ctx context.Context, id string) (*Build, error) {
	for {
		b, err := s.Get(ctx, id, GetBuildOptions{Wait: operationPollWait})
		if err != nil {
			return nil, err
		}
		if b.Done() {
			return b, nil
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
}

func decodeBuild(resp *http.Response) (*Build, error) {
	if err := CheckResponse(resp); err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	var b Build
	if err := json.NewDecoder(resp.Body).Decode(&b); err != nil {
		return nil, fmt.Errorf("decode build: %w", err)
	}
	return &b, nil
}

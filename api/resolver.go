package api

import (
	"encoding/base64"
	"fmt"

	"github.com/folsomintel/fuse/internal/core"
)

// defaultManifest is the manifest used when a caller omits an inline manifest.
// It is fused-profile data (the fused manifest schema), not a generic core
// default: the Resolver treats manifest bytes as opaque, so the default it
// falls back to belongs to the configured agent profile. It is sourced from
// the orchestrator fused profile (orchestrator.DefaultFusedManifest) so the
// schema lives in exactly one place.
var defaultManifest = orchestrator.DefaultFusedManifest

// Resolver turns a CreateEnvironmentRequest into raw manifest bytes.
// The default implementation understands inline base64 only; a future
// revision can plug in out-of-band ref resolution without touching
// handler code.
type Resolver interface {
	Resolve(req CreateEnvironmentRequest) ([]byte, error)
}

// InlineResolver decodes the ManifestInline field as standard base64.
// It rejects empty inputs and any leading/trailing whitespace via the
// stdlib decoder's strictness.
type InlineResolver struct{}

// Resolve implements Resolver.
func (InlineResolver) Resolve(req CreateEnvironmentRequest) ([]byte, error) {
	if req.ManifestInline == "" {
		return append([]byte(nil), defaultManifest...), nil
	}
	data, err := base64.StdEncoding.DecodeString(req.ManifestInline)
	if err != nil {
		return nil, fmt.Errorf("decode manifest_inline: %w", err)
	}
	return data, nil
}

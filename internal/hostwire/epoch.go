package hostwire

import (
	"net/http"
	"strconv"

	"github.com/folsomintel/fuse/internal/orchestrator"
)

// EpochHeader carries the incarnation a mutating host-agent request is for,
// as orchestrator.WithVMEpoch set it. agentlib.EPOCH_HEADER is the agents'
// side of it.
const EpochHeader = "X-Fuse-Epoch"

// epochTransport sets EpochHeader on mutating requests made under a ctx
// that carries an epoch. reads are never fenced, so they go out unchanged.
type epochTransport struct {
	Base http.RoundTripper
}

func (t *epochTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	epoch := orchestrator.VMEpochFrom(req.Context())
	if epoch == 0 || !mutating(req.Method) || req.Header.Get(EpochHeader) != "" {
		return t.Base.RoundTrip(req)
	}
	// a RoundTripper must not modify the caller's request.
	req = req.Clone(req.Context())
	req.Header.Set(EpochHeader, strconv.FormatInt(epoch, 10))
	return t.Base.RoundTrip(req)
}

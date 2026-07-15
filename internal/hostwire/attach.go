package hostwire

import (
	"net/url"
	"strconv"

	"github.com/folsomintel/fuse/internal/orchestrator"
)

// AttachProto is the value of the Upgrade header that opens an attach stream.
// It is spoken on both hops — client to orchestrator, and orchestrator to host
// agent — so the orchestrator can relay bytes without reframing them.
const AttachProto = "fuse-attach/1"

// AttachQuery encodes an AttachSpec as the query string of an attach request.
// The spec rides in the URL rather than a body because the upgrade is a GET:
// there is no body to put it in, and inventing a pre-upgrade handshake frame
// would buy nothing.
func AttachQuery(spec orchestrator.AttachSpec) url.Values {
	q := url.Values{}
	if spec.TTY {
		q.Set("tty", "1")
	}
	if spec.Rows > 0 {
		q.Set("rows", strconv.Itoa(int(spec.Rows)))
	}
	if spec.Cols > 0 {
		q.Set("cols", strconv.Itoa(int(spec.Cols)))
	}
	// Repeated cmd params preserve argv boundaries, so a command containing
	// spaces survives the round trip without a quoting convention.
	for _, arg := range spec.Cmd {
		q.Add("cmd", arg)
	}
	return q
}

// ParseAttachQuery is the inverse of AttachQuery, used by the orchestrator to
// read a client's attach request before relaying it onward.
func ParseAttachQuery(q url.Values) orchestrator.AttachSpec {
	spec := orchestrator.AttachSpec{
		Cmd: q["cmd"],
		TTY: q.Get("tty") == "1" || q.Get("tty") == "true",
	}
	if n, err := strconv.Atoi(q.Get("rows")); err == nil && n > 0 && n <= 0xffff {
		spec.Rows = uint16(n)
	}
	if n, err := strconv.Atoi(q.Get("cols")); err == nil && n > 0 && n <= 0xffff {
		spec.Cols = uint16(n)
	}
	return spec
}

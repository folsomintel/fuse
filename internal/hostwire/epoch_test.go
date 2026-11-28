package hostwire

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/folsomintel/fuse/internal/orchestrator"
)

func TestEpochHeaderOnMutatingRequests(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Method+" "+r.Header.Get(EpochHeader))
	}))
	defer srv.Close()
	c := NewClient()

	do := func(ctx context.Context, method, header string) {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, method, srv.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		if header != "" {
			req.Header.Set(EpochHeader, header)
		}
		res, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
	}
	fenced := orchestrator.WithVMEpoch(context.Background(), 1764000000123)
	do(fenced, http.MethodPost, "")
	do(fenced, http.MethodDelete, "")
	do(fenced, http.MethodGet, "")
	do(context.Background(), http.MethodPost, "")
	// a header the caller set wins over ctx.
	do(fenced, http.MethodPost, "7")

	want := []string{
		"POST 1764000000123",
		"DELETE 1764000000123",
		"GET ",
		"POST ",
		"POST 7",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("requests:\n got %q\nwant %q", got, want)
	}
}

// the agents must read the header this package sends, and accept every
// epoch orchestrator.VMEpoch makes.
func TestAgentsShareEpochHeader(t *testing.T) {
	b, err := os.ReadFile("../../host-agent/shared/agentlib.py")
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{
		`EPOCH_HEADER = "` + EpochHeader + `"`,
		`EPOCH_RE = re.compile(r"[1-9][0-9]{0,17}")`,
	} {
		if !strings.Contains(string(b), w) {
			t.Errorf("agentlib.py: want %s", w)
		}
	}
}

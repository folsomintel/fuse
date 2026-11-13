package telemetry

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
)

func TestSetupOffWithoutEndpoint(t *testing.T) {
	shutdown, enabled, err := Setup(context.Background(), func(string) string { return "" }, "svc")
	if err != nil || enabled {
		t.Fatalf("Setup with no endpoint: enabled=%v err=%v", enabled, err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	// spans stay noops: nothing is recorded or exported.
	_, span := Tracer().Start(context.Background(), "x")
	if span.SpanContext().IsValid() {
		t.Error("tracing off still produced a valid span")
	}
	if otel.GetTextMapPropagator().Fields() == nil {
		t.Error("propagator not installed")
	}
}

func TestLogHandlerAddsRequestID(t *testing.T) {
	var buf bytes.Buffer
	l := slog.New(NewLogHandler(slog.NewTextHandler(&buf, nil))).With("k", "v")
	l.InfoContext(WithRequestID(context.Background(), "req_log"), "hello")
	l.Info("no ctx")
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if !strings.Contains(lines[0], "request_id=req_log") || !strings.Contains(lines[0], "k=v") {
		t.Errorf("first line %q lacks request_id or the With attr", lines[0])
	}
	if strings.Contains(lines[1], "request_id") {
		t.Errorf("line without a ctx carries a request id: %q", lines[1])
	}
}

func TestWithRequestIDEmptyKeepsCtx(t *testing.T) {
	ctx := WithRequestID(context.Background(), "req_a")
	if got := RequestID(WithRequestID(ctx, "")); got != "req_a" {
		t.Errorf("empty id replaced the existing one: %q", got)
	}
}

// TestValidRequestID exercises the validator's edge cases directly so
// future tweaks to the character class are caught immediately.
func TestValidRequestID(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"", false},
		{"abc", true},
		{"abc-123_XYZ", true},
		{"req_0123456789abcdef", true},
		{"has space", false},
		{"has.dot", false},
		{"has/slash", false},
		{"has:colon", false},
		{"new\nline", false},
		{"tail\n", false},
		{strings.Repeat("a", MaxRequestIDLen), true},
		{strings.Repeat("a", MaxRequestIDLen+1), false},
	}
	for _, tc := range cases {
		if got := ValidRequestID(tc.in); got != tc.want {
			t.Errorf("ValidRequestID(%q): got %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestAgentsShareRequestIDRule keeps the host agents' REQUEST_ID_RE on the
// same rule as RequestIDPattern, so a change to one cannot drift from the
// other two.
func TestAgentsShareRequestIDRule(t *testing.T) {
	want := `REQUEST_ID_RE = re.compile(r"` + RequestIDPattern + `")`
	for _, f := range []string{
		"../../host-agent/firecracker/fc-agent.py",
		"../../host-agent/qemu/qemu-agent.py",
	} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), want) {
			t.Errorf("%s: want %s", f, want)
		}
	}
}

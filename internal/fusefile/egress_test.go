package fusefile

import (
	"reflect"
	"strings"
	"testing"
)

// parseEgress parses a Fusefile whose only interesting field is the egress
// block, so each case below is just the yaml under test.
func parseEgress(t *testing.T, body string) (*Fusefile, error) {
	t.Helper()
	return Parse([]byte("version: 1\n" + body))
}

func TestParseEgressProxy(t *testing.T) {
	f, err := parseEgress(t, `egress:
  mode: proxy
  provider: mock
  protocol: http
`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := &Egress{Mode: EgressModeProxy, Provider: "mock", Protocol: EgressProtocolHTTP}
	if !reflect.DeepEqual(f.Egress, want) {
		t.Errorf("egress = %+v, want %+v", f.Egress, want)
	}
}

// parse leaves an omitted protocol empty: the socks5 default is the
// compiler's, so `fuse validate` and `fuse compile` see the same block.
func TestParseEgressProxyOmittedProtocol(t *testing.T) {
	f, err := parseEgress(t, "egress:\n  mode: proxy\n  provider: mock\n")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if f.Egress.Protocol != "" {
		t.Errorf("protocol = %q, want empty before compile", f.Egress.Protocol)
	}
}

// an absent block must stay absent: nil is what tells every layer below that
// the author asked for nothing, and a zero-valued struct would not.
func TestParseEgressAbsent(t *testing.T) {
	f, err := parseEgress(t, "run: ./serve.sh\n")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if f.Egress != nil {
		t.Errorf("egress = %+v, want nil", f.Egress)
	}
}

func TestValidateEgressRejects(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "unknown mode",
			body: "egress:\n  mode: prox\n",
			want: `egress.mode: must be "direct" or "proxy", got "prox"`,
		},
		{
			name: "mode is case sensitive",
			body: "egress:\n  mode: Proxy\n  provider: mock\n",
			want: `egress.mode: must be "direct" or "proxy", got "Proxy"`,
		},
		{
			name: "direct with a provider",
			body: "egress:\n  mode: direct\n  provider: mock\n",
			want: `egress.provider: "mock" requires mode "proxy"`,
		},
		{
			name: "direct with a protocol",
			body: "egress:\n  mode: direct\n  protocol: socks5\n",
			want: `egress.protocol: "socks5" requires mode "proxy"`,
		},
		{
			name: "empty mode reads as direct",
			body: "egress:\n  provider: mock\n",
			want: `egress.provider: "mock" requires mode "proxy"`,
		},
		{
			name: "proxy without a provider",
			body: "egress:\n  mode: proxy\n",
			want: `egress.provider: is required when mode is "proxy"`,
		},
		{
			name: "unknown protocol",
			body: "egress:\n  mode: proxy\n  provider: mock\n  protocol: socks4\n",
			want: `egress.protocol: must be "socks5" or "http", got "socks4"`,
		},
		{
			name: "unknown field",
			body: "egress:\n  mode: proxy\n  provider: mock\n  region: eu\n",
			want: "field region not found",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseEgress(t, tc.body)
			if err == nil {
				t.Fatalf("Parse accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

// TestCompileEgress pins where the defaults are applied. They land in
// compileEgress rather than in Parse for the reason
// TestCompileExposeProtocolDefault gives: Decode and Validate are separable,
// so defaulting upstream of Compile would leave `fuse up` and `fuse compile`
// emitting different wires for identical input.
func TestCompileEgress(t *testing.T) {
	cases := []struct {
		name string
		body string
		want *EgressSpec
	}{
		{
			name: "proxy defaults the protocol to socks5",
			body: "egress:\n  mode: proxy\n  provider: mock\n",
			want: &EgressSpec{Mode: EgressModeProxy, Provider: "mock", Protocol: EgressProtocolSOCKS5},
		},
		{
			name: "an explicit protocol is carried through",
			body: "egress:\n  mode: proxy\n  provider: mock\n  protocol: http\n",
			want: &EgressSpec{Mode: EgressModeProxy, Provider: "mock", Protocol: EgressProtocolHTTP},
		},
		{
			name: "an explicit direct block compiles to direct alone",
			body: "egress:\n  mode: direct\n",
			want: &EgressSpec{Mode: EgressModeDirect},
		},
		{
			name: "an empty block compiles to direct",
			body: "egress: {}\n",
			want: &EgressSpec{Mode: EgressModeDirect},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, err := parseEgress(t, tc.body)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			c, err := Compile(f)
			if err != nil {
				t.Fatalf("Compile: %v", err)
			}
			if !reflect.DeepEqual(c.Egress, tc.want) {
				t.Errorf("egress = %+v, want %+v", c.Egress, tc.want)
			}
		})
	}
}

// a Fusefile with no egress block must compile to a nil spec, so nothing
// downstream has to tell "no block" apart from "an empty block".
func TestCompileEgressAbsent(t *testing.T) {
	f, err := parseEgress(t, "run: ./serve.sh\n")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	c, err := Compile(f)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if c.Egress != nil {
		t.Errorf("egress = %+v, want nil", c.Egress)
	}
}

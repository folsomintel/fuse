package providers

import (
	"testing"

	"github.com/surf-dev/surf/apps/orchestrator/firecracker"
)

func TestNew_default_firecracker(t *testing.T) {
	p, err := New(Config{
		Kind:        "",
		Firecracker: firecracker.Config{UseStub: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if p == nil {
		t.Fatal("expected non-nil provider")
	}
}

func TestNew_explicit_firecracker(t *testing.T) {
	p, err := New(Config{
		Kind:        Firecracker,
		Firecracker: firecracker.Config{UseStub: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if p == nil {
		t.Fatal("expected non-nil provider")
	}
}

func TestNew_mock_returns_error(t *testing.T) {
	_, err := New(Config{Kind: Mock})
	if err == nil {
		t.Fatal("expected error for mock provider")
	}
}

func TestNew_unknown_kind(t *testing.T) {
	_, err := New(Config{Kind: "unknown"})
	if err == nil {
		t.Fatal("expected error for unknown kind")
	}
}

func TestKind_constants(t *testing.T) {
	if Firecracker != "firecracker" {
		t.Fatal("Firecracker constant mismatch")
	}
	if Mock != "mock" {
		t.Fatal("Mock constant mismatch")
	}
}

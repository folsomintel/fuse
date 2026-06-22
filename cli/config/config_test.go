package config

import (
	"os"
	"path/filepath"
	"testing"
)

func tmpConfig(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "config.yaml")
}

func TestSaveLoadRoundtrip(t *testing.T) {
	path := tmpConfig(t)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load empty: %v", err)
	}
	cfg.Add(Context{Name: "prod", BaseURL: "http://prod:8080", Token: "tok", Master: true})
	if err := cfg.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}

	got, err := Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got.CurrentContext != "prod" {
		t.Errorf("current = %q, want prod", got.CurrentContext)
	}
	cur, err := got.Current("")
	if err != nil {
		t.Fatalf("current: %v", err)
	}
	if cur.BaseURL != "http://prod:8080" || cur.Token != "tok" || !cur.Master {
		t.Errorf("roundtrip mismatch: %+v", cur)
	}
}

func TestSavePerms(t *testing.T) {
	path := tmpConfig(t)
	cfg, _ := Load(path)
	cfg.Add(Context{Name: "a", BaseURL: "http://a"})
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("perm = %o, want 600", perm)
	}
}

func TestMissingFileIsEmpty(t *testing.T) {
	cfg, err := Load(tmpConfig(t))
	if err != nil {
		t.Fatalf("load missing: %v", err)
	}
	if _, err := cfg.Current(""); err != ErrNoContext {
		t.Errorf("want ErrNoContext, got %v", err)
	}
}

func TestAddPreservesActiveHost(t *testing.T) {
	cfg, _ := Load(tmpConfig(t))
	cfg.Add(Context{Name: "a", BaseURL: "http://a"})
	if err := cfg.SetActiveHost("", "host-1"); err != nil {
		t.Fatal(err)
	}
	// re-connect to the same context without an active host: it must survive.
	cfg.Add(Context{Name: "a", BaseURL: "http://a", Token: "new"})
	cur, _ := cfg.Current("")
	if cur.ActiveHost != "host-1" {
		t.Errorf("active host = %q, want host-1", cur.ActiveHost)
	}
	if cur.Token != "new" {
		t.Errorf("token = %q, want new", cur.Token)
	}
}

func TestUseUnknown(t *testing.T) {
	cfg, _ := Load(tmpConfig(t))
	if err := cfg.Use("nope"); err == nil {
		t.Error("expected error using unknown context")
	}
}

func TestRemoveClearsCurrent(t *testing.T) {
	cfg, _ := Load(tmpConfig(t))
	cfg.Add(Context{Name: "a", BaseURL: "http://a"})
	if err := cfg.Remove("a"); err != nil {
		t.Fatal(err)
	}
	if cfg.CurrentContext != "" {
		t.Errorf("current = %q, want empty after removing it", cfg.CurrentContext)
	}
	if _, err := cfg.Current(""); err != ErrNoContext {
		t.Errorf("want ErrNoContext, got %v", err)
	}
}

func TestCurrentOverride(t *testing.T) {
	cfg, _ := Load(tmpConfig(t))
	cfg.Add(Context{Name: "a", BaseURL: "http://a"})
	cfg.Add(Context{Name: "b", BaseURL: "http://b"}) // b is now current
	cur, err := cfg.Current("a")
	if err != nil {
		t.Fatal(err)
	}
	if cur.Name != "a" {
		t.Errorf("override = %q, want a", cur.Name)
	}
}

package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/folsomintel/fuse/internal/secrets"
)

func writeCreds(t *testing.T, dir string, creds *secrets.VMCredentials) (string, string, string) {
	t.Helper()
	token, cert, key := filepath.Join(dir, "auth-token"), filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	for path, data := range map[string][]byte{token: []byte(creds.AuthToken + "\n"), cert: creds.CertPEM, key: creds.KeyPEM} {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return token, cert, key
}

func TestCredentialsReloadSwapsTokenAndCert(t *testing.T) {
	dir := t.TempDir()
	first, err := secrets.GenerateVMCredentials("fuse-1")
	if err != nil {
		t.Fatal(err)
	}
	tokenFile, certFile, keyFile := writeCreds(t, dir, first)
	c, err := loadCredentials(tokenFile, certFile, keyFile, true)
	if err != nil {
		t.Fatal(err)
	}
	if c.authToken() != first.AuthToken {
		t.Fatalf("token = %q, want %q", c.authToken(), first.AuthToken)
	}
	oldCert, _ := c.getCertificate(nil)

	second, err := secrets.GenerateVMCredentials("fuse-2")
	if err != nil {
		t.Fatal(err)
	}
	writeCreds(t, dir, second)
	if err := c.reload(); err != nil {
		t.Fatal(err)
	}
	if c.authToken() != second.AuthToken {
		t.Fatalf("token after reload = %q, want %q", c.authToken(), second.AuthToken)
	}
	newCert, _ := c.getCertificate(nil)
	if bytes.Equal(oldCert.Certificate[0], newCert.Certificate[0]) {
		t.Fatal("certificate not swapped on reload")
	}
}

func TestCredentialsReloadKeepsOldOnBadFiles(t *testing.T) {
	dir := t.TempDir()
	creds, err := secrets.GenerateVMCredentials("fuse-1")
	if err != nil {
		t.Fatal(err)
	}
	tokenFile, certFile, keyFile := writeCreds(t, dir, creds)
	c, err := loadCredentials(tokenFile, certFile, keyFile, true)
	if err != nil {
		t.Fatal(err)
	}

	// an empty token file must not drop the agent to unauthenticated.
	if err := os.WriteFile(tokenFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.reload(); err == nil {
		t.Fatal("reload with an empty token file succeeded")
	}
	if c.authToken() != creds.AuthToken {
		t.Fatalf("token = %q after a failed reload, want the old one", c.authToken())
	}

	// a mismatched key pair keeps the old certificate.
	writeCreds(t, dir, creds)
	if err := os.WriteFile(keyFile, []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.reload(); err == nil {
		t.Fatal("reload with a bad key succeeded")
	}
	if cert, _ := c.getCertificate(nil); cert == nil {
		t.Fatal("certificate dropped on a failed reload")
	}

	// a new token beside a broken key is not taken either: the old token and
	// certificate stay together.
	if err := os.WriteFile(tokenFile, []byte("new-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.reload(); err == nil {
		t.Fatal("reload with a new token and a bad key succeeded")
	}
	if c.authToken() != creds.AuthToken {
		t.Fatalf("token = %q after a reload that failed on the key, want the old one", c.authToken())
	}
}

func TestProtectReadsTokenPerRequest(t *testing.T) {
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "auth-token")
	if err := os.WriteFile(tokenFile, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := loadCredentials(tokenFile, "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	h := protect(c, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	call := func(token string) int {
		r := httptest.NewRequest(http.MethodGet, "/v1/info", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		h(w, r)
		return w.Code
	}
	if got := call("old"); got != http.StatusOK {
		t.Fatalf("old token before reload = %d", got)
	}
	if err := os.WriteFile(tokenFile, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.reload(); err != nil {
		t.Fatal(err)
	}
	if got := call("old"); got != http.StatusUnauthorized {
		t.Fatalf("old token after reload = %d, want 401", got)
	}
	if got := call("new"); got != http.StatusOK {
		t.Fatalf("new token after reload = %d", got)
	}
}

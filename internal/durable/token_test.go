package durable

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/folsomintel/fuse/internal/orchestrator"
	"github.com/folsomintel/fuse/internal/secrets"
)

var tokenTestKey = bytes.Repeat([]byte{7}, 32)

// tokenGuest is the state a rotation changes, shared by the fleets of an
// endpoint and its restart: the token in the guest's files and the one the
// state store holds.
type tokenGuest struct {
	mu        sync.Mutex
	inGuest   string
	stored    string
	generated []string
	sealed    [][]byte
}

func (g *tokenGuest) tokens() (inGuest, stored string, generated []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.inGuest, g.stored, append([]string(nil), g.generated...)
}

// tokenFleet scripts the rotation steps over a tokenGuest, with real
// encryption so the journal carries what the fleet's steps really return.
type tokenFleet struct {
	*snapFleet
	guest *tokenGuest
}

func newTokenFleet(g *tokenGuest) *tokenFleet {
	return &tokenFleet{snapFleet: newSnapFleet(), guest: g}
}

func (f *tokenFleet) RotateTokenGenerate(ctx context.Context, vm string) (orchestrator.RotatedCredentials, error) {
	if err := f.enter(ctx, "generate"); err != nil {
		return orchestrator.RotatedCredentials{}, err
	}
	creds, err := secrets.GenerateVMCredentials(vm)
	if err != nil {
		return orchestrator.RotatedCredentials{}, err
	}
	sealed, err := secrets.EncryptToken(creds.AuthToken, tokenTestKey)
	if err != nil {
		return orchestrator.RotatedCredentials{}, err
	}
	f.guest.mu.Lock()
	f.guest.generated = append(f.guest.generated, creds.AuthToken)
	f.guest.sealed = append(f.guest.sealed, sealed)
	f.guest.mu.Unlock()
	return orchestrator.RotatedCredentials{Sealed: sealed, TokenEncrypted: sealed}, nil
}

func (f *tokenFleet) RotateTokenUpload(ctx context.Context, _ string, r orchestrator.RotatedCredentials) error {
	if err := f.enter(ctx, "upload"); err != nil {
		return err
	}
	token, err := secrets.DecryptToken(r.Sealed, tokenTestKey)
	if err != nil {
		return err
	}
	f.guest.mu.Lock()
	f.guest.inGuest = token
	f.guest.mu.Unlock()
	return nil
}

func (f *tokenFleet) RotateTokenPersist(ctx context.Context, _ string, r orchestrator.RotatedCredentials) error {
	if err := f.enter(ctx, "persist"); err != nil {
		return err
	}
	token, err := secrets.DecryptToken(r.TokenEncrypted, tokenTestKey)
	if err != nil {
		return err
	}
	f.guest.mu.Lock()
	f.guest.stored = token
	f.guest.mu.Unlock()
	return nil
}

// TestRotateTokenThroughRestate: an upload that fails twice is retried inside
// its step, generate runs once, and the stored token is the guest's.
func TestRotateTokenThroughRestate(t *testing.T) {
	guest := &tokenGuest{}
	fleet := newTokenFleet(guest)
	fleet.fail["upload"] = []error{errors.New("agent 503"), errors.New("agent 503")}
	client := serveFleet(t, fleet, fastPolicy)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := client.RotateToken(ctx, uniqueVM("rotate")); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if got, want := fleet.steps(), "generate,upload,upload,upload,persist"; got != want {
		t.Fatalf("steps %s, want %s", got, want)
	}
	inGuest, stored, generated := guest.tokens()
	if len(generated) != 1 || inGuest != generated[0] || stored != generated[0] {
		t.Fatalf("guest %q, stored %q, generated %v; want one token in both", inGuest, stored, generated)
	}
}

// TestRotateTokenPersistOutlastsThePolicy: a state store down for longer than
// the policy's attempts holds the rotation at the persist until it is back,
// rather than ending it with the guest's token never stored.
func TestRotateTokenPersistOutlastsThePolicy(t *testing.T) {
	guest := &tokenGuest{}
	fleet := newTokenFleet(guest)
	var downs []error
	for range fastPolicy.MaxAttempts + 3 {
		downs = append(downs, errors.New("store down"))
	}
	fleet.fail["persist"] = downs
	client := serveFleet(t, fleet, fastPolicy)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := client.RotateToken(ctx, uniqueVM("rotate-store-down")); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	want := "generate,upload," + strings.Repeat("persist,", len(downs)+1)
	if got := fleet.steps(); got != strings.TrimSuffix(want, ",") {
		t.Fatalf("steps %s, want %s", got, want)
	}
	inGuest, stored, _ := guest.tokens()
	if inGuest == "" || stored != inGuest {
		t.Fatalf("guest %q, stored %q; want the guest's token stored", inGuest, stored)
	}
}

// TestRotateTokenRefusals: a refusal crosses restate with the fleet's own
// message, and a vm the fleet does not know keeps its sentinel.
func TestRotateTokenRefusals(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	notRunning := fmt.Errorf("vm x in state stopped: %w", orchestrator.ErrRotateNotRunning)
	fleet := newTokenFleet(&tokenGuest{})
	fleet.fail["generate"] = []error{notRunning}
	client := serveFleet(t, fleet, fastPolicy)
	err := client.RotateToken(ctx, uniqueVM("rotate-refused"))
	if err == nil || err.Error() != notRunning.Error() {
		t.Fatalf("rotate error %v, want %q", err, notRunning)
	}
	if got := fleet.steps(); got != "generate" {
		t.Fatalf("steps %s, want only generate", got)
	}

	gone := newTokenFleet(&tokenGuest{})
	gone.fail["upload"] = []error{fmt.Errorf("%w: fuse-gone", orchestrator.ErrVMNotFound)}
	client = serveFleet(t, gone, fastPolicy)
	if err := client.RotateToken(ctx, uniqueVM("rotate-gone")); !errors.Is(err, orchestrator.ErrVMNotFound) {
		t.Fatalf("rotate error %v, want ErrVMNotFound", err)
	}
	if got := gone.steps(); got != "generate,upload" {
		t.Fatalf("steps %s, want generate,upload", got)
	}
}

// TestRotateTokenSurvivesEndpointRestart stands in for kill -9 of the
// orchestrator between the upload and the persist: the endpoint drops while
// the persist runs, and the restarted one persists the token already in the
// guest instead of generating another. while the invocation is held there,
// its journal is read through the admin api: it holds the generate step's
// ciphertext and nowhere the plaintext token.
func TestRotateTokenSurvivesEndpointRestart(t *testing.T) {
	guest := &tokenGuest{}
	first := newTokenFleet(guest)
	first.block["persist"] = make(chan struct{})
	defer close(first.block["persist"])
	client, addr, stop := serveFleetAt(t, first, "")
	vm := uniqueVM("rotate-restart")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- client.RotateToken(ctx, vm) }()
	<-first.entered

	_, _, generated := guest.tokens()
	if len(generated) != 1 {
		t.Fatalf("generated %v before the persist, want one token", generated)
	}
	guest.mu.Lock()
	sealed := guest.sealed[0]
	guest.mu.Unlock()
	journal := readJournal(t, ctx, vm)
	if !bytes.Contains(journal, []byte(base64.StdEncoding.EncodeToString(sealed))) {
		t.Fatalf("journal of %s does not hold the generate step's ciphertext; the check below would be vacuous", vm)
	}
	for _, leak := range [][]byte{
		[]byte(generated[0]),
		[]byte(base64.StdEncoding.EncodeToString([]byte(generated[0]))),
	} {
		if bytes.Contains(journal, leak) {
			t.Fatalf("journal of %s holds the plaintext token", vm)
		}
	}

	stop()
	second := newTokenFleet(guest)
	_, _, _ = serveFleetAt(t, second, addr)
	if err := <-done; err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if got := second.steps(); got != "persist" {
		t.Fatalf("steps after the restart %s, want only persist", got)
	}
	inGuest, stored, generated := guest.tokens()
	if len(generated) != 1 || inGuest != generated[0] || stored != inGuest {
		t.Fatalf("guest %q, stored %q, generated %v; want the first token in both", inGuest, stored, generated)
	}
}

// readJournal returns the journal entries of vm's in-flight invocations from
// restate's sql introspection, with every payload (rendered as an array of
// bytes) decoded back into its bytes.
func readJournal(t *testing.T, ctx context.Context, vm string) []byte {
	t.Helper()
	admin, _ := restateTestURLs(t)
	query := fmt.Sprintf("SELECT j.name, j.entry_json FROM sys_journal j JOIN sys_invocation i ON j.id = i.id "+
		"WHERE i.target_service_name = '%s' AND i.target_service_key = '%s'", EnvironmentObject, vm)
	body, _ := json.Marshal(map[string]string{"query": query})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, admin+"/query", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("journal query: http %d: %s", res.StatusCode, raw)
	}
	var out struct {
		Rows []struct {
			Name      string `json:"name"`
			EntryJSON string `json:"entry_json"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("journal query: %v: %s", err, raw)
	}
	var names []string
	all := append([]byte(nil), raw...)
	for _, r := range out.Rows {
		names = append(names, r.Name)
		var entry any
		if err := json.Unmarshal([]byte(r.EntryJSON), &entry); err != nil {
			t.Fatalf("journal entry %q: %v", r.EntryJSON, err)
		}
		all = appendPayloads(all, entry)
	}
	if !strings.Contains(strings.Join(names, ","), "generate") {
		t.Fatalf("journal of %s has entries %v, want the generate step", vm, names)
	}
	return all
}

// appendPayloads appends every array of numbers under v as the bytes it
// stands for.
func appendPayloads(out []byte, v any) []byte {
	switch v := v.(type) {
	case map[string]any:
		for _, x := range v {
			out = appendPayloads(out, x)
		}
	case []any:
		b := make([]byte, 0, len(v))
		for _, x := range v {
			f, ok := x.(float64)
			if !ok {
				// not a payload: a list of entries or headers.
				for _, y := range v {
					out = appendPayloads(out, y)
				}
				return out
			}
			b = append(b, byte(f))
		}
		out = append(out, ' ')
		out = append(out, b...)
	}
	return out
}

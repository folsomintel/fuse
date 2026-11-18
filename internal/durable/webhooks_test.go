package durable

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/folsomintel/fuse/internal/hitl"
	"github.com/folsomintel/fuse/internal/orchestrator"
	"github.com/folsomintel/fuse/internal/secrets"
)

func TestWebhookBackoff(t *testing.T) {
	want := []time.Duration{time.Second, 4 * time.Second, 16 * time.Second, 64 * time.Second,
		256 * time.Second, 1024 * time.Second, time.Hour, time.Hour}
	for i, w := range want {
		if got := webhookBackoff(i + 1); got != w {
			t.Errorf("backoff after attempt %d = %s, want %s", i+1, got, w)
		}
	}
}

// shortWebhookSchedule shrinks the retry schedule for one test.
func shortWebhookSchedule(t *testing.T, first, max, window time.Duration) {
	t.Helper()
	f, m, w := webhookFirstBackoff, webhookMaxBackoff, webhookWindow
	webhookFirstBackoff, webhookMaxBackoff, webhookWindow = first, max, window
	t.Cleanup(func() { webhookFirstBackoff, webhookMaxBackoff, webhookWindow = f, m, w })
}

// hookFleet runs the webhook steps on a real FleetManager, so the post
// unseals the endpoint and signs the body the way the orchestrator does. it
// records the attempts of the one delivery the test sent; a delivery left
// over from an earlier run is ignored.
type hookFleet struct {
	fakeFleet
	fm *orchestrator.FleetManager
	id string

	mu       sync.Mutex
	attempts []int
	results  []orchestrator.WebhookAttempt
	dead     []string
	posted   chan struct{}
	done     chan struct{}
}

func newHookFleet(id string) *hookFleet {
	return &hookFleet{
		fm:     orchestrator.NewFleetManager(orchestrator.FleetConfig{Prefix: "fuse-", TokenEncryptionKey: tokenTestKey}),
		id:     id,
		posted: make(chan struct{}, 64),
		done:   make(chan struct{}),
	}
}

func (f *hookFleet) WebhookPost(ctx context.Context, d orchestrator.WebhookDelivery) (orchestrator.WebhookAttempt, error) {
	if d.ID != f.id {
		return orchestrator.WebhookAttempt{Delivered: true}, nil
	}
	res, err := f.fm.WebhookPost(ctx, d)
	f.mu.Lock()
	f.attempts = append(f.attempts, d.Attempt)
	f.results = append(f.results, res)
	f.mu.Unlock()
	f.posted <- struct{}{}
	if res.Delivered {
		close(f.done)
	}
	return res, err
}

func (f *hookFleet) WebhookDeadLetter(ctx context.Context, d orchestrator.WebhookDelivery, lastErr string) error {
	if d.ID != f.id {
		return nil
	}
	f.mu.Lock()
	f.attempts = append(f.attempts, -d.Attempt)
	f.dead = append(f.dead, lastErr)
	f.mu.Unlock()
	close(f.done)
	return nil
}

func (f *hookFleet) seen() ([]int, []orchestrator.WebhookAttempt, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.attempts...), append([]orchestrator.WebhookAttempt(nil), f.results...), append([]string(nil), f.dead...)
}

// testDelivery is the first attempt at a raised notification to hookURL,
// sealed the way the orchestrator seals it.
func testDelivery(t *testing.T, hookURL string) orchestrator.WebhookDelivery {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"url": hookURL, "secret": []byte("whsec")})
	sealed, err := secrets.EncryptBytes(raw, tokenTestKey)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Minute).UTC()
	return orchestrator.WebhookDelivery{
		ID: fmt.Sprintf("dlv-%d", time.Now().UnixNano()),
		Notification: hitl.Notification{
			Event: hitl.AuditRaised, InterventionID: "int_abc", EnvID: "fuse-hook",
			Gate: "deploy", On: "ask", Deadline: &deadline,
			URL: "https://fuse.example.com/v1/interventions/int_abc",
		},
		Endpoint: "receiver",
		Target:   sealed,
		Attempt:  1,
		FirstAt:  time.Now().UTC(),
	}
}

// hookReceiver keeps the signed bodies posted to it.
type hookReceiver struct {
	mu  sync.Mutex
	got []delivered
}

type delivered struct {
	sig  string
	body []byte
}

func (r *hookReceiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	r.got = append(r.got, delivered{sig: req.Header.Get(hitl.HeaderSignature), body: body})
	r.mu.Unlock()
}

// freeAddr is a loopback address nothing listens on.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

func waitDone(t *testing.T, done chan struct{}, d time.Duration) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatal("delivery never finished")
	}
}

// TestWebhookSurvivesEndpointRestart is the acceptance run: the receiver is
// down, the handler endpoint is killed between attempts and a new one takes
// its place, the receiver comes back, and the webhook is delivered once,
// signed. every failed attempt ended its invocation and sent the next one
// delayed, so the restarted endpoint picks the chain up. the journal of the
// chain holds the delivery's ids and the sealed endpoint and nowhere the
// url's token or the signing secret.
func TestWebhookSurvivesEndpointRestart(t *testing.T) {
	shortWebhookSchedule(t, 300*time.Millisecond, 300*time.Millisecond, time.Minute)
	recvAddr := freeAddr(t)
	hookURL := "http://" + recvAddr + "/hooks/tok_pathsecret?key=querysecret"
	d := testDelivery(t, hookURL)

	first := newHookFleet(d.ID)
	client, addr, stop := serveFleetAt(t, first, "")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := client.SendWebhook(ctx, d); err != nil {
		t.Fatalf("send: %v", err)
	}
	<-first.posted
	stop()

	second := newHookFleet(d.ID)
	_, _, _ = serveFleetAt(t, second, addr)
	<-second.posted

	recv := &hookReceiver{}
	ln, err := net.Listen("tcp", recvAddr)
	if err != nil {
		t.Fatal(err)
	}
	srv := &httptest.Server{Listener: ln, Config: &http.Server{Handler: recv}}
	srv.Start()
	defer srv.Close()
	waitDone(t, second.done, 20*time.Second)

	firstAttempts, firstResults, _ := first.seen()
	attempts, results, dead := second.seen()
	if len(dead) != 0 {
		t.Fatalf("dead-lettered %v, want delivered", dead)
	}
	if len(firstAttempts) == 0 || firstResults[0].Delivered || firstResults[0].Status != 0 {
		t.Fatalf("first endpoint attempts %v %+v, want a failed post", firstAttempts, firstResults)
	}
	// the restarted endpoint goes on from the first's last attempt, or
	// reruns it when the kill came before its post was journaled.
	if !results[len(results)-1].Delivered || attempts[0] < firstAttempts[len(firstAttempts)-1] {
		t.Fatalf("restarted endpoint attempts %v %+v after %v, want the chain carried on and delivered", attempts, results, firstAttempts)
	}
	for _, r := range append(firstResults, results...) {
		if strings.Contains(r.Error, "tok_pathsecret") || strings.Contains(r.Error, "querysecret") {
			t.Fatalf("attempt error spells out the url: %s", r.Error)
		}
	}
	recv.mu.Lock()
	got := append([]delivered(nil), recv.got...)
	recv.mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("receiver got %d posts, want 1", len(got))
	}
	if err := hitl.VerifySignature([]byte("whsec"), got[0].sig, got[0].body, time.Now(), time.Minute); err != nil {
		t.Fatalf("signature: %v", err)
	}

	journal := readWebhookJournal(t, ctx, d.ID)
	for _, want := range [][]byte{[]byte(d.ID), []byte(base64.StdEncoding.EncodeToString(d.Target)), []byte("int_abc")} {
		if !bytes.Contains(journal, want) {
			t.Fatalf("journal does not hold %q; the checks below would be vacuous", want)
		}
	}
	for _, leak := range []string{"tok_pathsecret", "querysecret", "whsec", base64.StdEncoding.EncodeToString([]byte("whsec"))} {
		if bytes.Contains(journal, []byte(leak)) {
			t.Fatalf("journal holds %q", leak)
		}
	}
}

// TestWebhookDeadLettersAfterTheWindow: a receiver that never answers is
// retried until the next attempt would fall outside the window, then the
// delivery is dead-lettered with the last error and ends.
func TestWebhookDeadLettersAfterTheWindow(t *testing.T) {
	// restate 1.8 starts a delayed send up to about a second late, so the
	// window fits several attempts at that pace.
	shortWebhookSchedule(t, 50*time.Millisecond, 100*time.Millisecond, 6*time.Second)
	d := testDelivery(t, "http://"+freeAddr(t)+"/hook")
	fleet := newHookFleet(d.ID)
	client := serveFleet(t, fleet, fastPolicy)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := client.SendWebhook(ctx, d); err != nil {
		t.Fatalf("send: %v", err)
	}
	waitDone(t, fleet.done, 15*time.Second)
	attempts, _, dead := fleet.seen()
	if len(dead) != 1 || dead[0] == "" {
		t.Fatalf("dead letters %v, want one with the last error", dead)
	}
	n := len(attempts)
	if n < 4 || attempts[n-1] != -attempts[n-2] {
		t.Fatalf("attempts %v, want several posts and a dead letter at the last", attempts)
	}
	for i := range n - 1 {
		if attempts[i] != i+1 {
			t.Fatalf("attempts %v, want them numbered from 1", attempts)
		}
	}
}

// TestWebhookSealedEndpointDeadLetters: an endpoint that no longer decrypts
// is not retried.
func TestWebhookSealedEndpointDeadLetters(t *testing.T) {
	d := testDelivery(t, "http://unused.invalid/hook")
	d.Target = []byte("not ciphertext")
	fleet := newHookFleet(d.ID)
	client := serveFleet(t, fleet, fastPolicy)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := client.SendWebhook(ctx, d); err != nil {
		t.Fatalf("send: %v", err)
	}
	waitDone(t, fleet.done, 15*time.Second)
	attempts, _, dead := fleet.seen()
	if len(dead) != 1 || !strings.Contains(dead[0], "decrypt") {
		t.Fatalf("dead letters %v, want the sealed error", dead)
	}
	if fmt.Sprint(attempts) != "[1 -1]" {
		t.Fatalf("attempts %v, want one post and its dead letter", attempts)
	}
}

// readWebhookJournal returns the journals of the Webhooks invocations that
// carry deliveryID (restate keeps finished ones, so earlier runs' are there
// too), payloads decoded as readJournal does for an environment.
func readWebhookJournal(t *testing.T, ctx context.Context, deliveryID string) []byte {
	t.Helper()
	admin, _ := restateTestURLs(t)
	query := fmt.Sprintf("SELECT j.id, j.entry_json FROM sys_journal j JOIN sys_invocation i ON j.id = i.id "+
		"WHERE i.target_service_name = '%s'", WebhooksService)
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
			ID        string `json:"id"`
			EntryJSON string `json:"entry_json"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("journal query: %v: %s", err, raw)
	}
	byInvocation := map[string][]byte{}
	for _, r := range out.Rows {
		if r.EntryJSON == "" {
			// an entry with no json form, such as a still-pending one.
			continue
		}
		var entry any
		if err := json.Unmarshal([]byte(r.EntryJSON), &entry); err != nil {
			t.Fatalf("journal entry %q: %v", r.EntryJSON, err)
		}
		b := append(byInvocation[r.ID], r.EntryJSON...)
		byInvocation[r.ID] = appendPayloads(b, entry)
	}
	var all []byte
	for _, b := range byInvocation {
		if bytes.Contains(b, []byte(deliveryID)) {
			all = append(all, b...)
		}
	}
	return all
}

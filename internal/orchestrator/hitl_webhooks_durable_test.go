package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/folsomintel/fuse/internal/hitl"
)

// fakeWebhooks stands in for the restate client and keeps every delivery it
// was asked to enqueue. err fails every send.
type fakeWebhooks struct {
	mu   sync.Mutex
	sent []WebhookDelivery
	err  error
	// fails refuses that many sends first; tries counts every send.
	fails int
	tries int
}

func (f *fakeWebhooks) SendWebhook(_ context.Context, d WebhookDelivery) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tries++
	if f.err != nil {
		return f.err
	}
	if f.fails > 0 {
		f.fails--
		return errors.New("restate busy")
	}
	f.sent = append(f.sent, d)
	return nil
}

func (f *fakeWebhooks) wait(t *testing.T, n int) []WebhookDelivery {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		if len(f.sent) >= n {
			out := append([]WebhookDelivery(nil), f.sent...)
			f.mu.Unlock()
			return out
		}
		f.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("got %d durable webhook sends, want %d", len(f.sent), n)
	return nil
}

// hookMetrics records WebhookMetrics calls.
type hookMetrics struct {
	opMetrics
	mu       sync.Mutex
	attempts []int
	backlog  []int
	dead     []string
}

func (m *hookMetrics) WebhookAttempted(_ string, status int, _ error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.attempts = append(m.attempts, status)
}

func (m *hookMetrics) WebhookBacklog(_ string, n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.backlog = append(m.backlog, n)
}

func (m *hookMetrics) WebhookDeadLettered(endpoint string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dead = append(m.dead, endpoint)
}

// durableNotifyFleet is notifyFleet with an encryption key, a metrics
// recorder and hooks as the durable webhook client.
func durableNotifyFleet(t *testing.T, hookURL string, hooks DurableWebhooks) (*FleetManager, string, *hookMetrics) {
	t.Helper()
	metrics := &hookMetrics{}
	spec := askSpec()
	spec.Notify = []hitl.Webhook{{URL: hookURL, Secret: "HOOK"}}
	fm, vmID := hitlFleet(t, FleetConfig{TokenEncryptionKey: testEncryptionKey(), Metrics: metrics}, spec)
	fm.mu.Lock()
	fm.vms[vmID].hitlKeys = map[string]string{"HOOK": "whsec"}
	fm.mu.Unlock()
	fm.webhooks.backoff = []time.Duration{0, 0, 0}
	fm.SetPublicURL("https://fuse.example.com")
	if hooks != nil {
		fm.SetDurableWebhooks(hooks)
	}
	return fm, vmID, metrics
}

// TestHitlWebhookDurableSend: a notification is enqueued
// rather than posted, and what restate would journal carries neither the url
// (whose path holds a token here) nor the signing secret. the post step
// unseals both and delivers a signed, content-free body.
func TestHitlWebhookDurableSend(t *testing.T) {
	rc := newReceiver(t)
	hooks := &fakeWebhooks{}
	hookURL := rc.srv.URL + "/hooks/tok_pathsecret?key=querysecret"
	fm, vmID, metrics := durableNotifyFleet(t, hookURL, hooks)

	in, err := ask(fm, vmID)
	if err != nil {
		t.Fatal(err)
	}
	d := hooks.wait(t, 1)[0]
	u, _ := url.Parse(rc.srv.URL)
	if d.Endpoint != u.Host || d.Attempt != 1 || d.ID == "" || d.FirstAt.IsZero() {
		t.Fatalf("delivery = %+v", d)
	}
	if d.Notification.InterventionID != in.ID || d.Notification.Event != hitl.AuditRaised {
		t.Fatalf("notification = %+v", d.Notification)
	}
	journaled, _ := json.Marshal(d)
	for _, leak := range []string{"tok_pathsecret", "querysecret", "whsec", "deploy to prod"} {
		if bytes.Contains(journaled, []byte(leak)) {
			t.Fatalf("delivery json holds %q: %s", leak, journaled)
		}
	}

	res, err := fm.WebhookPost(context.Background(), d)
	if err != nil || !res.Delivered || res.Status != 200 {
		t.Fatalf("post = %+v, %v", res, err)
	}
	got := rc.wait(t, 1)[0]
	if err := hitl.VerifySignature([]byte("whsec"), got.sig, got.body, time.Now(), time.Minute); err != nil {
		t.Fatalf("signature: %v", err)
	}
	if strings.Contains(string(got.body), "deploy to prod") {
		t.Fatalf("webhook body leaked the payload: %s", got.body)
	}
	metrics.mu.Lock()
	defer metrics.mu.Unlock()
	if len(metrics.attempts) != 1 || metrics.attempts[0] != 200 {
		t.Fatalf("attempts = %v, want one 200", metrics.attempts)
	}
	if n := len(metrics.backlog); n == 0 || metrics.backlog[n-1] != 0 {
		t.Fatalf("backlog = %v, want it back at 0", metrics.backlog)
	}
}

// TestHitlWebhookDurableSendRetries: a send restate does not take is tried
// again on the webhook backoff, and one it never takes stays owed until a
// reconcile after restate is back sends it. nothing is posted from the
// orchestrator itself.
func TestHitlWebhookDurableSendRetries(t *testing.T) {
	rc := newReceiver(t)
	hooks := &fakeWebhooks{fails: 2}
	fm, vmID, _ := durableNotifyFleet(t, rc.srv.URL, hooks)
	if _, err := ask(fm, vmID); err != nil {
		t.Fatal(err)
	}
	fm.webhookSends.Wait()
	if len(hooks.sent) != 1 || hooks.tries != 3 {
		t.Fatalf("sent %d after %d tries, want one on the third", len(hooks.sent), hooks.tries)
	}

	rc2 := newReceiver(t)
	down := &fakeWebhooks{err: errors.New("restate down")}
	fm2, vm2, metrics := durableNotifyFleet(t, rc2.srv.URL, down)
	if _, err := ask(fm2, vm2); err != nil {
		t.Fatal(err)
	}
	fm2.webhookSends.Wait()
	if down.tries != len(fm2.webhooks.backoff) {
		t.Fatalf("%d sends, want one per backoff step (%d)", down.tries, len(fm2.webhooks.backoff))
	}
	rc2.mu.Lock()
	posted := len(rc2.got)
	rc2.mu.Unlock()
	if posted != 0 {
		t.Fatalf("a refused send was posted %d times in process", posted)
	}
	metrics.mu.Lock()
	if n := len(metrics.backlog); n == 0 || metrics.backlog[n-1] != 1 {
		t.Fatalf("backlog = %v, want the refused delivery still owed", metrics.backlog)
	}
	metrics.mu.Unlock()

	// a reconcile while restate is still down keeps it; one after it is
	// back sends it.
	fm2.resendWebhooks(context.Background())
	if len(down.sent) != 0 {
		t.Fatalf("resent %d while restate is down", len(down.sent))
	}
	down.mu.Lock()
	down.err = nil
	down.mu.Unlock()
	fm2.resendWebhooks(context.Background())
	down.mu.Lock()
	sent := len(down.sent)
	down.mu.Unlock()
	if sent != 1 {
		t.Fatalf("resent %d after restate came back, want 1", sent)
	}
	fm2.webhookMu.Lock()
	left := len(fm2.webhookUnsent)
	fm2.webhookMu.Unlock()
	if left != 0 {
		t.Fatalf("%d deliveries still owed after the resend", left)
	}
}

// TestWebhookPostFailures: a receiver that does not answer 2xx is a failed
// attempt with its status, a dead receiver one with status 0 and an error
// that does not spell out the url, and an endpoint that does not decrypt is
// the one error.
func TestWebhookPostFailures(t *testing.T) {
	rc := newReceiver(t)
	rc.fail = 1
	hooks := &fakeWebhooks{}
	fm, vmID, _ := durableNotifyFleet(t, rc.srv.URL, hooks)
	if _, err := ask(fm, vmID); err != nil {
		t.Fatal(err)
	}
	d := hooks.wait(t, 1)[0]
	res, err := fm.WebhookPost(context.Background(), d)
	if err != nil || res.Delivered || res.Status != 500 || res.Error == "" {
		t.Fatalf("post to a failing receiver = %+v, %v", res, err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := "http://" + ln.Addr().String() + "/hooks/tok_pathsecret?key=querysecret"
	_ = ln.Close()
	gone, err := fm.newWebhookDelivery(webhookTarget{url: dead, secret: []byte("whsec")}, d.Notification)
	if err != nil {
		t.Fatal(err)
	}
	res, err = fm.WebhookPost(context.Background(), gone)
	if err != nil || res.Delivered || res.Status != 0 || res.Error == "" {
		t.Fatalf("post to a dead receiver = %+v, %v", res, err)
	}
	if strings.Contains(res.Error, "tok_pathsecret") || strings.Contains(res.Error, "querysecret") {
		t.Fatalf("attempt error spells out the url: %s", res.Error)
	}

	sealed := d
	sealed.Target = []byte("not ciphertext")
	if _, err := fm.WebhookPost(context.Background(), sealed); !WebhookEndpointSealed(err) {
		t.Fatalf("post with an unreadable endpoint: %v, want the sealed error", err)
	}
}

// TestWebhookDeadLetter: a delivery out of its window leaves a dead letter
// keyed by its id, without the url, and leaves the backlog.
func TestWebhookDeadLetter(t *testing.T) {
	rc := newReceiver(t)
	hooks := &fakeWebhooks{}
	hookURL := rc.srv.URL + "/hooks/tok_pathsecret"
	fm, vmID, metrics := durableNotifyFleet(t, hookURL, hooks)
	if _, err := ask(fm, vmID); err != nil {
		t.Fatal(err)
	}
	d := hooks.wait(t, 1)[0]
	d.Attempt = 7
	if err := fm.WebhookDeadLetter(context.Background(), d, "receiver answered 503"); err != nil {
		t.Fatal(err)
	}
	rows, err := fm.store.ListDeadLetters(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Kind != DeadLetterWebhook || rows[0].EntityID != d.ID ||
		rows[0].RetryCount != 7 || rows[0].Reason != "receiver answered 503" {
		t.Fatalf("dead letters = %+v", rows)
	}
	if bytes.Contains(rows[0].Payload, []byte("tok_pathsecret")) {
		t.Fatalf("dead letter payload holds the url: %s", rows[0].Payload)
	}
	metrics.mu.Lock()
	defer metrics.mu.Unlock()
	if len(metrics.dead) != 1 || metrics.dead[0] != d.Endpoint {
		t.Fatalf("dead-lettered = %v, want [%s]", metrics.dead, d.Endpoint)
	}
	if n := len(metrics.backlog); n < 2 || metrics.backlog[0] != 1 || metrics.backlog[n-1] != 0 {
		t.Fatalf("backlog = %v, want 1 then back to 0", metrics.backlog)
	}
}

// TestHitlWebhookDeliveryMetered: the durable delivery reports every attempt,
// the failed one before the retry that lands.
func TestHitlWebhookDeliveryMetered(t *testing.T) {
	rc := newReceiver(t)
	rc.fail = 1
	fm, vmID, metrics := durableNotifyFleet(t, rc.srv.URL, nil)
	if _, err := ask(fm, vmID); err != nil {
		t.Fatal(err)
	}
	rc.wait(t, 1)
	deadline := time.Now().Add(5 * time.Second)
	for {
		metrics.mu.Lock()
		got := append([]int(nil), metrics.attempts...)
		metrics.mu.Unlock()
		if len(got) == 2 {
			if got[0] != 500 || got[1] != 200 {
				t.Fatalf("attempts = %v, want 500 then 200", got)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("attempts = %v, want 500 then 200", got)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestWebhookEndpoint(t *testing.T) {
	for raw, want := range map[string]string{
		"https://hooks.example.com/services/T0/B0/secret?x=1": "hooks.example.com",
		"http://127.0.0.1:8080/h":                             "127.0.0.1:8080",
		"::not a url":                                         "invalid",
	} {
		if got := webhookEndpoint(raw); got != want {
			t.Errorf("webhookEndpoint(%q) = %q, want %q", raw, got, want)
		}
	}
}

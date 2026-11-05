package orchestrator

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/folsomintel/fuse/internal/hitl"
)

// fakeGuest is a guest agent's computer surface: it records the actions it
// is sent and answers each with a screenshot.
type fakeGuest struct {
	mu      sync.Mutex
	actions []map[string]any
	shots   int
}

func (g *fakeGuest) received() []map[string]any {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]map[string]any(nil), g.actions...)
}

func startFakeGuest(t *testing.T, fm *FleetManager, vmID string) *fakeGuest {
	t.Helper()
	g := &fakeGuest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		switch r.URL.Path {
		case "/v1/computer/action":
			var a map[string]any
			_ = json.NewDecoder(r.Body).Decode(&a)
			g.actions = append(g.actions, a)
			_ = json.NewEncoder(w).Encode(hitl.ComputerResult{Output: "ok", Screenshot: "UE5H"})
		case "/v1/computer/screenshot":
			g.shots++
			_ = json.NewEncoder(w).Encode(hitl.ComputerResult{Screenshot: "U0hPVA=="})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	mockEnvOf(fm, vmID).url = strings.TrimPrefix(srv.URL, "http://")
	return g
}

func computerSpec(gates ...hitl.Gate) *hitl.Spec {
	return &hitl.Spec{TimeoutSeconds: 600, Requests: hitl.Requests{Ask: &hitl.AskRequests{}}, Gates: gates}
}

func gateComputer(fm *FleetManager, vmID, body string) (*hitl.Intervention, error) {
	return fm.GateComputer(context.Background(), vmID, []byte(body), GateCaller{By: testActor})
}

func TestComputerGateApprovedWithEditedTextTypesTheEdit(t *testing.T) {
	fm, vmID := hitlFleet(t, FleetConfig{}, computerSpec(
		hitl.Gate{Name: "typing", On: hitl.KindComputer, Match: hitl.GateMatch{Action: []string{"type"}}},
		hitl.Gate{Name: "no-card", On: hitl.KindComputer, Match: hitl.GateMatch{TextContains: []string{"4242"}}, Mode: hitl.ModeDeny},
	))
	guest := startFakeGuest(t, fm, vmID)

	if held, err := gateComputer(fm, vmID, `{"action":"left_click","coordinate":[10,20]}`); held != nil || err != nil {
		t.Fatalf("an ungated click must pass: held=%v err=%v", held, err)
	}
	var denied *GateDeniedError
	if _, err := gateComputer(fm, vmID, `{"action":"type","text":"4242 4242"}`); !errors.As(err, &denied) || denied.Gate != "no-card" {
		t.Fatalf("card number: err = %v", err)
	}

	held, err := gateComputer(fm, vmID, `{"action":"type","text":"rm -rf ~"}`)
	if err != nil || held == nil || held.Kind != hitl.KindComputer || held.Computer.Text != "rm -rf ~" {
		t.Fatalf("type should be held: held=%+v err=%v", held, err)
	}
	// the edit is re-checked against the deny gates.
	bad := "pay 4242"
	if _, err := fm.DecideIntervention(context.Background(), held.ID, DecideRequest{Outcome: hitl.OutcomeApprove, EditedText: &bad, By: approver}); !errors.As(err, &denied) {
		t.Fatalf("an edit matching a deny gate must be refused: %v", err)
	}
	edited := "ls ~"
	in, err := fm.DecideIntervention(context.Background(), held.ID, DecideRequest{Outcome: hitl.OutcomeApprove, EditedText: &edited, By: approver})
	if err != nil {
		t.Fatal(err)
	}
	if in.Decision.EditedText == nil || *in.Decision.EditedText != edited || in.Decision.Grant != "once" {
		t.Fatalf("decision = %+v", in.Decision)
	}
	got := waitSettled(t, fm, held.ID)
	if got.Result == nil || got.Result.Computer == nil || got.Result.Computer.Screenshot != "UE5H" {
		t.Fatalf("result = %+v", got.Result)
	}
	acts := guest.received()
	if len(acts) != 1 || acts[0]["action"] != "type" || acts[0]["text"] != edited {
		t.Fatalf("guest received %v; want only the edited type", acts)
	}
}

func TestComputerEditRefusedOnAClick(t *testing.T) {
	fm, vmID := hitlFleet(t, FleetConfig{}, computerSpec(hitl.Gate{Name: "clicks", On: hitl.KindComputer, Match: hitl.GateMatch{Action: []string{"left_click"}}}))
	held, err := gateComputer(fm, vmID, `{"action":"left_click","coordinate":[1,2]}`)
	if err != nil || held == nil {
		t.Fatalf("click should be held: %v", err)
	}
	text := "x"
	if _, err := fm.DecideIntervention(context.Background(), held.ID, DecideRequest{Outcome: hitl.OutcomeApprove, EditedText: &text, By: approver}); !errors.Is(err, ErrInvalidIntervention) {
		t.Fatalf("edited_text on a click: err = %v", err)
	}
}

// fakeDecider answers by the text of a type action, and records what it
// was sent.
type fakeDecider struct {
	mu   sync.Mutex
	reqs []hitl.DeciderRequest
	sigs []string
	raw  [][]byte
}

func startFakeDecider(t *testing.T, answer func(hitl.DeciderRequest) (int, string, time.Duration)) (*fakeDecider, string) {
	t.Helper()
	d := &fakeDecider{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req hitl.DeciderRequest
		_ = json.Unmarshal(body, &req)
		d.mu.Lock()
		d.reqs = append(d.reqs, req)
		d.sigs = append(d.sigs, r.Header.Get(hitl.HeaderSignature))
		d.raw = append(d.raw, body)
		d.mu.Unlock()
		status, out, delay := answer(req)
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return
			}
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, out)
	}))
	t.Cleanup(srv.Close)
	return d, srv.URL
}

func TestDeciderAllowsDeniesAndEscalates(t *testing.T) {
	old := deciderTimeout
	deciderTimeout = 300 * time.Millisecond
	t.Cleanup(func() { deciderTimeout = old })

	d, url := startFakeDecider(t, func(r hitl.DeciderRequest) (int, string, time.Duration) {
		switch r.Computer.Text {
		case "hello":
			return 200, `{"decision":"allow","reason":"harmless"}`, 0
		case "drop table":
			return 200, `{"decision":"deny","reason":"no sql in the browser"}`, 0
		case "slow":
			return 200, `{"decision":"allow"}`, 2 * time.Second
		case "broken":
			return 500, `oops`, 0
		}
		return 200, `{"decision":"ask","reason":"looks like a payment"}`, 0
	})
	fm, vmID := hitlFleet(t, FleetConfig{}, computerSpec(hitl.Gate{
		Name: "typing", On: hitl.KindComputer, Match: hitl.GateMatch{Action: []string{"type"}},
		Decide: &hitl.Decider{URL: url, Secret: "DECIDE_KEY"},
	}))
	fm.mu.Lock()
	fm.vms[vmID].hitlKeys = map[string]string{"DECIDE_KEY": "s3cret"}
	fm.mu.Unlock()
	guest := startFakeGuest(t, fm, vmID)

	// allow: the caller goes on as if no gate matched, and the record says
	// the decider decided.
	held, err := gateComputer(fm, vmID, `{"action":"type","text":"hello"}`)
	if held != nil || err != nil {
		t.Fatalf("allow: held=%v err=%v", held, err)
	}
	// deny: refused with the decider's reason.
	var denied *GateDeniedError
	if _, err := gateComputer(fm, vmID, `{"action":"type","text":"drop table"}`); !errors.As(err, &denied) || denied.Reason != "no sql in the browser" {
		t.Fatalf("deny: err = %v", err)
	}
	// ask: held for a human, with the decider's note.
	held, err = gateComputer(fm, vmID, `{"action":"type","text":"card"}`)
	if err != nil || held == nil || held.Decider == nil || held.Decider.Reason != "looks like a payment" {
		t.Fatalf("ask: held=%+v err=%v", held, err)
	}
	// a timeout and a 500 both fall through to ask.
	for _, text := range []string{"slow", "broken"} {
		start := time.Now()
		h, err := gateComputer(fm, vmID, `{"action":"type","text":"`+text+`"}`)
		if err != nil || h == nil || h.Decider == nil || h.Decider.Error == "" {
			t.Fatalf("%s: held=%+v err=%v", text, h, err)
		}
		if time.Since(start) > 1500*time.Millisecond {
			t.Fatalf("%s: the decider call outlived its timeout (%s)", text, time.Since(start))
		}
	}

	// the decided rows record source: decider.
	list, _, err := fm.ListInterventions(context.Background(), InterventionListFilter{EnvID: vmID})
	if err != nil {
		t.Fatal(err)
	}
	var allowed, rejected int
	for _, in := range list {
		if in.Decision == nil || in.Decision.DecidedBy.Source != hitl.ActorDecider {
			continue
		}
		if in.Decision.DecidedBy.ID != "decider:typing" {
			t.Fatalf("decided by = %+v", in.Decision.DecidedBy)
		}
		if in.State == hitl.StateApproved {
			allowed++
			if !in.Settled() || in.Runs() {
				t.Fatal("a decider allow is settled and not run by fuse")
			}
		} else if in.State == hitl.StateRejected {
			rejected++
		}
	}
	if allowed != 1 || rejected != 1 {
		t.Fatalf("decider rows: allowed=%d rejected=%d", allowed, rejected)
	}

	// every call is signed and carries a screenshot for a computer action.
	d.mu.Lock()
	defer d.mu.Unlock()
	for i, sig := range d.sigs {
		if err := hitl.VerifySignature([]byte("s3cret"), sig, d.raw[i], time.Now(), time.Minute); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if d.reqs[i].Screenshot != "U0hPVA==" || d.reqs[i].Gate != "typing" || d.reqs[i].On != hitl.KindComputer {
			t.Fatalf("call %d = %+v", i, d.reqs[i])
		}
	}
	if guest.shots != len(d.reqs) {
		t.Fatalf("screenshots = %d, decider calls = %d", guest.shots, len(d.reqs))
	}
}

func TestDeciderWithoutItsSecretFallsThrough(t *testing.T) {
	_, url := startFakeDecider(t, func(hitl.DeciderRequest) (int, string, time.Duration) {
		return 200, `{"decision":"allow"}`, 0
	})
	fm, vmID := hitlFleet(t, FleetConfig{}, gateSpecWithDecider(url))
	held, err := gateExec(fm, vmID, "kubectl", "apply")
	if err != nil || held == nil || held.Decider == nil || !strings.Contains(held.Decider.Error, "unavailable") {
		t.Fatalf("an unsigned decider call must not be made: held=%+v err=%v", held, err)
	}
}

func gateSpecWithDecider(url string) *hitl.Spec {
	return computerSpec(hitl.Gate{Name: "deploys", On: hitl.KindExec, Match: hitl.GateMatch{Program: []string{"kubectl"}}, Decide: &hitl.Decider{URL: url, Secret: "MISSING"}})
}

func TestReviewRequestReadsTheFileAndCommentsReachTheAgent(t *testing.T) {
	spec := &hitl.Spec{TimeoutSeconds: 600, Requests: hitl.Requests{Review: &hitl.ReviewRequests{}}}
	fm, vmID := hitlFleet(t, FleetConfig{}, spec)
	env := mockEnvOf(fm, vmID)
	env.execResult = ExecResult{ExitCode: 0, Stdout: []byte("diff --git a/x b/x\n--- a/x\n+++ b/x\n")}

	in, err := fm.RequestHuman(context.Background(), vmID, HumanRequest{Kind: hitl.KindReview, Review: hitl.ReviewPayload{Path: "/work/change.diff", Message: "please check"}, By: testActor})
	if err != nil {
		t.Fatal(err)
	}
	if in.Review == nil || in.Review.Format != hitl.ReviewDiff || !strings.HasPrefix(in.Review.Content, "diff --git") {
		t.Fatalf("review = %+v", in.Review)
	}
	if argv := env.execArgv[len(env.execArgv)-1]; argv[0] != "head" || argv[len(argv)-1] != "/work/change.diff" {
		t.Fatalf("read argv = %q", argv)
	}
	out, err := fm.DecideIntervention(context.Background(), in.ID, DecideRequest{Outcome: hitl.OutcomeReject, Reason: "the second hunk drops a check", By: approver})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := fm.GetIntervention(context.Background(), out.ID)
	if got.Decision.Reason != "the second hunk drops a check" || got.Review.Content == "" {
		t.Fatalf("stored review = %+v", got)
	}

	env.execResult = ExecResult{ExitCode: 1, Stderr: []byte("head: cannot open '/nope'")}
	if _, err := fm.RequestHuman(context.Background(), vmID, HumanRequest{Kind: hitl.KindReview, Review: hitl.ReviewPayload{Path: "/nope"}, By: testActor}); !errors.Is(err, ErrInvalidIntervention) {
		t.Fatalf("a missing file: err = %v", err)
	}
	if _, err := fm.RequestHuman(context.Background(), vmID, HumanRequest{Kind: hitl.KindAsk, Ask: hitl.AskPayload{Question: "x"}, By: testActor}); !errors.Is(err, ErrHitlNotDeclared) {
		t.Fatalf("an undeclared kind: err = %v", err)
	}
}

// rfbClient builds the viewer half of an rfb 3.8 session with no auth.
func rfbClient() []byte {
	out := []byte("RFB 003.008\n")
	out = append(out, 1, 0)                   // security none, ClientInit shared=0
	out = append(out, 2, 0, 0, 1, 0, 0, 0, 0) // SetEncodings, one encoding
	return out
}

func rfbKey(down bool, sym uint32) []byte {
	b := make([]byte, 8)
	b[0] = 4
	if down {
		b[1] = 1
	}
	binary.BigEndian.PutUint32(b[4:], sym)
	return b
}

func rfbPointer(mask byte, x, y uint16) []byte {
	b := []byte{5, mask, 0, 0, 0, 0}
	binary.BigEndian.PutUint16(b[2:], x)
	binary.BigEndian.PutUint16(b[4:], y)
	return b
}

type nopStream struct{ io.Reader }

func (nopStream) Write(p []byte) (int, error) { return len(p), nil }
func (nopStream) Close() error                { return nil }

func TestDesktopRecorderLogsInputNotText(t *testing.T) {
	done := make(chan desktopRecording, 1)
	shots := 0
	rec := newDesktopRecorder(nopStream{strings.NewReader("")}, desktopRecording{InterventionID: "int_1"}, 20*time.Millisecond,
		func(context.Context) (string, error) { shots++; return "U0hPVA==", nil },
		func(r desktopRecording) { done <- r })

	var in []byte
	in = append(in, rfbClient()...)
	in = append(in, rfbPointer(0, 5, 5)...)     // a move: not logged
	in = append(in, rfbPointer(1, 100, 200)...) // press
	in = append(in, rfbPointer(0, 100, 200)...) // release
	for _, c := range "hunter2" {
		in = append(in, rfbKey(true, uint32(c))...)
		in = append(in, rfbKey(false, uint32(c))...)
	}
	in = append(in, rfbKey(true, 0xff0d)...) // Return
	in = append(in, rfbKey(true, 0xffe3)...) // ctrl down
	in = append(in, rfbKey(true, 'c')...)
	in = append(in, rfbKey(false, 0xffe3)...)
	in = append(in, []byte{6, 0, 0, 0, 0, 0, 0, 3, 'a', 'b', 'c'}...) // a paste
	// feed in awkward chunks: the parser must not care where writes split.
	for i := 0; i < len(in); i += 5 {
		end := min(i+5, len(in))
		if _, err := rec.Write(in[i:end]); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(70 * time.Millisecond)
	_ = rec.Close()
	r := <-done

	b, _ := json.Marshal(r.Events)
	if strings.Contains(string(b), "hunter2") || strings.Contains(string(b), "abc") {
		t.Fatalf("the log must not hold typed or pasted text: %s", b)
	}
	var kinds []string
	for _, e := range r.Events {
		kinds = append(kinds, e.Type+":"+e.Key)
	}
	want := "pointer:,pointer:,text:,key:Return,key:ctrl+c,clipboard:"
	if strings.Join(kinds, ",") != want {
		t.Fatalf("events = %s, want %s", strings.Join(kinds, ","), want)
	}
	if r.Events[2].Chars != 7 || r.Events[5].Bytes != 3 || *r.Events[0].X != 100 || *r.Events[0].Buttons != 1 {
		t.Fatalf("events = %s", b)
	}
	if r.InputLogIncomplete || r.Kind != recordingDesktop || len(r.Screenshots) < 2 || r.EndedAt.IsZero() {
		t.Fatalf("recording = %+v (shots %d)", r, len(r.Screenshots))
	}
}

func TestDesktopTakeoverLocksTheStreamAndNeedsDesktopVia(t *testing.T) {
	spec := &hitl.Spec{TimeoutSeconds: 600, Requests: hitl.Requests{Takeover: &hitl.TakeoverRequests{Via: []hitl.TakeoverVia{hitl.ViaTerminal, hitl.ViaDesktop}}}}
	fm, vmID := hitlFleet(t, FleetConfig{}, spec)
	ctx := context.Background()
	in, err := fm.StartTakeover(ctx, vmID, TakeoverRequest{Via: hitl.ViaDesktop, By: approver})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fm.ComputerStream(ctx, vmID); !errors.Is(err, ErrEnvLocked) {
		t.Fatalf("plain stream while held: err = %v, want ErrEnvLocked", err)
	}
	if _, err := fm.GateComputer(ctx, vmID, []byte(`{"action":"left_click","coordinate":[1,1]}`), GateCaller{By: testActor}); !errors.Is(err, ErrEnvLocked) {
		t.Fatalf("agent click while held: err = %v, want ErrEnvLocked", err)
	}
	if _, err := fm.TakeoverDesktop(ctx, in.ID, testActor, false, ""); !errors.Is(err, ErrNotTakeoverHolder) {
		t.Fatalf("non-holder desktop: err = %v", err)
	}
	if _, err := fm.ReleaseTakeover(ctx, vmID, "done", approver, ""); err != nil {
		t.Fatal(err)
	}
	term, err := fm.StartTakeover(ctx, vmID, TakeoverRequest{Via: hitl.ViaTerminal, By: approver})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fm.TakeoverDesktop(ctx, term.ID, approver, false, ""); !errors.Is(err, ErrInvalidIntervention) {
		t.Fatalf("desktop on a terminal takeover: err = %v", err)
	}
}

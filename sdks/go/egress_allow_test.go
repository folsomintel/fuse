package fuse

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"testing"
)

func TestSetEgressAllow_sendsListAndDecodesStatus(t *testing.T) {
	var bodies []string
	c, done := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/v1/environments/vm-1/egress/allow" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Idempotency-Key") == "" {
			t.Errorf("no Idempotency-Key")
		}
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		var req struct {
			Allow []string `json:"allow"`
		}
		_ = json.Unmarshal(b, &req)
		if len(req.Allow) == 0 {
			_, _ = w.Write([]byte(`{"mode":"direct"}`))
			return
		}
		_, _ = w.Write([]byte(`{"mode":"direct","allow":["10.0.0.0/8","1.2.3.4/32"]}`))
	})
	defer done()

	ctx := context.Background()
	st, err := c.Environments.SetEgressAllow(ctx, "vm-1", []string{"10.0.0.0/8", "1.2.3.4"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"10.0.0.0/8", "1.2.3.4/32"}; st.Mode != EgressModeDirect || !reflect.DeepEqual(st.Allow, want) {
		t.Fatalf("status = %+v, want direct with %v", st, want)
	}
	// nil lifts the list and must still send the key, as [].
	st, err = c.Environments.SetEgressAllow(ctx, "vm-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if st.Allow != nil {
		t.Errorf("allow = %v after lifting, want nil", st.Allow)
	}
	want := []string{`{"allow":["10.0.0.0/8","1.2.3.4"]}`, `{"allow":[]}`}
	for i := range want {
		if i >= len(bodies) || compactJSON(t, bodies[i]) != want[i] {
			t.Errorf("bodies = %q, want %q", bodies, want)
			break
		}
	}
}

func TestSetEgressAllow_followsAcceptedOperation(t *testing.T) {
	c, done := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"operation_id":"op_1","operation":{"id":"op_1","kind":"egress.allow","state":"running","started_at":"2026-11-15T00:00:00Z"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"op_1","kind":"egress.allow","state":"succeeded","started_at":"2026-11-15T00:00:00Z","status_code":200,"result":{"mode":"direct","allow":["10.0.0.0/8"]}}`))
	})
	defer done()

	st, err := c.Environments.SetEgressAllow(context.Background(), "vm-1", []string{"10.0.0.0/8"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(st.Allow, []string{"10.0.0.0/8"}) {
		t.Fatalf("allow = %v, want [10.0.0.0/8]", st.Allow)
	}
}

func TestSetEgressAllow_conflictIsAPIError(t *testing.T) {
	c, done := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"code":"conflict","message":"egress allow requires direct egress"}}`))
	})
	defer done()

	_, err := c.Environments.SetEgressAllow(context.Background(), "vm-1", []string{"10.0.0.0/8"})
	if !IsConflict(err) {
		t.Fatalf("err = %v, want a conflict", err)
	}
}

func TestEgressSpec_allowOnCreate(t *testing.T) {
	b, err := json.Marshal(CreateRequest{TaskID: "t", Egress: &EgressSpec{Mode: EgressModeDirect, Allow: []string{"10.0.0.0/8"}}})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Egress map[string]any `json:"egress"`
	}
	_ = json.Unmarshal(b, &got)
	if allow, _ := got.Egress["allow"].([]any); len(allow) != 1 || allow[0] != "10.0.0.0/8" {
		t.Fatalf("egress = %v, want allow [10.0.0.0/8]", got.Egress)
	}
}

func compactJSON(t *testing.T, s string) string {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("bad json %q: %v", s, err)
	}
	b, _ := json.Marshal(v)
	return string(b)
}

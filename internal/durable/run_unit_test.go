package durable

import (
	"encoding/json"
	"iter"
	"testing"

	restate "github.com/restatedev/sdk-go"
)

// headers satisfies restate's read-only header map.
type headers map[string]string

func (h headers) Get(k string) string { return h[k] }
func (h headers) Iter() iter.Seq2[string, string] {
	return func(yield func(string, string) bool) {
		for k, v := range h {
			if !yield(k, v) {
				return
			}
		}
	}
}
func (h headers) ToMap() map[string]string     { return map[string]string(h) }
func (h headers) MarshalJSON() ([]byte, error) { return json.Marshal(map[string]string(h)) }

// any ingress client can set the header, so only an id that passes the shared
// rule is taken.
func TestInvocationRequestID(t *testing.T) {
	cases := []struct {
		h    headers
		want string
	}{
		{headers{"x-request-id": "req_abc"}, "req_abc"},
		{headers{"X-Request-ID": "req_abc"}, "req_abc"},
		{headers{"x-request-id": "bad id\nforged=1"}, ""},
		{headers{"other": "req_abc"}, ""},
		{nil, ""},
	}
	for _, c := range cases {
		req := &restate.Request{}
		if c.h != nil {
			req.Headers = c.h
		}
		if got := invocationRequestID(req); got != c.want {
			t.Errorf("headers %v: got %q, want %q", c.h, got, c.want)
		}
	}
}

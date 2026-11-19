package fuse

import (
	"context"
	"net/http"
	"testing"
)

func TestImagesGetOnHost_readsTheHostsRecord(t *testing.T) {
	c, done := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/hosts/gone/images/abc" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"host \"gone\" is not registered"}}`))
			return
		}
		if r.Method != http.MethodGet || r.URL.Path != "/v1/hosts/fc-2/images/abc" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"digest":"abc","ref":"ubuntu","manifest_digest":"sha256:x","host_id":"fc-2","state":"converting"}`))
	})
	defer done()

	img, err := c.Images.GetOnHost(context.Background(), "fc-2", "abc")
	if err != nil {
		t.Fatal(err)
	}
	if img.HostID != "fc-2" || img.State != ImageConverting {
		t.Fatalf("image %+v", img)
	}
	if _, err := c.Images.GetOnHost(context.Background(), "gone", "abc"); !IsNotFound(err) {
		t.Fatalf("unknown host: err = %v, want not_found", err)
	}
	if _, err := c.Images.GetOnHost(context.Background(), "", "abc"); err == nil {
		t.Fatal("want an error for an empty host id")
	}
}

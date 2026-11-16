package firecracker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/folsomintel/fuse/internal/orchestrator"
)

// TestConvertImageDecodesTheAgentRecord: every key fc-agent's image_public
// writes reaches orchestrator.Image, created_at among them, since it keys a
// durable convert's watch.
func TestConvertImageDecodesTheAgentRecord(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/images" {
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"digest":"d1","ref":"ubuntu","manifest_digest":"sha256:m","name":"oci-d1","state":"converting","error":"","created_at":"2026-11-16T00:00:00Z"}`))
	}))
	defer ts.Close()

	img, err := New(Config{BaseURL: ts.URL, Token: "token"}).ConvertImage(context.Background(), "ubuntu")
	if err != nil {
		t.Fatal(err)
	}
	want := orchestrator.Image{Digest: "d1", Ref: "ubuntu", ManifestDigest: "sha256:m", Name: "oci-d1",
		State: orchestrator.ImageConverting, CreatedAt: "2026-11-16T00:00:00Z"}
	if img != want {
		t.Fatalf("image %+v, want %+v", img, want)
	}
}

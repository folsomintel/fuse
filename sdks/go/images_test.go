package fuse

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestImagesConvert_sendsRefAndDecodes(t *testing.T) {
	var got ConvertImageRequest
	c, done := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/images" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"digest":"` + strings.Repeat("a", 64) + `","ref":"ubuntu:24.04","manifest_digest":"sha256:` + strings.Repeat("b", 64) + `","host_id":"fc-1","state":"converting"}`))
	})
	defer done()

	img, err := c.Images.Convert(context.Background(), ConvertImageRequest{Ref: "ubuntu:24.04", HostID: "fc-1", Region: "eu-west", Labels: map[string]string{"zone": "a"}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Ref != "ubuntu:24.04" || got.HostID != "fc-1" || got.Region != "eu-west" || got.Labels["zone"] != "a" {
		t.Fatalf("sent %+v", got)
	}
	if img.Digest != strings.Repeat("a", 64) || img.State != ImageConverting || img.HostID != "fc-1" || !strings.HasPrefix(img.ManifestDigest, "sha256:") {
		t.Fatalf("image %+v", img)
	}
}

func TestImagesGet_decodesFailureAndNotFound(t *testing.T) {
	c, done := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/images/missing" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"image not found"}}`))
			return
		}
		if r.Method != http.MethodGet || r.URL.Path != "/v1/images/abc" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"digest":"abc","ref":"busybox","manifest_digest":"sha256:x","host_id":"fc-1","state":"failed","error":"fc-bake-distro-rootfs: unsupported distro"}`))
	})
	defer done()

	img, err := c.Images.Get(context.Background(), "abc")
	if err != nil {
		t.Fatal(err)
	}
	if img.State != ImageFailed || img.Error == "" {
		t.Fatalf("image %+v", img)
	}
	_, err = c.Images.Get(context.Background(), "missing")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusNotFound {
		t.Fatalf("err = %v, want a 404 APIError", err)
	}
}

func TestSpec_carriesImageDigest(t *testing.T) {
	data, err := json.Marshal(Spec{ImageDigest: "abc"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"image_digest":"abc"`) {
		t.Fatalf("spec json %s", data)
	}
}

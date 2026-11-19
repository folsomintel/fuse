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

func TestImagesConvert_sendsAnIdempotencyKey(t *testing.T) {
	var keys []string
	c, done := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"digest":"` + strings.Repeat("a", 64) + `","ref":"ubuntu","state":"converting"}`))
	})
	defer done()

	if _, err := c.Images.Convert(context.Background(), ConvertImageRequest{Ref: "ubuntu"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Images.Convert(context.Background(), ConvertImageRequest{Ref: "ubuntu"}, WithIdempotencyKey("my-key")); err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 || keys[0] == "" || keys[1] != "my-key" {
		t.Fatalf("keys = %q, want a generated one then my-key", keys)
	}
}

func TestImagesConvert_unkeyedCallsSendDifferentKeys(t *testing.T) {
	var keys []string
	c, done := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"digest":"` + strings.Repeat("a", 64) + `","ref":"ubuntu","state":"converting"}`))
	})
	defer done()

	for range 2 {
		if _, err := c.Images.Convert(context.Background(), ConvertImageRequest{Ref: "ubuntu"}); err != nil {
			t.Fatal(err)
		}
	}
	if len(keys) != 2 || keys[0] == "" || keys[0] == keys[1] {
		t.Fatalf("keys = %q, want two different generated keys", keys)
	}
}

func TestImagesConvert_followsAcceptedOperation(t *testing.T) {
	c, done := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/images":
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"operation_id":"op_1","operation":{"id":"op_1","kind":"image.convert","state":"running","started_at":"2026-11-15T00:00:00Z"}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/operations/op_1":
			_, _ = w.Write([]byte(`{"id":"op_1","kind":"image.convert","state":"succeeded","started_at":"2026-11-15T00:00:00Z","finished_at":"2026-11-15T00:01:00Z","status_code":202,"result":{"digest":"` + strings.Repeat("a", 64) + `","ref":"ubuntu","host_id":"fc-1","state":"converting"}}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	defer done()

	img, err := c.Images.Convert(context.Background(), ConvertImageRequest{Ref: "ubuntu"})
	if err != nil {
		t.Fatal(err)
	}
	if img.Digest != strings.Repeat("a", 64) || img.State != ImageConverting || img.HostID != "fc-1" {
		t.Fatalf("image %+v, want the operation's result", img)
	}
}

func TestImagesConvert_followedFailureIsAPIError(t *testing.T) {
	c, done := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"operation_id":"op_1","operation":{"id":"op_1","kind":"image.convert","state":"running","started_at":"2026-11-15T00:00:00Z"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"op_1","kind":"image.convert","state":"failed","started_at":"2026-11-15T00:00:00Z","finished_at":"2026-11-15T00:01:00Z","status_code":507,"error":{"code":"unavailable","message":"image cache full"}}`))
	})
	defer done()

	_, err := c.Images.Convert(context.Background(), ConvertImageRequest{Ref: "ubuntu"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusInsufficientStorage {
		t.Fatalf("err = %v, want a 507 APIError", err)
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

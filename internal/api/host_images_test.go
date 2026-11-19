package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/folsomintel/fuse/internal/orchestrator"
)

func TestGetHostImage(t *testing.T) {
	d := strings.Repeat("a", 64)
	h := newImageHandler(t, orchestrator.Image{Digest: d, Ref: "ubuntu", State: orchestrator.ImageConverting})

	rr := doJSON(t, h, http.MethodGet, "/v1/hosts/fc-1/images/"+d, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. body: %s", rr.Code, rr.Body.String())
	}
	var img Image
	if err := json.NewDecoder(rr.Body).Decode(&img); err != nil {
		t.Fatal(err)
	}
	if img.Digest != d || img.State != "converting" || img.HostID != "fc-1" || img.Ref != "ubuntu" {
		t.Fatalf("image = %+v", img)
	}

	for _, c := range []struct {
		path string
		want int
	}{
		{"/v1/hosts/fc-1/images/" + strings.Repeat("b", 64), http.StatusNotFound},
		{"/v1/hosts/fc-1/images/nothex", http.StatusBadRequest},
		{"/v1/hosts/nope/images/" + d, http.StatusNotFound},
		{"/v1/hosts/gpu-1/images/" + d, http.StatusBadRequest},
	} {
		if rr := doJSON(t, h, http.MethodGet, c.path, nil); rr.Code != c.want {
			t.Fatalf("GET %s: status = %d, want %d. body: %s", c.path, rr.Code, c.want, rr.Body.String())
		}
	}
}

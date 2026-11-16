package fusefile

import (
	"errors"
	"strings"
	"testing"
)

func TestParseFrom(t *testing.T) {
	f, err := Parse([]byte("version: 1\nfrom: ubuntu:24.04\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if f.From != "ubuntu:24.04" {
		t.Fatalf("from = %q, want ubuntu:24.04", f.From)
	}
	c, err := Compile(f)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if c.Spec.From != "ubuntu:24.04" || c.Spec.Image != "" {
		t.Fatalf("compiled spec from=%q image=%q, want from carried and no image", c.Spec.From, c.Spec.Image)
	}
}

func TestParseFromRejections(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"with image", "from: ubuntu:24.04\nimage: base\n", "from and image"},
		{"with desktop", "from: ubuntu:24.04\ndesktop:\n  width: 1280\n  height: 800\n", "from and desktop"},
		{"with gpu", "from: ubuntu:24.04\nresources:\n  gpu: 1\n", "from and resources.gpu"},
		{"leading dash", "from: -rf\n", "from: invalid oci image reference"},
		{"uppercase repo", "from: Ubuntu:24.04\n", "from: invalid oci image reference"},
		{"short digest", "from: ubuntu@sha256:abc\n", "from: invalid oci image reference"},
		{"spaces", "from: ubuntu 24.04\n", "from: invalid oci image reference"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte("version: 1\n" + tc.body))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestValidImageRef(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	for _, ref := range []string{
		"ubuntu",
		"ubuntu:24.04",
		"library/ubuntu:24.04",
		"docker.io/library/ubuntu",
		"ghcr.io/org/app:v1.2",
		"localhost:5000/app",
		"fedora@" + digest,
		"quay.io/org/app:tag@" + digest,
		"my_org/my-app__x",
	} {
		if !ValidImageRef(ref) {
			t.Errorf("ValidImageRef(%q) = false, want true", ref)
		}
	}
	for _, ref := range []string{
		"",
		"-ubuntu",
		"ubuntu:",
		"ubuntu:-tag",
		"ubuntu/../etc",
		"ubuntu;rm",
		"ubuntu@sha512:" + strings.Repeat("a", 128),
		strings.Repeat("a", 256),
	} {
		if ValidImageRef(ref) {
			t.Errorf("ValidImageRef(%q) = true, want false", ref)
		}
	}
}

// a from: image is keyed on its converted digest, so a moved tag (a new
// digest under the same ref) gets new layer keys, and until the digest is
// known nothing is cacheable at all.
func TestLayerKeysFromImageUseDigest(t *testing.T) {
	f := &Fusefile{Version: 1, From: "ubuntu:24.04", Cache: Cache{Enabled: true}, Build: []Step{{Run: "a"}}}

	unconverted, err := LayerKeys(f, nil, LayerOptions{})
	if err != nil {
		t.Fatalf("LayerKeys: %v", err)
	}
	if unconverted[0].Cacheable || unconverted[0].Reason != ReasonImageUnconverted {
		t.Fatalf("step 0 = %+v, want uncacheable with %q", unconverted[0], ReasonImageUnconverted)
	}

	// the inputs are checked before the digest is known, so a bad entry
	// fails before the conversion is waited on.
	withInputs := &Fusefile{Version: 1, From: "ubuntu:24.04", Cache: Cache{Enabled: true},
		Build: []Step{{Run: "a", Inputs: []string{"missing.txt"}}}}
	bad := func([]string) (string, error) { return "", errors.New("no such file") }
	if _, err := LayerKeys(withInputs, bad, LayerOptions{}); err == nil || !strings.Contains(err.Error(), "inputs") {
		t.Fatalf("LayerKeys with a bad input before conversion: err = %v, want an inputs error", err)
	}

	a, err := LayerKeys(f, nil, LayerOptions{ImageDigest: strings.Repeat("a", 64)})
	if err != nil {
		t.Fatalf("LayerKeys: %v", err)
	}
	b, err := LayerKeys(f, nil, LayerOptions{ImageDigest: strings.Repeat("b", 64)})
	if err != nil {
		t.Fatalf("LayerKeys: %v", err)
	}
	if !a[0].Cacheable || !b[0].Cacheable {
		t.Fatalf("keys not cacheable: %+v %+v", a[0], b[0])
	}
	if a[0].Key == b[0].Key {
		t.Fatalf("two digests produced the same key %q", a[0].Key)
	}
	if a[0].ParentKey != ImageBaseKey(strings.Repeat("a", 64), nil) {
		t.Fatalf("parent = %q, want the image base key", a[0].ParentKey)
	}
	// a digest must not collide with an image: name of the same text.
	if ImageBaseKey("x", nil) == BaseKey("x", nil) {
		t.Fatal("ImageBaseKey and BaseKey collide")
	}
}

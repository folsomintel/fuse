package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// ImageState is where a host is with converting an oci image to a rootfs.
type ImageState string

const (
	// ImageConverting means the host is running the bake script.
	ImageConverting ImageState = "converting"
	// ImageReady means the rootfs is on the host and a create can boot it.
	ImageReady ImageState = "ready"
	// ImageFailed means the bake failed; Image.Error says why. converting
	// the same ref again retries it.
	ImageFailed ImageState = "failed"
)

// Image is one oci image converted, or being converted, on one host.
//
// Digest is the cache key: a hash of the image's manifest digest for the
// host's arch and the digest of the fused binary baked into it, so the same
// ref on two hosts with different fused builds is two images. Name is the
// rootfs the host resolves it to (spec.Image at create).
//
// CreatedAt is when the host started the current conversion attempt, as the
// agent wrote it: a retry of a failed image is a new attempt with a new time.
// "" from an agent that predates the field.
type Image struct {
	Digest         string     `json:"digest"`
	Ref            string     `json:"ref"`
	ManifestDigest string     `json:"manifest_digest"`
	Name           string     `json:"name"`
	HostID         string     `json:"host_id"`
	State          ImageState `json:"state"`
	Error          string     `json:"error,omitempty"`
	CreatedAt      string     `json:"created_at,omitempty"`
}

// ImageConverter is implemented by a provider whose host agent can convert
// oci images. Only firecracker hosts can; qemu hosts boot qcow2 images and
// have no conversion.
type ImageConverter interface {
	// ConvertImage resolves ref to its manifest for the host's arch and
	// starts converting it in the background, or returns the existing record
	// when that digest is already converting or ready.
	ConvertImage(ctx context.Context, ref string) (Image, error)
	// GetImage returns the host's record for digest. a host without one
	// answers 404 (see IsNotFound).
	GetImage(ctx context.Context, digest string) (Image, error)
}

var (
	// ErrInvalidImage is a malformed ref or digest, or a ref the host could
	// not use (no manifest for its arch, a private registry). maps to 400.
	ErrInvalidImage = errors.New("invalid image")
	// ErrImageNotFound is a digest no host has a record for, or a ref the
	// registry does not know. maps to 404.
	ErrImageNotFound = errors.New("image not found")
	// ErrImageNotReady is a create naming a digest that is not ready on any
	// host it could land on. maps to 409.
	ErrImageNotReady = errors.New("image is not ready")
	// ErrImageUnsupported is a convert pinned to a host that cannot convert
	// (a qemu host). maps to 400.
	ErrImageUnsupported = errors.New("oci images are not supported on this host")
	// ErrImageHostFailed is a host that could not be asked, or failed in a
	// way that is not the caller's fault (registry unreachable, fused
	// missing). maps to 503.
	ErrImageHostFailed = errors.New("image host failed")
	// ErrImageCacheFull is a convert the host has no room for: its converted
	// images are at FC_IMAGE_CACHE_GB and every one a new bake could evict is
	// converting or used by a vm or snapshot. maps to 507.
	ErrImageCacheFull = errors.New("image cache full")
)

// imageDigestPattern is the form of Image.Digest: a bare hex sha256, the
// same shape the artifact store uses.
var imageDigestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ValidImageDigest reports whether s is a well-formed image digest.
func ValidImageDigest(s string) bool {
	return imageDigestPattern.MatchString(s)
}

// imageCallTimeout bounds one call to a host agent's image endpoints. a
// convert call resolves the ref against its registry before it returns, so
// it gets more room than a status read.
const (
	imageConvertTimeout = 60 * time.Second
	imageGetTimeout     = 10 * time.Second
)

// ConvertImageRequest asks for ref to be converted on a firecracker host.
// HostID, Region and Labels narrow which host, the same gates a create
// applies, so a caller that will pin the create can convert where it will
// land.
type ConvertImageRequest struct {
	Ref    string            `json:"ref"`
	HostID string            `json:"host_id,omitempty"`
	Region string            `json:"region,omitempty"`
	Labels map[string]string `json:"labels,omitempty"`
}

// ConvertImage picks a firecracker host for req and starts the conversion
// there: through restate when it is configured (see SetDurableImages), in
// this process otherwise.
func (fm *FleetManager) ConvertImage(ctx context.Context, req ConvertImageRequest) (Image, error) {
	if d := fm.durableImages(); d != nil {
		ctx, cancel := durableWait(ctx)
		defer cancel()
		return d.ConvertImage(ctx, req)
	}
	return fm.ImageConvert(ctx, req)
}

// ImageConvert is the plain step a convert runs: one agent call, no polling,
// and safe to repeat because the agent answers a repeat with the record it
// already has.
func (fm *FleetManager) ImageConvert(ctx context.Context, req ConvertImageRequest) (Image, error) {
	hostID, conv, err := fm.imageConvertHost(req)
	if err != nil {
		return Image{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, imageConvertTimeout)
	defer cancel()
	img, err := conv.ConvertImage(ctx, req.Ref)
	if err != nil {
		return Image{}, imageHostError(hostID, err)
	}
	img.HostID = hostID
	fm.logger.Info("image convert", "ref", req.Ref, "digest", img.Digest, "host", hostID, "state", img.State)
	return img, nil
}

// GetImage returns the best record any firecracker host holds for digest:
// ready over converting over failed. the hosts are the source of truth, so
// this asks each of them rather than reading a copy of its own.
func (fm *FleetManager) GetImage(ctx context.Context, digest string) (Image, error) {
	if !ValidImageDigest(digest) {
		return Image{}, fmt.Errorf("%w: digest %q is not a hex sha256", ErrInvalidImage, digest)
	}
	found, unreachable := fm.imageRecords(ctx, digest, fm.imageHosts(""))
	if len(found) == 0 {
		if unreachable > 0 {
			return Image{}, fmt.Errorf("%w: %s (%d host(s) could not be asked)", ErrImageNotFound, digest, unreachable)
		}
		return Image{}, fmt.Errorf("%w: %s", ErrImageNotFound, digest)
	}
	return found[0], nil
}

// resolveImage turns a create's spec.ImageDigest into the rootfs to boot and
// the host to boot it on: a host where the image is ready. a spec pinned
// with HostID only looks there. it is a no-op for a spec without a digest,
// and for one that seeds from a snapshot, whose rootfs already replaces the
// image.
func (fm *FleetManager) resolveImage(ctx context.Context, spec Spec) (Spec, error) {
	if spec.ImageDigest == "" || spec.SeedSnapshotID != "" {
		return spec, nil
	}
	if !ValidImageDigest(spec.ImageDigest) {
		return spec, fmt.Errorf("%w: spec.image_digest %q is not a hex sha256", ErrInvalidImage, spec.ImageDigest)
	}
	found, _ := fm.imageRecords(ctx, spec.ImageDigest, fm.imageHosts(spec.HostID))
	var ready []Image
	for _, img := range found {
		if img.State == ImageReady {
			ready = append(ready, img)
		}
	}
	if len(ready) == 0 {
		where := "any active firecracker host"
		if spec.HostID != "" {
			where = "host " + spec.HostID
		}
		if len(found) == 0 {
			return spec, fmt.Errorf("%w: %s has not been converted on %s; convert it first (POST /v1/images)",
				ErrImageNotReady, spec.ImageDigest, where)
		}
		best := found[0]
		detail := fmt.Sprintf("%s is %s on host %s", spec.ImageDigest, best.State, best.HostID)
		if best.Error != "" {
			detail += ": " + best.Error
		}
		return spec, fmt.Errorf("%w: %s", ErrImageNotReady, detail)
	}

	// ready on more than one host: let the scheduler pick among them, so the
	// pin lands where the workload fits. if none fits, pin the first anyway
	// and let placement report the shortfall the way it does for any create.
	pick := ready[0]
	if len(ready) > 1 {
		ids := make(map[string]Image, len(ready))
		for _, img := range ready {
			ids[img.HostID] = img
		}
		fm.mu.RLock()
		var hosts []*Host
		for _, h := range fm.activeHostsLocked() {
			if _, ok := ids[h.ID]; ok {
				hosts = append(hosts, h)
			}
		}
		fm.mu.RUnlock()
		if h, _, err := Schedule(spec, hosts, fm.placementPolicy); err == nil {
			pick = ids[h.ID]
		}
	}
	spec.Image = pick.Name
	spec.PinnedHostID = pick.HostID
	return spec, nil
}

// imageHosts returns the ids of the active firecracker hosts an image may
// live on, sorted, narrowed to pin when it is set.
func (fm *FleetManager) imageHosts(pin string) []string {
	fm.mu.RLock()
	defer fm.mu.RUnlock()
	var out []string
	for _, h := range fm.hosts {
		if !servesImages(h) || (pin != "" && h.ID != pin) {
			continue
		}
		out = append(out, h.ID)
	}
	sort.Strings(out)
	return out
}

// imageRecords asks each host for its record of digest, in parallel, and
// returns the ones that have it, best first, plus how many hosts errored.
func (fm *FleetManager) imageRecords(ctx context.Context, digest string, hostIDs []string) ([]Image, int) {
	type result struct {
		img Image
		ok  bool
		err bool
	}
	results := make([]result, len(hostIDs))
	var wg sync.WaitGroup
	for i, id := range hostIDs {
		fm.mu.RLock()
		p, ok := fm.providerForHost(id)
		fm.mu.RUnlock()
		conv, isConv := p.(ImageConverter)
		if !ok || !isConv {
			continue
		}
		wg.Add(1)
		go func(i int, id string, conv ImageConverter) {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, imageGetTimeout)
			defer cancel()
			img, err := conv.GetImage(cctx, digest)
			switch {
			case err == nil:
				img.HostID = id
				results[i] = result{img: img, ok: true}
			case !IsNotFound(err):
				fm.logger.Warn("image status failed", "digest", digest, "host", id, "err", err)
				results[i] = result{err: true}
			}
		}(i, id, conv)
	}
	wg.Wait()

	var found []Image
	unreachable := 0
	for _, r := range results {
		if r.ok {
			found = append(found, r.img)
		}
		if r.err {
			unreachable++
		}
	}
	rank := map[ImageState]int{ImageReady: 0, ImageConverting: 1, ImageFailed: 2}
	sort.SliceStable(found, func(i, j int) bool { return rank[found[i].State] < rank[found[j].State] })
	return found, unreachable
}

// imageConvertHost picks the host a convert runs on: the pinned one, or one
// of the active firecracker hosts in the region and matching the labels. the pick hashes the
// ref over the sorted candidates, so the same ref keeps landing on the same
// host (and finds its earlier conversion there) while different refs spread.
func (fm *FleetManager) imageConvertHost(req ConvertImageRequest) (string, ImageConverter, error) {
	fm.mu.RLock()
	defer fm.mu.RUnlock()
	if len(fm.hosts) == 0 {
		return "", nil, fmt.Errorf("%w: converting an oci image needs a registered firecracker host", ErrNoHosts)
	}
	if req.HostID != "" {
		h, ok := fm.hosts[req.HostID]
		if !ok {
			return "", nil, fmt.Errorf("%w: host_id %q is not registered", ErrHostNotFound, req.HostID)
		}
		if !isFirecracker(h) {
			return "", nil, fmt.Errorf("%w: host %s runs %s; oci images convert only on firecracker hosts", ErrImageUnsupported, h.ID, h.Backend)
		}
	}
	var candidates []*Host
	for _, h := range fm.hosts {
		if servesImages(h) && (req.HostID == "" || h.ID == req.HostID) &&
			(req.Region == "" || h.Region == req.Region) && labelsMatch(h.Labels, req.Labels) {
			candidates = append(candidates, h)
		}
	}
	if len(candidates) == 0 {
		return "", nil, fmt.Errorf("%w: no active firecracker host matches the request", ErrNoCapacity)
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].ID < candidates[j].ID })
	sum := fnv.New32a()
	_, _ = sum.Write([]byte(req.Ref))
	h := candidates[int(sum.Sum32()%uint32(len(candidates)))]
	p, ok := fm.providerForHost(h.ID)
	conv, isConv := p.(ImageConverter)
	if !ok || !isConv {
		return "", nil, fmt.Errorf("%w: host %s has no image-capable provider", ErrImageUnsupported, h.ID)
	}
	return h.ID, conv, nil
}

// isFirecracker reports whether h runs the firecracker backend. an empty
// backend is a record that predates the field, which means firecracker.
func isFirecracker(h *Host) bool {
	return h.Backend == "" || h.Backend == BackendFirecracker
}

// servesImages reports whether h can hold converted images and take a
// create from one: an active firecracker host.
func servesImages(h *Host) bool {
	return isFirecracker(h) && h.schedulable()
}

// imageHostError turns an agent's answer to a convert into a sentinel the
// api maps to a status: the agent's 4xx are the caller's problem (a bad ref,
// an unknown repository), everything else is the host's.
func imageHostError(hostID string, err error) error {
	var statusErr *HTTPStatusError
	if errors.As(err, &statusErr) {
		body := strings.TrimSpace(statusErr.Body)
		switch statusErr.Code {
		case http.StatusBadRequest, http.StatusUnprocessableEntity:
			return fmt.Errorf("%w: %s", ErrInvalidImage, body)
		case http.StatusNotFound:
			return fmt.Errorf("%w: %s", ErrImageNotFound, body)
		case http.StatusNotImplemented:
			return fmt.Errorf("%w: host %s: %s", ErrImageUnsupported, hostID, body)
		case http.StatusInsufficientStorage:
			return fmt.Errorf("%w: host %s: %s", ErrImageCacheFull, hostID, body)
		}
	}
	if errors.Is(err, ErrImageUnsupported) {
		return err
	}
	return fmt.Errorf("%w: host %s: %v", ErrImageHostFailed, hostID, err)
}

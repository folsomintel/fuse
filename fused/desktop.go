package main

// The desktop geometry declared in the Fusefile reaches the guest as
// /fuse/desktop.json, uploaded by the orchestrator before this agent is
// (re)started — the same file-not-flag channel the healthcheck config uses
// (see health.go for why).
//
// The display, though, boots before that upload happens: the image's
// fuse-display unit starts Xvfb at the baked default geometry as soon as the
// VM is up, and only then does the orchestrator upload files and start this
// agent. So fused closes the loop: at startup it compares the declared
// geometry with the live display and restarts the display units on a
// mismatch. The display runner itself prefers /fuse/desktop.json when it
// exists, so the restarted Xvfb comes up at the declared geometry.
//
// The window manager closes the same loop: fuse-wm-run starts the declared
// wm (falling back to the baked default when the image lacks it) and records
// the one it started in liveWMPath, which fused compares against.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"
)

// liveWMPath is where fuse-wm-run records the window manager it started. It
// sits in fuse-wm's RuntimeDirectory, so it disappears when the unit stops.
const liveWMPath = "/run/fuse-desktop/wm"

// liveWM reads the running window manager, empty when none is recorded.
func liveWM() string {
	raw, err := os.ReadFile(liveWMPath)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// wmInstalled reports whether the image carries the named window manager.
// every supported wm installs its binary under the same name.
func wmInstalled(wm string) bool {
	_, err := os.Stat("/usr/bin/" + wm)
	return err == nil
}

// desktopSpec mirrors orchestrator.DesktopSpec field for field; that struct
// is marshalled straight into the file, so the two shapes are the same shape
// by construction.
type desktopSpec struct {
	Width  int    `json:"width"`
	Height int    `json:"height"`
	WM     string `json:"wm,omitempty"`
}

// loadDesktopSpec reads the declared geometry. A missing file is the ordinary
// "no desktop declared" case and returns nil; a file that exists but does not
// parse is an error the caller reports.
func loadDesktopSpec(path string) (*desktopSpec, error) {
	if path == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read desktop %s: %w", path, err)
	}
	var spec desktopSpec
	if err := json.Unmarshal(raw, &spec); err != nil {
		return nil, fmt.Errorf("parse desktop %s: %w", path, err)
	}
	if spec.Width <= 0 || spec.Height <= 0 {
		return nil, fmt.Errorf("desktop %s: width and height must be positive, got %dx%d", path, spec.Width, spec.Height)
	}
	return &spec, nil
}

// needsDisplayRestart reports whether the live geometry disagrees with the
// declared one.
func needsDisplayRestart(spec *desktopSpec, liveW, liveH int) bool {
	return spec != nil && (liveW != spec.Width || liveH != spec.Height)
}

// needsWMRestart reports whether the running window manager disagrees with
// the declared one. An empty declaration keeps whatever the image started.
func needsWMRestart(spec *desktopSpec, live string) bool {
	return spec != nil && spec.WM != "" && spec.WM != live
}

// applyDesktop reconciles the live display with the declared geometry, best
// effort. Unlike a broken healthcheck config this is never fatal: killing the
// agent would take exec and every other route down with it, and the failure
// mode being avoided — a display at the wrong geometry — is visible to any
// caller through /v1/computer/display.
func applyDesktop(path, display string) {
	spec, err := loadDesktopSpec(path)
	if err != nil {
		log.Printf("WARNING: %v; display keeps the image default geometry", err)
		return
	}
	if spec == nil {
		return
	}

	comp := newComputer(display)
	if err := comp.ready(); err != nil {
		log.Printf("WARNING: desktop %dx%d declared but %v", spec.Width, spec.Height, err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	w, h, err := comp.geometry(ctx)
	if err != nil {
		log.Printf("WARNING: cannot read display geometry: %v", err)
		return
	}
	if !needsDisplayRestart(spec, w, h) {
		applyWM(spec)
		return
	}

	// the display runner reads the declared geometry from the same file this
	// agent just did, so a restart is all it takes. the wm, panel, and vnc
	// server are restarted with it: they die with their display and systemd
	// does not bring a Requires= dependent back on its own.
	log.Printf("display is %dx%d, desktop declares %dx%d; restarting display units", w, h, spec.Width, spec.Height)
	restartCtx, cancelRestart := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancelRestart()
	cmd := exec.CommandContext(restartCtx, "systemctl", "restart",
		"fuse-display.service", "fuse-wm.service", "fuse-panel.service", "fuse-vnc.service")
	if out, err := cmd.CombinedOutput(); err != nil {
		log.Printf("WARNING: display restart failed: %v (%s)", err, string(out))
	}
}

// applyWM swaps the window manager when the declared one is not the one
// running. A full display restart already restarts fuse-wm, which reads the
// declared wm itself, so this only runs when the geometry was right.
func applyWM(spec *desktopSpec) {
	live := liveWM()
	if !needsWMRestart(spec, live) {
		return
	}
	// restarting into a wm the image lacks would only fall back to the
	// default again, so say so instead
	if !wmInstalled(spec.WM) {
		log.Printf("WARNING: desktop declares wm %q but this image does not carry it; keeping %q", spec.WM, live)
		return
	}
	log.Printf("wm is %q, desktop declares %q; restarting fuse-wm", live, spec.WM)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "systemctl", "restart", "fuse-wm.service")
	if out, err := cmd.CombinedOutput(); err != nil {
		log.Printf("WARNING: wm restart failed: %v (%s)", err, string(out))
	}
}

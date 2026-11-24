package main

// GET /v1/doctor is the guest's self-check: one report that says which of
// the pieces fused depends on is working, so an operator can ask "what is
// wrong with this environment" without exec-ing in and poking around.
//
// It differs from the two other health signals on purpose. /health answers
// "is the agent up" and stays 200 whatever else is broken. The healthcheck
// probe (health.go) answers "is the author's workload serving". doctor
// answers "is the sandbox itself in shape": the display and its vnc server
// on a desktop image, the author's probe if one was declared, and the disk
// and memory everything else runs on.
//
// The route always answers 200 with a report, even when every check fails:
// the report is the answer, and a non-2xx would make the control plane relay
// an error envelope instead of the detail the caller asked for.

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Check outcomes, ordered by severity so the overall verdict is the worst
// one seen. skip means the check does not apply (no desktop, no probe) and
// never drags the verdict down.
const (
	checkOK   = "ok"
	checkSkip = "skip"
	checkWarn = "warn"
	checkFail = "fail"
)

var checkRank = map[string]int{checkOK: 0, checkSkip: 0, checkWarn: 1, checkFail: 2}

const (
	// doctorTimeout bounds the whole report; every check is local, so
	// anything slower is itself a symptom.
	doctorTimeout = 10 * time.Second
	// doctorDialTimeout bounds the vnc dial.
	doctorDialTimeout = time.Second

	// below these fractions free, disk and memory warn and then fail.
	lowFreeWarn = 0.10
	lowFreeFail = 0.02
)

// startedAt is when this agent process started, for the agent check's
// uptime.
var startedAt = time.Now()

// doctorCheck is one line of the report.
type doctorCheck struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

// doctorReport is the body of GET /v1/doctor.
type doctorReport struct {
	Status  string        `json:"status"`
	VMID    string        `json:"vm_id,omitempty"`
	Version string        `json:"version"`
	Checks  []doctorCheck `json:"checks"`
}

// doctor builds the report. Its inputs are fields so tests can drive every
// branch without an X server, a vnc server or a real /proc.
type doctor struct {
	vmID        string
	comp        *computer
	probe       *prober
	desktopPath string
	meminfoPath string
	rootPath    string
	// desktopImage reports whether the image carries the desktop tooling at
	// all; when it does not, the display checks are skipped, not failed.
	desktopImage func() bool
	liveWM       func() string
	dial         func(addr string) error
	diskUsage    func(path string) (free, total uint64, err error)
}

func newDoctor(c config, probe *prober) *doctor {
	return &doctor{
		vmID:         c.vmID,
		comp:         newComputer(c.display),
		probe:        probe,
		desktopPath:  c.desktop,
		meminfoPath:  "/proc/meminfo",
		rootPath:     "/",
		desktopImage: hasDesktopTools,
		liveWM:       liveWM,
		dial:         dialLocal,
		diskUsage:    statfsUsage,
	}
}

func hasDesktopTools() bool {
	for _, bin := range []string{"xdotool", "scrot"} {
		if _, err := exec.LookPath(bin); err != nil {
			return false
		}
	}
	return true
}

func dialLocal(addr string) error {
	conn, err := net.DialTimeout("tcp", addr, doctorDialTimeout)
	if err != nil {
		return err
	}
	return conn.Close()
}

// handle serves GET /v1/doctor.
func (d *doctor) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), doctorTimeout)
	defer cancel()
	writeJSON(w, http.StatusOK, d.report(ctx))
}

// report runs every check in a fixed order, so the output reads the same on
// every environment.
func (d *doctor) report(ctx context.Context) doctorReport {
	checks := []doctorCheck{d.checkAgent()}
	checks = append(checks, d.checkDesktop(ctx)...)
	checks = append(checks, d.checkHealthcheck(), d.checkDisk(), d.checkMemory())

	status := checkOK
	for _, c := range checks {
		if checkRank[c.Status] > checkRank[status] {
			status = c.Status
		}
	}
	return doctorReport{Status: status, VMID: d.vmID, Version: version, Checks: checks}
}

func (d *doctor) checkAgent() doctorCheck {
	up := time.Since(startedAt).Round(time.Second)
	return doctorCheck{Name: "agent", Status: checkOK, Message: fmt.Sprintf("fused %s, up %s", version, up)}
}

// checkDesktop reports the display, window manager and vnc server together:
// on an image without the desktop tooling all three are skipped, and with a
// dead display the other two cannot be judged.
func (d *doctor) checkDesktop(ctx context.Context) []doctorCheck {
	if !d.desktopImage() {
		msg := "not a desktop image"
		return []doctorCheck{
			{Name: "display", Status: checkSkip, Message: msg},
			{Name: "wm", Status: checkSkip, Message: msg},
			{Name: "vnc", Status: checkSkip, Message: msg},
		}
	}

	spec, specErr := loadDesktopSpec(d.desktopPath)
	display := d.checkDisplay(ctx, spec, specErr)
	if display.Status == checkFail {
		msg := "display is not up"
		return []doctorCheck{
			display,
			{Name: "wm", Status: checkSkip, Message: msg},
			{Name: "vnc", Status: checkSkip, Message: msg},
		}
	}
	return []doctorCheck{display, d.checkWM(spec), d.checkVNC()}
}

func (d *doctor) checkDisplay(ctx context.Context, spec *desktopSpec, specErr error) doctorCheck {
	c := doctorCheck{Name: "display"}
	if err := d.comp.ready(); err != nil {
		c.Status, c.Message = checkFail, err.Error()
		return c
	}
	w, h, err := d.comp.geometry(ctx)
	if err != nil {
		c.Status, c.Message = checkFail, "cannot read geometry: "+err.Error()
		return c
	}
	geom := fmt.Sprintf("%s %dx%d", d.comp.display, w, h)
	switch {
	case specErr != nil:
		c.Status, c.Message = checkWarn, geom+"; "+specErr.Error()
	case needsDisplayRestart(spec, w, h):
		c.Status, c.Message = checkWarn, fmt.Sprintf("%s, desktop declares %dx%d", geom, spec.Width, spec.Height)
	default:
		c.Status, c.Message = checkOK, geom
	}
	return c
}

func (d *doctor) checkWM(spec *desktopSpec) doctorCheck {
	c := doctorCheck{Name: "wm"}
	live := d.liveWM()
	switch {
	case live == "":
		c.Status, c.Message = checkWarn, "no window manager recorded at "+liveWMPath
	case needsWMRestart(spec, live):
		c.Status, c.Message = checkWarn, fmt.Sprintf("%s running, desktop declares %s", live, spec.WM)
	default:
		c.Status, c.Message = checkOK, live
	}
	return c
}

func (d *doctor) checkVNC() doctorCheck {
	c := doctorCheck{Name: "vnc"}
	if err := d.dial(d.comp.vncAddr); err != nil {
		c.Status, c.Message = checkFail, "vnc server not reachable at "+d.comp.vncAddr+": "+err.Error()
		return c
	}
	c.Status, c.Message = checkOK, "listening on "+d.comp.vncAddr
	return c
}

// checkHealthcheck folds the author's probe verdict in. starting is a warn,
// not a fail: the start period exists so a booting service is not reported
// broken.
func (d *doctor) checkHealthcheck() doctorCheck {
	c := doctorCheck{Name: "healthcheck"}
	if d.probe == nil {
		c.Status, c.Message = checkSkip, "no healthcheck declared"
		return c
	}
	s := d.probe.snapshot()
	switch s.State {
	case healthPassing:
		c.Status, c.Message = checkOK, "passing"
	case healthFailing:
		c.Status = checkFail
		c.Message = fmt.Sprintf("failing (%d in a row): %s", s.Failures, s.Message)
	default:
		c.Status, c.Message = checkWarn, "starting"
		if s.Message != "" {
			c.Message += ": " + s.Message
		}
	}
	return c
}

// statfsUsage returns the bytes available to unprivileged writers and the
// filesystem's size.
func statfsUsage(path string) (free, total uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, fmt.Errorf("statfs %s: %w", path, err)
	}
	bsize := uint64(st.Bsize) // #nosec G115 -- block size is never negative
	return st.Bavail * bsize, st.Blocks * bsize, nil
}

func (d *doctor) checkDisk() doctorCheck {
	c := doctorCheck{Name: "disk"}
	free, total, err := d.diskUsage(d.rootPath)
	if err != nil {
		c.Status, c.Message = checkWarn, err.Error()
		return c
	}
	c.Status = freeStatus(free, total)
	c.Message = fmt.Sprintf("%s free of %s on %s", humanBytes(free), humanBytes(total), d.rootPath)
	return c
}

func (d *doctor) checkMemory() doctorCheck {
	c := doctorCheck{Name: "memory"}
	total, avail, err := readMeminfo(d.meminfoPath)
	if err != nil {
		c.Status, c.Message = checkWarn, err.Error()
		return c
	}
	c.Status = freeStatus(avail, total)
	c.Message = fmt.Sprintf("%s available of %s", humanBytes(avail), humanBytes(total))
	return c
}

// freeStatus grades a free/total pair against the low-free thresholds.
func freeStatus(free, total uint64) string {
	if total == 0 {
		return checkWarn
	}
	frac := float64(free) / float64(total)
	switch {
	case frac < lowFreeFail:
		return checkFail
	case frac < lowFreeWarn:
		return checkWarn
	default:
		return checkOK
	}
}

// readMeminfo returns MemTotal and MemAvailable in bytes.
func readMeminfo(path string) (total, avail uint64, err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, fmt.Errorf("read %s: %w", path, err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		kb, perr := strconv.ParseUint(fields[1], 10, 64)
		if perr != nil {
			continue
		}
		switch fields[0] {
		case "MemTotal:":
			total = kb * 1024
		case "MemAvailable:":
			avail = kb * 1024
		}
	}
	if total == 0 {
		return 0, 0, fmt.Errorf("%s has no MemTotal", path)
	}
	return total, avail, nil
}

func humanBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := uint64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

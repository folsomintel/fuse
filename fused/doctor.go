package main

// GET /v1/doctor is the guest's self-check: one report that says which of
// the pieces fused depends on is working, so an operator can ask "what is
// wrong with this environment" without exec-ing in and poking around.
//
// It differs from the two other health signals on purpose. /health answers
// "is the agent up" and stays 200 whatever else is broken. The healthcheck
// probe (health.go) answers "is the author's workload serving". doctor
// answers "is the sandbox itself in shape": identity, the display and each
// thing it needs, egress, the clock, and the disk and memory everything else
// runs on.
//
// Every check runs every time; one failing never stops the rest. The route
// always answers 200 with a report, even when every check fails: the report
// is the answer, and a non-2xx would make the control plane relay an error
// envelope instead of the detail the caller asked for.

import (
	"context"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Check outcomes, ordered by severity so the overall verdict is the worst
// one seen. skip means the check does not apply (no desktop, no probe) and
// never drags the verdict down.
const (
	checkPass = "pass"
	checkSkip = "skip"
	checkWarn = "warn"
	checkFail = "fail"
)

var checkRank = map[string]int{checkPass: 0, checkSkip: 0, checkWarn: 1, checkFail: 2}

const (
	// doctorTimeout bounds the whole report; the slowest check is the
	// egress round trip, and anything slower than this is itself a symptom.
	doctorTimeout = 10 * time.Second
	// doctorDialTimeout bounds the vnc dial.
	doctorDialTimeout = time.Second
	// doctorNetTimeout bounds the dns lookup and the egress request each.
	doctorNetTimeout = 3 * time.Second

	// doctorProbeHost is resolved and fetched to check dns, egress and the
	// clock. example.com is reserved by IANA for exactly this kind of use
	// and serves a Date header over plain http.
	doctorProbeHost = "example.com"

	// below these fractions free, disk and memory warn and then fail.
	lowFreeWarn = 0.10
	lowFreeFail = 0.02

	// clock skew beyond these warns, then fails: past a few minutes, token
	// expiry and tls validity checks start to break.
	skewWarn = 5 * time.Second
	skewFail = 5 * time.Minute
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
// branch without an X server, a network, or a real /proc.
type doctor struct {
	vmID          string
	comp          *computer
	probe         *prober
	desktopPath   string
	egressPath    string
	meminfoPath   string
	machineIDPath string
	dbusPath      string
	rootPath      string

	liveWM    func() string
	dial      func(addr string) error
	diskUsage func(path string) (free, total uint64, err error)
	lookup    func(ctx context.Context, host string) error
	// fetch makes one GET through proxy (nil for direct) and returns the
	// response's Date header.
	fetch func(ctx context.Context, target string, proxy *url.URL) (time.Time, error)
	now   func() time.Time
	// offline skips dns, egress and clock. The boot-time run sets it, so
	// starting a sandbox (or forking a hundred) sends nothing off the guest;
	// the network checks run only when someone asks.
	offline bool
}

func newDoctor(c config, probe *prober) *doctor {
	return &doctor{
		vmID:          c.vmID,
		comp:          newComputer(c.display),
		probe:         probe,
		desktopPath:   c.desktop,
		egressPath:    c.egress,
		meminfoPath:   "/proc/meminfo",
		machineIDPath: "/etc/machine-id",
		dbusPath:      "/run/dbus/system_bus_socket",
		rootPath:      "/",
		liveWM:        liveWM,
		dial:          dialLocal,
		diskUsage:     statfsUsage,
		lookup:        lookupHost,
		fetch:         fetchDate,
		now:           time.Now,
	}
}

func dialLocal(addr string) error {
	conn, err := net.DialTimeout("tcp", addr, doctorDialTimeout)
	if err != nil {
		return err
	}
	return conn.Close()
}

func lookupHost(ctx context.Context, host string) error {
	_, err := net.DefaultResolver.LookupHost(ctx, host)
	return err
}

// fetchDate GETs target and returns its Date header. Any status counts as
// reaching the internet; only a transport failure or a missing header is an
// error.
func fetchDate(ctx context.Context, target string, proxy *url.URL) (time.Time, error) {
	tr := &http.Transport{Proxy: nil}
	if proxy != nil {
		tr.Proxy = http.ProxyURL(proxy)
	}
	defer tr.CloseIdleConnections()
	client := &http.Client{
		Transport: tr,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return time.Time{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return time.Time{}, err
	}
	_ = resp.Body.Close()
	date, err := http.ParseTime(resp.Header.Get("Date"))
	if err != nil {
		return time.Time{}, fmt.Errorf("no usable Date header from %s", target)
	}
	return date, nil
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

// logReport runs the checks once and writes one line each to the journal,
// so a guest that boots broken says so without anyone asking. It runs
// offline: see doctor.offline.
func (d *doctor) logReport() {
	d.offline = true
	ctx, cancel := context.WithTimeout(context.Background(), doctorTimeout)
	defer cancel()
	r := d.report(ctx)
	for _, c := range r.Checks {
		log.Printf("doctor: %s", doctorLine(c))
	}
	log.Printf("doctor: %s", strings.ToUpper(r.Status))
}

// doctorLine renders a check as `PASS name: detail`.
func doctorLine(c doctorCheck) string {
	line := strings.ToUpper(c.Status) + " " + c.Name
	if c.Message != "" {
		line += ": " + c.Message
	}
	return line
}

// report runs every check in a fixed order, so the output reads the same on
// every environment.
func (d *doctor) report(ctx context.Context) doctorReport {
	checks := []doctorCheck{d.checkAgent(), d.checkMachineID(), d.checkDBus(), d.checkDisk(), d.checkMemory()}
	checks = append(checks, d.checkDesktop(ctx)...)
	checks = append(checks, d.checkHealthcheck())
	checks = append(checks, d.checkNetwork(ctx)...)

	status := checkPass
	for _, c := range checks {
		if checkRank[c.Status] > checkRank[status] {
			status = c.Status
		}
	}
	return doctorReport{Status: status, VMID: d.vmID, Version: version, Checks: checks}
}

func (d *doctor) checkAgent() doctorCheck {
	up := time.Since(startedAt).Round(time.Second)
	return doctorCheck{Name: "agent", Status: checkPass, Message: fmt.Sprintf("fused %s, up %s", version, up)}
}

// checkMachineID reports the guest's machine id. It is printed in full
// because the useful comparison is across environments: two that report the
// same id share an identity that journald, dbus and dhcp all key on.
func (d *doctor) checkMachineID() doctorCheck {
	c := doctorCheck{Name: "machine-id"}
	raw, err := os.ReadFile(d.machineIDPath)
	if err != nil {
		c.Status, c.Message = checkFail, "read "+d.machineIDPath+": "+err.Error()
		return c
	}
	id := strings.TrimSpace(string(raw))
	if _, err := hex.DecodeString(id); err != nil || len(id) != 32 {
		c.Status, c.Message = checkFail, fmt.Sprintf("%s holds %q, want 32 hex characters", d.machineIDPath, id)
		return c
	}
	c.Status, c.Message = checkPass, id
	return c
}

// checkDBus is a warn rather than a fail when the bus is absent: the baked
// images all run one, but a bring-your-own image can work without it.
func (d *doctor) checkDBus() doctorCheck {
	c := doctorCheck{Name: "dbus"}
	if _, err := os.Stat(d.dbusPath); err != nil {
		c.Status, c.Message = checkWarn, "no system bus at "+d.dbusPath
		return c
	}
	c.Status, c.Message = checkPass, "system bus at "+d.dbusPath
	return c
}

// checkDesktop reports each piece the display needs, then the window manager
// and vnc server. An image with none of the pieces is not a desktop image and
// every line is a skip; with only some of them, each missing one fails.
func (d *doctor) checkDesktop(ctx context.Context) []doctorCheck {
	pieces := d.comp.pieces()
	present := 0
	for _, p := range pieces {
		if p.err == nil {
			present++
		}
	}
	names := make([]string, 0, len(pieces)+2)
	for _, p := range pieces {
		names = append(names, p.name)
	}
	names = append(names, "wm", "vnc")
	if present == 0 {
		return skipAll(names, "not a desktop image")
	}

	out := make([]doctorCheck, 0, len(names))
	displayUp := true
	for _, p := range pieces {
		c := doctorCheck{Name: p.name}
		switch {
		case p.err != nil:
			c.Status, c.Message = checkFail, p.err.Error()
			displayUp = false
		case p.name == "display":
			c = d.checkGeometry(ctx)
			displayUp = c.Status != checkFail
		default:
			c.Status, c.Message = checkPass, "installed"
		}
		out = append(out, c)
	}
	if !displayUp {
		return append(out, skipAll([]string{"wm", "vnc"}, "display is not up")...)
	}
	spec, _ := loadDesktopSpec(d.desktopPath)
	return append(out, d.checkWM(spec), d.checkVNC())
}

func skipAll(names []string, why string) []doctorCheck {
	out := make([]doctorCheck, 0, len(names))
	for _, n := range names {
		out = append(out, doctorCheck{Name: n, Status: checkSkip, Message: why})
	}
	return out
}

// checkGeometry reads the live geometry and compares it with the declared
// desktop block.
func (d *doctor) checkGeometry(ctx context.Context) doctorCheck {
	c := doctorCheck{Name: "display"}
	w, h, err := d.comp.geometry(ctx)
	if err != nil {
		c.Status, c.Message = checkFail, "cannot read geometry: "+err.Error()
		return c
	}
	geom := fmt.Sprintf("%s %dx%d", d.comp.display, w, h)
	spec, specErr := loadDesktopSpec(d.desktopPath)
	switch {
	case specErr != nil:
		c.Status, c.Message = checkWarn, geom+"; "+specErr.Error()
	case needsDisplayRestart(spec, w, h):
		c.Status, c.Message = checkWarn, fmt.Sprintf("%s, desktop declares %dx%d", geom, spec.Width, spec.Height)
	default:
		c.Status, c.Message = checkPass, geom
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
		c.Status, c.Message = checkPass, live
	}
	return c
}

func (d *doctor) checkVNC() doctorCheck {
	c := doctorCheck{Name: "vnc"}
	if err := d.dial(d.comp.vncAddr); err != nil {
		c.Status, c.Message = checkFail, "vnc server not reachable at "+d.comp.vncAddr+": "+err.Error()
		return c
	}
	c.Status, c.Message = checkPass, "listening on "+d.comp.vncAddr
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
		c.Status, c.Message = checkPass, "passing"
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

// checkNetwork checks dns, egress and the clock, in that order, because each
// needs the one before it: the clock is read off the egress response's Date
// header.
//
// Under proxied egress the guest's own resolver is not on the path (names
// resolve at the proxy, and direct traffic is dropped on the host), so dns is
// a skip and the egress request goes through the proxy the workload uses.
func (d *doctor) checkNetwork(ctx context.Context) []doctorCheck {
	if d.offline {
		return skipAll([]string{"dns", "egress", "clock"}, "not run at boot")
	}
	proxy, proxyErr := egressProxy(d.egressPath)

	dns := doctorCheck{Name: "dns"}
	switch {
	case proxyErr != nil:
		dns.Status, dns.Message = checkSkip, proxyErr.Error()
	case proxy != nil:
		dns.Status, dns.Message = checkSkip, "egress is proxied; names resolve at the proxy"
	default:
		lctx, cancel := context.WithTimeout(ctx, doctorNetTimeout)
		err := d.lookup(lctx, doctorProbeHost)
		cancel()
		if err != nil {
			dns.Status, dns.Message = checkFail, "resolve "+doctorProbeHost+": "+err.Error()
		} else {
			dns.Status, dns.Message = checkPass, "resolved "+doctorProbeHost
		}
	}

	egress := doctorCheck{Name: "egress"}
	clock := doctorCheck{Name: "clock"}
	if proxyErr != nil {
		egress.Status, egress.Message = checkFail, proxyErr.Error()
		clock.Status, clock.Message = checkSkip, "egress is down"
		return []doctorCheck{dns, egress, clock}
	}
	via := "direct"
	if proxy != nil {
		via = "via " + proxy.Redacted()
	}
	fctx, cancel := context.WithTimeout(ctx, doctorNetTimeout)
	date, err := d.fetch(fctx, "http://"+doctorProbeHost+"/", proxy)
	cancel()
	if err != nil {
		egress.Status, egress.Message = checkFail, fmt.Sprintf("GET http://%s/ %s: %v", doctorProbeHost, via, err)
		clock.Status, clock.Message = checkSkip, "egress is down"
		return []doctorCheck{dns, egress, clock}
	}
	egress.Status, egress.Message = checkPass, fmt.Sprintf("reached %s %s", doctorProbeHost, via)

	// the Date header has one-second resolution, so a skew under a second
	// is noise, not a reading.
	skew := d.now().Sub(date)
	if skew < 0 {
		skew = -skew
	}
	clock.Message = fmt.Sprintf("%s off %s", skew.Round(time.Second), doctorProbeHost)
	switch {
	case skew > skewFail:
		clock.Status = checkFail
	case skew > skewWarn:
		clock.Status = checkWarn
	default:
		clock.Status = checkPass
	}
	return []doctorCheck{dns, egress, clock}
}

// egressProxy returns the proxy the workload's egress goes through, nil for
// direct egress or for an orchestrator that predates the egress file.
func egressProxy(path string) (*url.URL, error) {
	doc := readEgress(path)
	if doc == nil {
		return nil, nil
	}
	if _, bad := doc["error"]; bad {
		return nil, fmt.Errorf("%s is not valid json", path)
	}
	if mode, _ := doc["mode"].(string); mode != "proxy" {
		return nil, nil
	}
	endpoint, _ := doc["endpoint"].(string)
	if endpoint == "" {
		return nil, fmt.Errorf("egress is proxied but %s names no endpoint", path)
	}
	if !strings.Contains(endpoint, "://") {
		protocol, _ := doc["protocol"].(string)
		if protocol == "" {
			protocol = "socks5"
		}
		endpoint = protocol + "://" + endpoint
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("egress endpoint %q: %w", endpoint, err)
	}
	return u, nil
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
		return checkPass
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

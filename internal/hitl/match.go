package hitl

import (
	"path"
	"slices"
	"strings"
)

// ExecCall is an exec as the gates see it.
type ExecCall struct {
	// Argv is what the guest would run. a shell exec is ["sh", "-lc", script].
	Argv []string
	// Shell is true when the caller sent an opaque shell string.
	Shell bool
}

// Verdict is every gate that matched one action, split by mode.
type Verdict struct {
	// Deny is the first matching deny gate, nil when none matched.
	Deny *Gate
	// Ask are the matching ask gates, in declaration order.
	Ask []Gate
	// Notify are the matching notify gates, in declaration order.
	Notify []Gate
}

// Mode returns the strictest mode that matched, or "" when nothing did.
func (v Verdict) Mode() Mode {
	switch {
	case v.Deny != nil:
		return ModeDeny
	case len(v.Ask) > 0:
		return ModeAsk
	case len(v.Notify) > 0:
		return ModeNotify
	}
	return ""
}

func (v *Verdict) add(g Gate) {
	switch g.EffectiveMode() {
	case ModeDeny:
		if v.Deny == nil {
			v.Deny = &g
		}
	case ModeAsk:
		v.Ask = append(v.Ask, g)
	case ModeNotify:
		v.Notify = append(v.Notify, g)
	}
}

// EvaluateExec runs an exec through the spec's exec gates and allowlist.
func (s Spec) EvaluateExec(call ExecCall) Verdict {
	var v Verdict
	stripped := StripWrappers(call.Argv)
	shell := call.Shell || isShellC(stripped)
	for _, g := range s.Gates {
		if g.On == KindExec && g.Match.matchExec(stripped, shell) {
			v.add(g)
		}
	}
	if s.Exec != nil && !slices.Contains(s.Exec.Allow, Program(stripped)) {
		v.add(s.allowlistGate())
	}
	return v
}

// EvaluateLifecycle runs an environment action through the lifecycle gates.
func (s Spec) EvaluateLifecycle(action LifecycleAction) Verdict {
	var v Verdict
	for _, g := range s.Gates {
		if g.On == KindLifecycle && slices.Contains(g.Match.Action, action) {
			v.add(g)
		}
	}
	return v
}

// matchExec matches an already stripped argv. shell exec is opaque: only a
// shell gate sees it, never a program or args gate.
func (m GateMatch) matchExec(argv []string, shell bool) bool {
	if m.Shell {
		return shell
	}
	if shell || len(argv) == 0 {
		return false
	}
	if len(m.Program) > 0 && !slices.Contains(m.Program, Program(argv)) {
		return false
	}
	return isSubsequence(argv[1:], m.Args)
}

// isSubsequence reports whether want appears in have in order.
func isSubsequence(have, want []string) bool {
	i := 0
	for _, h := range have {
		if i < len(want) && h == want[i] {
			i++
		}
	}
	return i == len(want)
}

// Program returns the basename of argv[0], or "" for an empty argv.
func Program(argv []string) string {
	if len(argv) == 0 {
		return ""
	}
	return path.Base(argv[0])
}

var shells = []string{"sh", "bash", "dash", "zsh", "ash", "ksh", "fish"}

// isShellC reports whether argv runs a shell on a command string
// (bash -c, sh -lc), which is as opaque as a shell: exec.
func isShellC(argv []string) bool {
	if !slices.Contains(shells, Program(argv)) {
		return false
	}
	for _, a := range argv[1:] {
		if len(a) > 1 && a[0] == '-' && a[1] != '-' && strings.ContainsRune(a, 'c') {
			return true
		}
	}
	return false
}

// wrapper describes a program that runs another program: the options that
// take a value, as short letters and long names.
type wrapper struct {
	short string
	long  []string
	// positional is how many plain args come before the wrapped program
	// (timeout's duration).
	positional int
}

var wrappers = map[string]wrapper{
	"env":     {short: "uCS", long: []string{"--unset", "--chdir", "--split-string"}},
	"sudo":    {short: "ugCDhpRrtTU", long: []string{"--user", "--group", "--close-from", "--chdir", "--host", "--prompt", "--chroot", "--role", "--type", "--command-timeout", "--other-user"}},
	"nice":    {short: "n", long: []string{"--adjustment"}},
	"timeout": {short: "sk", long: []string{"--signal", "--kill-after"}, positional: 1},
	"xargs":   {short: "adEILnPs", long: []string{"--arg-file", "--delimiter", "--eof", "--replace", "--max-lines", "--max-args", "--max-procs", "--max-chars", "--process-slot-var"}},
	"nohup":   {},
	"stdbuf":  {short: "ioe", long: []string{"--input", "--output", "--error"}},
}

// maxWrapperDepth bounds stripping so a crafted argv cannot loop forever.
const maxWrapperDepth = 16

// StripWrappers removes known wrappers (env and its VAR=x args, sudo, nice,
// timeout, xargs, nohup, stdbuf) from the front of argv, so the program a
// gate sees is the one that actually runs. a wrapper with nothing after it
// is left alone: it is the program.
func StripWrappers(argv []string) []string {
	for range maxWrapperDepth {
		rest, ok := stripOne(argv)
		if !ok {
			break
		}
		argv = rest
	}
	return argv
}

func stripOne(argv []string) ([]string, bool) {
	if len(argv) < 2 {
		return argv, false
	}
	name := Program(argv)
	w, ok := wrappers[name]
	if !ok {
		return argv, false
	}
	args := argv[1:]
	var split []string
	i := 0
	for i < len(args) {
		a := args[i]
		if a == "--" {
			i++
			break
		}
		if name == "env" && isAssignment(a) {
			i++
			continue
		}
		if len(a) < 2 || a[0] != '-' {
			break
		}
		opt, value, takes := w.optionValue(a)
		if takes && value == "" && i+1 < len(args) {
			i++
			value = args[i]
		}
		// env -S "git push --force" carries the command in one string.
		if name == "env" && (opt == "-S" || opt == "--split-string") {
			split = strings.Fields(value)
		}
		i++
	}
	if name == "env" {
		for i < len(args) && isAssignment(args[i]) {
			i++
		}
	}
	for n := 0; n < w.positional && i < len(args); n++ {
		i++
	}
	rest := append(split, args[i:]...)
	if len(rest) == 0 {
		return argv, false
	}
	return rest, true
}

// optionValue parses one option. it returns the option name, an attached
// value (--opt=v, -nv) and whether the option takes a value at all.
func (w wrapper) optionValue(a string) (opt, value string, takes bool) {
	if strings.HasPrefix(a, "--") {
		opt, value, _ = strings.Cut(a, "=")
		return opt, value, slices.Contains(w.long, opt)
	}
	// a short cluster: the first letter that takes a value ends it, and
	// whatever follows that letter is the value.
	for j := 1; j < len(a); j++ {
		if strings.IndexByte(w.short, a[j]) >= 0 {
			return "-" + string(a[j]), a[j+1:], true
		}
	}
	return a, "", false
}

// isAssignment reports whether a is an env-style NAME=value.
func isAssignment(a string) bool {
	name, _, ok := strings.Cut(a, "=")
	if !ok || name == "" {
		return false
	}
	for i, r := range name {
		if r != '_' && (r < 'A' || r > 'Z') && (r < 'a' || r > 'z') && (i == 0 || r < '0' || r > '9') {
			return false
		}
	}
	return true
}

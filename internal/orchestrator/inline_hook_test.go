package orchestrator

import "sync"

// WireInlineDurable sets orchestratortest's in-process restate stand-in as
// all of fm's durable clients and returns its Wait. inline_wire_test.go sets
// it: this package's own tests cannot import orchestratortest, which imports
// this package.
var WireInlineDurable func(fm *FleetManager) (wait func())

// inlineFleetKey is orchestratortest.TestKey, the token key newInlineFleet
// gives a fleet that has none.
const inlineFleetKey = "inline-durable-test-key-32-bytes"

var (
	inlineWaitsMu sync.Mutex
	inlineWaits   = map[*FleetManager]func(){}
)

// newInlineFleet is NewFleetManager(cfg) with every durable client set to
// orchestratortest's stand-in.
func newInlineFleet(cfg FleetConfig) *FleetManager {
	if WireInlineDurable == nil {
		panic("orchestrator: WireInlineDurable is nil; inline_wire_test.go must set it")
	}
	if len(cfg.TokenEncryptionKey) == 0 {
		cfg.TokenEncryptionKey = []byte(inlineFleetKey)
	}
	fm := NewFleetManager(cfg)
	wait := WireInlineDurable(fm)
	inlineWaitsMu.Lock()
	inlineWaits[fm] = wait
	inlineWaitsMu.Unlock()
	return fm
}

// inlineWait blocks until the stand-in newInlineFleet gave fm has no one-way
// send or awaited invocation running.
func inlineWait(fm *FleetManager) {
	inlineWaitsMu.Lock()
	wait := inlineWaits[fm]
	inlineWaitsMu.Unlock()
	wait()
}

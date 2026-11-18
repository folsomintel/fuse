package orchestrator_test

import (
	"github.com/folsomintel/fuse/internal/orchestrator"
	"github.com/folsomintel/fuse/internal/orchestrator/orchestratortest"
)

func init() {
	orchestrator.WireInlineDurable = func(fm *orchestrator.FleetManager) func() {
		return orchestratortest.Wire(fm).Wait
	}
}

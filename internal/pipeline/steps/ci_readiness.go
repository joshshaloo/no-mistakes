package steps

import (
	"fmt"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

const defaultChecksGracePeriod = time.Minute

func (s *CIStep) gracePeriod() time.Duration {
	if s.checksGracePeriod != 0 {
		return s.checksGracePeriod
	}
	return defaultChecksGracePeriod
}

// Readiness failures are fatal: publishing a green log line after its durable
// write failed would leave AXI and subscribers with contradictory authority.
func (s *CIStep) logCIMonitorStatus(sctx *pipeline.StepContext, message, previous string) (string, error) {
	if message == previous {
		return message, nil
	}
	ready := message == ciChecksPassedMsg || message == ciNoChecksPassedMsg
	var err error
	if s.setRunCIReady != nil {
		err = s.setRunCIReady(sctx.Run.ID, ready)
	} else {
		err = setCIMonitorReadiness(sctx, ready, message == ciNoChecksPassedMsg)
	}
	if err != nil {
		return previous, fmt.Errorf("persist CI readiness %t: %w", ready, err)
	}
	sctx.Log(message)
	return message, nil
}

func (s *CIStep) clearCIMonitorReady(sctx *pipeline.StepContext) error {
	var err error
	if s.setRunCIReady != nil {
		err = s.setRunCIReady(sctx.Run.ID, false)
	} else {
		err = setCIMonitorReadiness(sctx, false, false)
	}
	if err != nil {
		return fmt.Errorf("clear durable CI readiness: %w", err)
	}
	return nil
}

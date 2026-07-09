package piko

import (
	"sync/atomic"
	"time"
)

func (p part) probeIdleTimeout() time.Duration {
	if !p.rateProbe {
		return 0
	}
	return rateLimitIdle
}

type rateProbeTimer struct {
	timer    *time.Timer
	timedOut atomic.Bool
}

func newRateProbeTimer(timeout time.Duration, onTimeout func()) *rateProbeTimer {
	if timeout <= 0 {
		return nil
	}
	probe := &rateProbeTimer{}
	probe.timer = time.AfterFunc(timeout, func() {
		probe.timedOut.Store(true)
		onTimeout()
	})
	return probe
}

func (p *rateProbeTimer) stop() {
	if p != nil {
		p.timer.Stop()
	}
}

func (p *rateProbeTimer) reset(timeout time.Duration) {
	if p != nil {
		resetTimer(p.timer, timeout)
	}
}

func (p *rateProbeTimer) expired() bool {
	return p != nil && p.timedOut.Load()
}

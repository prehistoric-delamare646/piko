package piko

import "time"

type rangeRetryPlan struct {
	maxRequeues int
	delay       time.Duration
}

func (d *downloader) planRangeRetry(scheduler *partScheduler, workerID int, p part, offset int64, partSize int64, err error) rangeRetryPlan {
	plan := rangeRetryPlan{maxRequeues: max(d.retries*4, 8)}
	switch {
	case isRateLimitedDownloadError(err):
		plan.maxRequeues = max(d.retries*16, 64)
		plan.delay = rateLimitDelay(p.requeues)
		scheduler.limitForRateLimit(plan.delay)
		if p.end-offset+1 <= max(partSize, int64(minDynamicPartSize*2)) {
			plan.delay = 0
		}
	case isRateProbeTimeout(err):
		plan.maxRequeues = max(d.retries*16, 64)
		scheduler.rejectRateProbe(rateLimitRecover)
	case isTransientRangeError(err):
		plan.maxRequeues = max(d.retries*24, 96)
		if p.requeues >= d.retries {
			plan.delay = retryDelay(p.requeues - d.retries)
		}
		scheduler.penalize(workerID)
	default:
		scheduler.penalize(workerID)
	}
	return plan
}

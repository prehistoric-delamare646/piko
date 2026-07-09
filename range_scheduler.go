package piko

import (
	"context"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

const (
	rangeLease         = 12 * time.Second
	maxDynamicPartSize = 1024 * 1024 * 1024
	warmupPartSize     = 4 * 1024 * 1024
	minDynamicPartSize = 512 * 1024
	minTailPartSize    = 128 * 1024
	idlePartPoll       = 50 * time.Millisecond
	startupActive      = 4
	tailPartsPerConn   = 4
	limitedTailParts   = 2
	speedSmoothFactor  = 0.35
	partSizeTargetTime = 24 * time.Second
	minLeasedPartSpeed = 64 * 1024
	rateLimitMinActive = 2
	rateLimitCooldown  = 10 * time.Second
	rateLimitRecover   = 15 * time.Second
	rateLimitWindow    = 30 * time.Second
	rateLimitStrikes   = 2
	rateLimitIdle      = 1 * time.Second
	rateLimitedPartMin = 32 * 1024 * 1024
)

type partScheduler struct {
	initialPartSize int64
	maxPartSize     int64
	concurrency     int

	mu           sync.Mutex
	front        int64
	back         int64
	index        int
	workerDone   []int
	workerSpeed  []float64
	workerSize   []int64
	partSizeHint int64
	activeCount  int
	maxActive    int
	probeLimit   int
	rateLimited  bool
	recoverAt    time.Time
	limitedAt    time.Time
	limitStrikes int
	queue        []part
	delayed      []delayedPart
	active       []*activePart
}

type delayedPart struct {
	part      part
	readyTime time.Time
}

type activePart struct {
	mu     sync.Mutex
	part   part
	offset atomic.Int64
	end    atomic.Int64
}

func newPartScheduler(size int64, initialPartSize int64, concurrency int) *partScheduler {
	if initialPartSize < 1 {
		initialPartSize = DefaultPartSize
	}
	if concurrency < 1 {
		concurrency = 1
	}
	maxPartSize := max(initialPartSize, min(int64(maxDynamicPartSize), initialPartSize*256))
	if size > 0 {
		maxPartSize = min(maxPartSize, size)
	}
	workerSize := make([]int64, concurrency)
	for i := range workerSize {
		workerSize[i] = initialPartSize
	}
	return &partScheduler{
		initialPartSize: initialPartSize,
		maxPartSize:     maxPartSize,
		concurrency:     concurrency,
		back:            size - 1,
		workerDone:      make([]int, concurrency),
		workerSpeed:     make([]float64, concurrency),
		workerSize:      workerSize,
		partSizeHint:    initialPartSize,
		maxActive:       min(concurrency, startupActive),
		active:          make([]*activePart, concurrency),
	}
}

func (s *partScheduler) nextPart(workerID int) (*activePart, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.recoverRateLimitLocked(time.Now())
	if s.activeCount >= s.maxActive {
		return nil, false
	}

	s.moveReadyDelayedLocked()
	if p, ok := s.popQueuedPartLocked(); ok {
		return s.activateNextLocked(workerID, p), true
	}

	p, ok := s.nextFreshPartLocked(workerID)
	if !ok {
		return nil, false
	}
	return s.activateNextLocked(workerID, p), true
}

func (s *partScheduler) popQueuedPartLocked() (part, bool) {
	if len(s.queue) == 0 {
		return part{}, false
	}
	last := len(s.queue) - 1
	p := s.queue[last]
	s.queue = s.queue[:last]
	return p, true
}

func (s *partScheduler) nextFreshPartLocked(workerID int) (part, bool) {
	if s.front > s.back {
		return part{}, false
	}
	index := s.index + 1
	remaining := s.back - s.front + 1
	partSize := s.nextPartSizeLocked(workerID, remaining)
	if index%2 == 0 {
		end := s.back
		start := max(end-partSize+1, s.front)
		s.back = start - 1
		return part{start: start, end: end}, true
	}
	start := s.front
	end := min(start+partSize-1, s.back)
	s.front = end + 1
	return part{start: start, end: end}, true
}

func (s *partScheduler) activateNextLocked(workerID int, p part) *activePart {
	s.index++
	p.index = s.index
	p.rateProbe = s.rateProbeLocked()
	return s.activateLocked(workerID, p)
}

func (s *partScheduler) activateLocked(workerID int, p part) *activePart {
	active := &activePart{part: p}
	active.offset.Store(p.start)
	active.end.Store(p.end)

	if workerID >= 0 && workerID < len(s.active) {
		if s.active[workerID] == nil {
			s.activeCount++
		}
		s.active[workerID] = active
	}
	return active
}

func (s *partScheduler) finish(workerID int, active *activePart) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if workerID >= 0 && workerID < len(s.active) && s.active[workerID] == active {
		s.active[workerID] = nil
		if s.activeCount > 0 {
			s.activeCount--
		}
	}
}

func (s *partScheduler) hasInFlight() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.moveReadyDelayedLocked()
	if len(s.queue) > 0 || len(s.delayed) > 0 || s.front <= s.back {
		return true
	}
	for _, active := range s.active {
		if active != nil {
			return true
		}
	}
	return false
}

func (s *partScheduler) requeue(p part, offset int64, maxRequeues int, delay time.Duration) bool {
	if offset > p.end {
		return false
	}
	if maxRequeues < 0 {
		maxRequeues = 0
	}
	p.start = offset
	p.requeues++
	p.rateProbe = false
	if p.requeues > maxRequeues+1 {
		return false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if delay > 0 {
		s.delayed = append(s.delayed, delayedPart{part: p, readyTime: time.Now().Add(delay)})
		return true
	}

	chunkSize := p.length() >> 1
	chunkSize = max(chunkSize, s.initialPartSize)
	chunkSize = max(chunkSize, int64(minDynamicPartSize))
	chunkSize = min(chunkSize, p.length())

	chunks := make([]part, 0, (p.length()+chunkSize-1)/chunkSize)
	for start := p.start; start <= p.end; {
		end := min(start+chunkSize-1, p.end)
		chunks = append(chunks, part{start: start, end: end, requeues: p.requeues})
		start = end + 1
	}
	for _, chunk := range slices.Backward(chunks) {
		s.queue = append(s.queue, chunk)
	}
	return true
}

func (s *partScheduler) moveReadyDelayedLocked() {
	if len(s.delayed) == 0 {
		return
	}
	now := time.Now()
	pending := s.delayed[:0]
	for _, delayed := range s.delayed {
		if now.Before(delayed.readyTime) {
			pending = append(pending, delayed)
			continue
		}
		s.queue = append(s.queue, delayed.part)
	}
	s.delayed = pending
}

func (s *partScheduler) limitForRateLimit(delay time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.normalizeMaxActiveLocked()
	if delay < rateLimitCooldown {
		delay = rateLimitCooldown
	}
	now := time.Now()
	if now.Sub(s.limitedAt) > rateLimitWindow {
		s.limitStrikes = 0
	}
	s.limitedAt = now
	s.limitStrikes++
	if s.limitStrikes >= rateLimitStrikes && s.maxActive > rateLimitMinActive {
		s.maxActive--
		s.clearRateProbeLocked()
		s.limitStrikes = rateLimitStrikes - 1
	}
	s.rateLimited = true
	s.extendRecoveryLocked(now, delay)
}

func (s *partScheduler) recoverRateLimitLocked(now time.Time) {
	s.normalizeMaxActiveLocked()
	if !s.rateLimited || s.maxActive >= s.concurrency || now.Before(s.recoverAt) {
		return
	}
	s.maxActive++
	s.probeLimit = s.maxActive
	s.recoverAt = now.Add(rateLimitRecover)
}

func (s *partScheduler) rateProbeLocked() bool {
	return s.probeLimit == s.maxActive &&
		s.probeLimit > rateLimitMinActive &&
		s.activeCount >= s.probeLimit-1
}

func (s *partScheduler) confirmRateProbe(p part) {
	if !p.rateProbe {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.probeLimit == s.maxActive {
		s.clearRateProbeLocked()
		if s.maxActive >= s.concurrency {
			s.rateLimited = false
		}
	}
}

func (s *partScheduler) rejectRateProbe(delay time.Duration) {
	if delay < rateLimitRecover {
		delay = rateLimitRecover
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.probeLimit == s.maxActive && s.maxActive > rateLimitMinActive {
		s.maxActive--
	}
	s.clearRateProbeLocked()
	s.extendRecoveryLocked(time.Now(), delay)
}

func (s *partScheduler) normalizeMaxActiveLocked() {
	if s.maxActive < 1 || s.maxActive > s.concurrency {
		s.maxActive = s.concurrency
	}
}

func (s *partScheduler) clearRateProbeLocked() {
	s.probeLimit = 0
}

func (s *partScheduler) extendRecoveryLocked(now time.Time, delay time.Duration) {
	recoverAt := now.Add(delay)
	if recoverAt.After(s.recoverAt) {
		s.recoverAt = recoverAt
	}
}

func (s *partScheduler) record(workerID int, bytes int64, elapsed time.Duration) {
	if workerID < 0 || workerID >= len(s.workerSpeed) || bytes <= 0 || elapsed <= 0 {
		return
	}

	speed := float64(bytes) / elapsed.Seconds()
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.workerSpeed[workerID] == 0 {
		s.workerSpeed[workerID] = speed
	} else {
		s.workerSpeed[workerID] = s.workerSpeed[workerID]*(1-speedSmoothFactor) + speed*speedSmoothFactor
	}
	s.adjustPartSizeLocked(workerID, bytes, elapsed)
	s.updatePartSizeHintLocked(s.workerPartSizeLocked(workerID))
	s.workerDone[workerID]++
	s.growStartupLocked()
}

func (s *partScheduler) penalize(workerID int) {
	if workerID < 0 || workerID >= len(s.workerSize) {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	current := s.workerSize[workerID]
	if current <= 0 {
		current = s.initialPartSize
	}
	s.workerSize[workerID] = max(current/2, int64(minDynamicPartSize))
}

func (s *partScheduler) nextPartSizeLocked(workerID int, remaining int64) int64 {
	return s.basePartSizeLocked(workerID, remaining)
}

func (s *partScheduler) basePartSizeLocked(workerID int, remaining int64) int64 {
	if s.shouldWarmupLocked(workerID) {
		return min(remaining, min(s.maxPartSize, int64(warmupPartSize)))
	}

	if remaining > s.tailWindowLocked() {
		return min(remaining, s.workerPartSizeLocked(workerID))
	}

	targetParts := int64(s.effectiveConcurrencyLocked()) * s.tailPartsPerConnLocked()
	partSize := (remaining + targetParts - 1) / targetParts
	return clampPartSize(partSize, remaining, s.maxPartSize, minTailPartSize)
}

func (s *partScheduler) shouldWarmupLocked(workerID int) bool {
	if s.rateLimited {
		return false
	}
	return workerID >= 0 && workerID < len(s.workerDone) && s.workerDone[workerID] == 0
}

func (s *partScheduler) tailWindowLocked() int64 {
	return max(s.initialPartSize*16, int64(s.effectiveConcurrencyLocked())*s.initialPartSize)
}

func (s *partScheduler) effectiveConcurrencyLocked() int {
	s.normalizeMaxActiveLocked()
	return max(s.maxActive, 1)
}

func (s *partScheduler) tailPartsPerConnLocked() int64 {
	if s.rateLimited {
		return limitedTailParts
	}
	return tailPartsPerConn
}

func (s *partScheduler) workerPartSizeLocked(workerID int) int64 {
	size := int64(0)
	if workerID >= 0 && workerID < len(s.workerSize) && s.workerSize[workerID] > 0 {
		size = s.workerSize[workerID]
	}
	if size <= 0 {
		size = s.initialPartSize
	}
	if s.rateLimited {
		size = max(size, s.partSizeHint, s.rateLimitedPartFloorLocked())
	}
	return min(size, s.maxPartSize)
}

func (s *partScheduler) adjustPartSizeLocked(workerID int, bytes int64, elapsed time.Duration) {
	if workerID < 0 || workerID >= len(s.workerSize) || bytes < min(s.initialPartSize, s.workerPartSizeLocked(workerID)/2) {
		return
	}
	current := s.workerPartSizeLocked(workerID)
	target := int64(float64(bytes) / elapsed.Seconds() * partSizeTargetTime.Seconds())
	target = clampPartSize(target, s.maxPartSize, s.maxPartSize, minDynamicPartSize)
	if s.workerDone[workerID] == 0 {
		s.workerSize[workerID] = target
		return
	}
	switch {
	case target > current:
		s.workerSize[workerID] = min(target, current*4)
	case target < current/2:
		s.workerSize[workerID] = max(target, current/2)
	default:
		s.workerSize[workerID] = (current + target) / 2
	}
}

func (s *partScheduler) updatePartSizeHintLocked(size int64) {
	if size <= 0 {
		return
	}
	if s.partSizeHint <= s.initialPartSize {
		s.partSizeHint = size
		return
	}
	s.partSizeHint = (s.partSizeHint*2 + size) / 3
}

func (s *partScheduler) rateLimitedPartFloorLocked() int64 {
	return min(max(s.initialPartSize, int64(rateLimitedPartMin)), s.maxPartSize)
}

func (s *partScheduler) growStartupLocked() {
	if s.rateLimited || s.maxActive >= s.concurrency {
		return
	}
	s.maxActive++
}

func clampPartSize(size int64, remaining int64, maxPartSize int64, minPartSize int64) int64 {
	if maxPartSize < 1 {
		maxPartSize = DefaultPartSize
	}
	if minPartSize < 1 {
		minPartSize = minDynamicPartSize
	}
	size = max(size, minPartSize)
	size = min(size, maxPartSize)
	return min(size, remaining)
}

func partLease(p part) time.Duration {
	if p.requeues == 0 && p.length() > slowTailWindow {
		return 0
	}
	if p.length() > DefaultPartSize*4 {
		return 0
	}

	lease := rangeLease
	if p.length() <= slowTailWindow {
		lease = time.Duration(p.length()*int64(time.Second)) / minLeasedPartSpeed
	}
	if p.length() <= minDynamicPartSize && lease < rangeLease/2 {
		lease = rangeLease / 2
	}
	if p.requeues > 0 {
		lease = lease / time.Duration(p.requeues+1)
	}
	if lease < 4*time.Second {
		return 4 * time.Second
	}
	return lease
}

func startLeaseMonitor(cancel context.CancelFunc, lease time.Duration) func() {
	if lease <= 0 {
		return func() {}
	}

	done := make(chan struct{})
	timer := time.NewTimer(lease)
	go func() {
		select {
		case <-timer.C:
			cancel()
		case <-done:
		}
	}()

	return func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		close(done)
	}
}

// Package circuitbreaker implements a 3-state FSM: Closed → Open → Half-Open, with
// rolling error-rate and latency-based trip conditions. The FSM is stored in sync/atomic.Value
// (lock-free snapshot reads); request/latency windows use a separate mutex.
package circuitbreaker

import (
	"fmt"
	"log"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// State of the circuit breaker.
type State int8

const (
	// StateClosed: all traffic uses the primary (Redis) limiter. Trip conditions evaluated after each call.
	StateClosed State = iota
	// StateOpen: all traffic uses local-only; zero primary calls. Fails fast to prevent cascading.
	StateOpen
	// StateHalfOpen: a single probe may hit the primary; all other requests use local until the probe finishes.
	StateHalfOpen
)

const (
	rollingRequests        = 10
	minObservedRequests    = 5
	tripErrorRateThreshold = 0.5

	latencyWindow = 30 * time.Second
	p99MaxLatency = 50 * time.Millisecond

	openDuration = 30 * time.Second
	probeIdle    = int32(0)
	probeClaimed = int32(1) // one in-flight probe in half-open
)

// fsmView is an immutable snapshot: stored in Breaker.fsm (atomic.Value). Hot path: single Load.
type fsmView struct {
	phase  State
	openAt time.Time // time entered open (or zero if not open)
}

// Route tells the caller what to do for the next request.
type Route struct {
	ToPrimary bool  // if false, use local (zero Redis in Open)
	IsProbe   bool  // this goroutine is the one allowed probe in half-open
	Phase     State // for debugging
}

// Breaker coordinates primary vs local traffic and records outcomes for tripping.
// The FSM is stored in atomic.Value (lock-free read on the hot path).
type Breaker struct {
	fsm  atomic.Value // *fsmView
	prob int32

	mu       sync.Mutex
	recentOK []bool // up to 10, oldest trimmed; true = request succeeded (no transport error to Redis)

	// last30s: samples with timestamp, trimmed on record and in trip
	lat []struct {
		t time.Time
		d time.Duration
	}
}

// NewWithDefaults returns a Breaker: trip if error rate > 50% over last 10 with ≥5 samples,
// or p99 > 50ms in last 30s; 30s open, then one probe; probe success → closed, failure → open.
func NewWithDefaults() *Breaker {
	b := &Breaker{
		prob:     0,
		recentOK: make([]bool, 0, rollingRequests),
	}
	b.fsm.Store(&fsmView{phase: StateClosed})
	return b
}

// Route returns the routing decision. Lock-free fsm read on the hot path.
func (b *Breaker) Route() Route {
	now := time.Now()
	// 1) Closed: always primary
	if snap := fsmGet(&b.fsm); snap != nil && snap.phase == StateClosed {
		return Route{ToPrimary: true, IsProbe: false, Phase: StateClosed}
	}

	// 2) Open: local until timeout, then we try to move to half-open
	snap := fsmGet(&b.fsm)
	if snap == nil {
		return Route{ToPrimary: true, IsProbe: false, Phase: StateClosed}
	}
	if snap.phase == StateOpen {
		if now.Sub(snap.openAt) < openDuration {
			return Route{ToPrimary: false, IsProbe: false, Phase: StateOpen}
		}
		b.tryTransitionOpenToHalfOpen(now)
	}

	// 3) Re-load after possible transition
	snap = fsmGet(&b.fsm)
	if snap != nil && snap.phase == StateHalfOpen {
		if atomic.CompareAndSwapInt32(&b.prob, probeIdle, probeClaimed) {
			return Route{ToPrimary: true, IsProbe: true, Phase: StateHalfOpen}
		}
		return Route{ToPrimary: false, IsProbe: false, Phase: StateHalfOpen}
	}

	// 4) Still open (race) or other
	snap = fsmGet(&b.fsm)
	phase := StateOpen
	if snap != nil {
		phase = snap.phase
	}
	if phase == StateOpen {
		return Route{ToPrimary: false, IsProbe: false, Phase: StateOpen}
	}
	return Route{ToPrimary: true, IsProbe: false, Phase: StateClosed}
}

func fsmGet(v *atomic.Value) *fsmView {
	x := v.Load()
	if x == nil {
		return nil
	}
	return x.(*fsmView)
}

func (b *Breaker) tryTransitionOpenToHalfOpen(now time.Time) {
	for {
		oldV := b.fsm.Load()
		p, _ := oldV.(*fsmView)
		if p == nil || p.phase != StateOpen {
			return
		}
		if now.Sub(p.openAt) < openDuration {
			return
		}
		next := &fsmView{phase: StateHalfOpen, openAt: time.Time{}}
		if b.fsm.CompareAndSwap(oldV, next) {
			atomic.StoreInt32(&b.prob, probeIdle)
			return
		}
	}
}

// FinishAfterPrimary records a primary (Redis) round trip and may transition the FSM.
// success means the Redis limiter call returned with err == nil (not business-level allowed/denied).
func (b *Breaker) FinishAfterPrimary(isProbe bool, success bool, d time.Duration) {
	if isProbe {
		if success {
			b.finishProbeOK()
		} else {
			b.finishProbeFail("redis_error", fmt.Errorf("primary returned error"))
		}
		return
	}

	// Closed path: record, then check trip
	b.mu.Lock()
	if success {
		b.recentOK = pushBool(b.recentOK, true)
	} else {
		b.recentOK = pushBool(b.recentOK, false)
	}
	trimBefore := time.Now().Add(-latencyWindow)
	b.lat = append(b.lat, struct {
		t time.Time
		d time.Duration
	}{t: time.Now(), d: d})
	var kept []struct {
		t time.Time
		d time.Duration
	}
	for _, s := range b.lat {
		if s.t.After(trimBefore) {
			kept = append(kept, s)
		}
	}
	b.lat = kept
	recent := append([]bool(nil), b.recentOK...)
	b.mu.Unlock()

	snap := fsmGet(&b.fsm)
	if snap == nil || snap.phase != StateClosed {
		return
	}

	if reason, ok := shouldTripErrorRate(recent, minObservedRequests, tripErrorRateThreshold); ok {
		b.tripOpen(reason, time.Now())
		return
	}
	if b.shouldTripP99() {
		b.tripOpen("p99_latency>50ms", time.Now())
	}
}

func pushBool(s []bool, v bool) []bool {
	s = append(s, v)
	if len(s) > rollingRequests {
		s = s[len(s)-rollingRequests:]
	}
	return s
}

func shouldTripErrorRate(recent []bool, minN int, maxErr float64) (string, bool) {
	n := len(recent)
	if n < minN {
		return "", false
	}
	bad := 0
	for _, ok := range recent {
		if !ok {
			bad++
		}
	}
	rate := float64(bad) / float64(n)
	if rate > maxErr {
		return fmt.Sprintf("error_rate=%.0f%% over last %d (threshold %.0f%% and min %d)", rate*100, n, maxErr*100, minN), true
	}
	return "", false
}

func (b *Breaker) shouldTripP99() bool {
	b.mu.Lock()
	now := time.Now()
	cutoff := now.Add(-latencyWindow)
	var durs []int64
	for _, s := range b.lat {
		if s.t.After(cutoff) {
			durs = append(durs, int64(s.d))
		}
	}
	b.mu.Unlock()
	if len(durs) < 1 {
		return false
	}
	sort.Slice(durs, func(i, j int) bool { return durs[i] < durs[j] })
	idx := int(ceil99(len(durs))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(durs) {
		idx = len(durs) - 1
	}
	return time.Duration(durs[idx]) > p99MaxLatency
}

func ceil99(n int) int {
	if n <= 0 {
		return 0
	}
	// 1-based index of p99: ceil(0.99 * n) when using sorted ascending
	k := (99*n + 99) / 100
	if k < 1 {
		return 1
	}
	if k > n {
		return n
	}
	return k
}

func (b *Breaker) tripOpen(reason string, at time.Time) {
	oldV := b.fsm.Load()
	p, _ := oldV.(*fsmView)
	if p == nil || p.phase != StateClosed {
		return
	}
	nxt := &fsmView{phase: StateOpen, openAt: at}
	if !b.fsm.CompareAndSwap(oldV, nxt) {
		return
	}
	log.Printf("circuit breaker: OPEN (trip) at %s reason=%s", at.UTC().Format(time.RFC3339Nano), reason)
}

func (b *Breaker) finishProbeOK() {
	// one success in half-open → closed
	b.mu.Lock()
	b.recentOK = b.recentOK[:0]
	b.lat = b.lat[:0]
	b.mu.Unlock()
	atomic.StoreInt32(&b.prob, probeIdle)
	for {
		oldV := b.fsm.Load()
		p, _ := oldV.(*fsmView)
		if p == nil {
			return
		}
		if p.phase != StateHalfOpen {
			return
		}
		next := &fsmView{phase: StateClosed, openAt: time.Time{}}
		if b.fsm.CompareAndSwap(oldV, next) {
			return
		}
	}
}

func (b *Breaker) finishProbeFail(reason string, err error) {
	atomic.StoreInt32(&b.prob, probeIdle)
	at := time.Now()
	for {
		oldV := b.fsm.Load()
		p, _ := oldV.(*fsmView)
		if p == nil {
			return
		}
		if p.phase != StateHalfOpen {
			return
		}
		nxt := &fsmView{phase: StateOpen, openAt: at}
		if b.fsm.CompareAndSwap(oldV, nxt) {
			log.Printf("circuit breaker: OPEN (probe fail) at %s reason=%s err=%v", at.UTC().Format(time.RFC3339Nano), reason, err)
			return
		}
	}
}

// Snapshot is for tests / health (optional).
func (b *Breaker) Snapshot() (phase State) {
	if s := fsmGet(&b.fsm); s != nil {
		return s.phase
	}
	return StateClosed
}

package circuitbreaker

import (
	"io"
	"log"
	"os"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	log.SetOutput(io.Discard)
	os.Exit(m.Run())
}

func newBreakerTestClock() (*Breaker, func() time.Time, func(nanos int64) time.Time) {
	b := NewWithDefaults()
	epoch := int64(1_000_000_000_000_000_000) // 2001-09-09, stable
	b.now = func() time.Time { return time.Unix(0, epoch) }
	advance := func(nanos int64) time.Time {
		epoch += nanos
		return b.now()
	}
	return b, b.now, advance
}

// Five closed-path failures in the rolling window trips Open (>50% with n≥5).
func TestErrorRate_5FailedPrimaries_TripsToOpen(t *testing.T) {
	b, _, _ := newBreakerTestClock()

	// 5/5 = 100% > 50%, minN met.
	for range 5 {
		b.FinishAfterPrimary(false, false, 0)
	}
	if got := b.Snapshot(); got != StateOpen {
		t.Fatalf("after 5 failed primaries: Snapshot = %v, want StateOpen", got)
	}
	// Local-only while open (inside cooldown).
	r := b.Route()
	if r.ToPrimary {
		t.Fatal("in Open, expected ToPrimary false")
	}
}

// After Open, 30s later: transition to half-open, one probe, success → closed.
func TestOpen_After30s_ProbeSucceeds_Closed(t *testing.T) {
	b, _, adv := newBreakerTestClock()
	for range 5 {
		b.FinishAfterPrimary(false, false, 0)
	}
	if b.Snapshot() != StateOpen {
		t.Fatalf("pre: want Open")
	}
	adv(30 * int64(time.Second)) // 30s later

	// One goroutine wins probe; others are local in same instant (simulated: single test thread).
	probe := b.Route()
	if !probe.IsProbe {
		// In rare CAS races we might still be Open; advance and retry.
		adv(1)
		probe = b.Route()
	}
	if !probe.ToPrimary || !probe.IsProbe {
		t.Fatalf("half-open: want single probe, got %#v", probe)
	}
	b.FinishAfterPrimary(true, true, time.Microsecond)
	if b.Snapshot() != StateClosed {
		t.Fatalf("after probe success: want Closed, got %v", b.Snapshot())
	}
}

// Probe failure re-opens and resets 30s window (relative to that transition).
func TestHalfOpen_ProbeFailure_Reopens(t *testing.T) {
	b, _, adv := newBreakerTestClock()
	for range 5 {
		b.FinishAfterPrimary(false, false, 0)
	}
	adv(30 * int64(time.Second))
	r1 := b.Route()
	if !r1.ToPrimary || !r1.IsProbe {
		t.Fatalf("expected probe, got %#v", r1)
	}
	b.FinishAfterPrimary(true, false, 0) // probe: error
	if b.Snapshot() != StateOpen {
		t.Fatalf("after probe error: want Open, got %v", b.Snapshot())
	}
}

// Table: closed-path error streak → tripped
func TestTrip_ErrorRateTable(t *testing.T) {
	cases := []struct {
		name      string
		oks       int
		fails     int
		wantTrip  bool
	}{
		{"no_trip_4_1", 4, 0, false},          // 4/4 success
		{"no_trip_4_1_4_fails_1", 0, 4, false}, // 4/4 fail but n<5
		{"trip_5_5_fails", 0, 5, true},
		{"trip_5_3fail_2ok", 2, 3, true}, // 3/5=60% > 50%, n>=5
		{"no_trip_5_2fails_3ok", 3, 2, false}, // 2/5=40%
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := NewWithDefaults()
			for i := 0; i < tc.oks; i++ {
				b.FinishAfterPrimary(false, true, 0)
			}
			for i := 0; i < tc.fails; i++ {
				b.FinishAfterPrimary(false, false, 0)
			}
			open := b.Snapshot() == StateOpen
			if open != tc.wantTrip {
				t.Fatalf("open=%v want %v (snap=%v)", open, tc.wantTrip, b.Snapshot())
			}
		})
	}
}

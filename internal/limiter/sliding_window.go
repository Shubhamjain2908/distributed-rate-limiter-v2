package limiter

import (
	"context"
	"sort"
	"sync"
	"time"
)

// SlidingWindow is a pure-Go sliding window limit: max n events per client per window.
type SlidingWindow struct {
	mu       sync.Mutex
	clients  map[string]*slidingState
	limit    int
	window   time.Duration
	clock    func() time.Time
	sanitize func(string) string
}

type slidingState struct {
	timestamps []time.Time
}

// NewSlidingWindow creates a new limiter: at most `limit` requests per `window` per clientID.
func NewSlidingWindow(limit int, window time.Duration) *SlidingWindow {
	if limit < 1 {
		limit = 1
	}
	if window <= 0 {
		window = time.Second
	}
	return &SlidingWindow{
		clients:  make(map[string]*slidingState),
		limit:    limit,
		window:   window,
		clock:    time.Now,
		sanitize: defaultSanitize,
	}
}

func (s *SlidingWindow) WithSanitizeKey(fn func(string) string) *SlidingWindow {
	s.sanitize = fn
	return s
}

// Allow returns allowed if the client has fewer than `limit` events in the last `window`.
// cost adds `cost` event slots (e.g. charge multiple units for a heavy request).
func (s *SlidingWindow) Allow(ctx context.Context, clientID string, cost int) (LimitResult, error) {
	if err := ctx.Err(); err != nil {
		return LimitResult{}, err
	}
	if cost < 0 {
		cost = 0
	}
	if cost == 0 {
		return LimitResult{Allowed: true, Remaining: s.limit, Algorithm: AlgorithmSlidingWindow}, nil
	}
	now := s.clock()
	cutoff := now.Add(-s.window)
	key := s.sanitize(clientID)

	s.mu.Lock()
	defer s.mu.Unlock()

	st, ok := s.clients[key]
	if !ok {
		st = &slidingState{}
		s.clients[key] = st
	}
	// Prune old timestamps
	kept := st.timestamps[:0]
	for _, ts := range st.timestamps {
		if !ts.Before(cutoff) {
			kept = append(kept, ts)
		}
	}
	st.timestamps = kept

	if cost > s.limit {
		return LimitResult{
			Allowed:      false,
			Remaining:    0,
			RetryAfterMs: s.window.Milliseconds(),
			Algorithm:    AlgorithmSlidingWindow,
		}, nil
	}

	used := len(st.timestamps)
	if used+cost > s.limit {
		// Need `excess` oldest events to fall out of the window before this request can fit.
		excess := used + cost - s.limit
		if excess < 1 {
			excess = 1
		}
		tmp := make([]time.Time, len(st.timestamps))
		copy(tmp, st.timestamps)
		sort.Slice(tmp, func(i, j int) bool { return tmp[i].Before(tmp[j]) })
		// The `excess`-th oldest (1-based) timestamp determines when a slot appears.
		if len(tmp) == 0 {
			return LimitResult{
				Allowed:      false,
				Remaining:    0,
				RetryAfterMs: s.window.Milliseconds(),
				Algorithm:    AlgorithmSlidingWindow,
			}, nil
		}
		idx := excess - 1
		if idx < 0 {
			idx = 0
		}
		if idx >= len(tmp) {
			idx = len(tmp) - 1
		}
		// Next moment we have headroom: when tmp[idx] ages past the window edge
		boundary := tmp[idx].Add(s.window)
		retryAfter := boundary.Sub(now)
		if retryAfter < 0 {
			retryAfter = 0
		}
		remaining := s.limit - used
		if remaining < 0 {
			remaining = 0
		}
		return LimitResult{
			Allowed:      false,
			Remaining:    remaining,
			RetryAfterMs: retryAfter.Milliseconds(),
			Algorithm:    AlgorithmSlidingWindow,
		}, nil
	}
	// Record `cost` events as now (coarse; good enough for a learning core)
	for i := 0; i < cost; i++ {
		st.timestamps = append(st.timestamps, now)
	}
	rem := s.limit - len(st.timestamps)
	if rem < 0 {
		rem = 0
	}
	return LimitResult{
		Allowed:      true,
		Remaining:    rem,
		RetryAfterMs: 0,
		Algorithm:    AlgorithmSlidingWindow,
	}, nil
}

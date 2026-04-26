// Request logging for rate-limit path (enable with LOG_REQUESTS; set LOG_REQUESTS=0 in prod to reduce I/O).
package middleware

import (
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/shubhamjain2908/distributed-rate-limiter-v2/internal/limiter"
)

var logRequests = true

func init() {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("LOG_REQUESTS"))) {
	case "0", "false", "no", "off":
		logRequests = false
	}
}

// RequestLogEnabled is true unless LOG_REQUESTS=0|false|no|off
func RequestLogEnabled() bool {
	return logRequests
}

// LogRateLimitDecision logs one line after LimitResult is known. duration is Allow() wall time.
func LogRateLimitDecision(
	r *http.Request,
	clientID string,
	cost int,
	res limiter.LimitResult,
	allowErr error,
	duration time.Duration,
	httpStatus int,
) {
	if !logRequests {
		return
	}
	cid := truncateID(clientID, 128)
	algo := res.Algorithm
	if algo == "" {
		algo = "-"
	}
	var errStr string
	if allowErr != nil {
		errStr = allowErr.Error()
	} else {
		errStr = "none"
	}
	log.Printf(
		"http %s %s | client_id=%q cost=%d | allowed=%t remaining=%d algorithm=%s via_fallback=%t retry_after_ms=%d | allow_err=%s | allow=%.3fms | status=%d | remote=%s",
		r.Method, r.URL.Path, cid, cost, res.Allowed, res.Remaining, algo, res.ViaFallback, res.RetryAfterMs, errStr,
		duration.Seconds()*1000, httpStatus, truncateID(r.RemoteAddr, 64),
	)
}

func truncateID(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

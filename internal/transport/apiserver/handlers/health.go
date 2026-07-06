// Package handlers owns the HTTP handlers for the v1 API listener.
// Handlers consume pool and auth contracts from sibling packages; they
// must not import each other or any transport-level wiring.
package handlers

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"vpntunnel/internal/application/asyncjob"
	"vpntunnel/internal/tunnel"
)

// NewHealthHandler returns an http.Handler that serves GET requests with the
// aggregated multi-tunnel health snapshot and async job counts. The handler
// is safe for concurrent use; it holds no mutable state beyond the pool and
// counter references, which are themselves safe for concurrent use.
//
// maxAge is the threshold used to decide whether a tunnel's last handshake is
// fresh enough to be considered healthy. counter supplies the async job counts
// included in every response; if its Counts call fails the three count fields
// degrade to -1 (operator signal: count temporarily unavailable). opLog is
// used to record internal errors only; it must not be nil.
//
// Note: Counts performs an O(N) scan over all job records per request. This is
// acceptable for v2 (bounded record set). Flag for optimisation in v3 if the
// job bucket grows to millions of records and the scan becomes a hot path.
func NewHealthHandler(p tunnelPool, maxAge time.Duration, counter AsyncJobCounter, opLog *slog.Logger) http.Handler {
	return &healthHandler{pool: p, maxAge: maxAge, counter: counter, log: opLog}
}

// healthHandler implements http.Handler for the multi-tunnel health endpoint.
type healthHandler struct {
	pool    tunnelPool
	maxAge  time.Duration
	counter AsyncJobCounter
	log     *slog.Logger
}

// ServeHTTP handles a single health probe request.
//
// Non-GET methods receive 405 with an Allow header and no body.
// For GET, ServeHTTP aggregates Reports() into a JSON response:
//   - 200 when status is "ok" (all tunnels healthy).
//   - 200 when status is "degraded" (some healthy, some not). HTTP 200 is
//     intentional — automation distinguishing ok vs degraded must parse the
//     JSON body; HTTP status alone is insufficient.
//   - 503 when status is "down" (no healthy tunnels, or empty pool).
func (h *healthHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	now := time.Now()
	reports := h.pool.Reports()

	tunnelEntries := make([]entry, len(reports))
	for i, rep := range reports {
		tunnelEntries[i] = tunnelHealthEntry(rep, now, h.maxAge)
	}

	// counts are fetched unconditionally — the 503 path (tunnel down) still
	// includes them so operators can see queue depth at a glance during an outage.
	pendingCount, completedCount, tombstoneCount := h.fetchCounts()

	status := aggregateStatus(tunnelEntries)
	body := healthResponse{
		Status:             status,
		Tunnels:            tunnelEntries,
		PendingJobsCount:   pendingCount,
		CompletedJobsCount: completedCount,
		TombstoneJobsCount: tombstoneCount,
	}

	data, err := json.Marshal(body)
	if err != nil {
		h.log.Error("health handler: failed to marshal response", slog.String("error", err.Error()))
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	code := http.StatusOK
	if status == "down" {
		code = http.StatusServiceUnavailable
	}
	w.WriteHeader(code)
	_, _ = w.Write(data) // unrecoverable once WriteHeader sent
}

// fetchCounts calls counter.Counts and returns the three count values. On
// error it logs at ERROR and returns -1 for all three as an operator signal
// that the counts are temporarily unavailable. The HTTP status code is
// unaffected — that remains driven by tunnel health.
func (h *healthHandler) fetchCounts() (pending, completed, tombstone int) {
	counts, err := h.counter.Counts()
	if err != nil {
		h.log.Error("health handler: failed to fetch async job counts", slog.Any("err", err))
		return -1, -1, -1
	}
	return counts.Pending, counts.Completed, counts.Tombstone
}

// AsyncJobCounter is the narrow counter contract the health handler needs from
// the async job store. Using this interface instead of the full asyncjob.Store
// keeps the handler decoupled from store lifecycle methods and lets callers
// stub the count behaviour cheaply (e.g. a zero-allocation noop for when no
// store is wired yet).
type AsyncJobCounter interface {
	Counts() (asyncjob.JobCounts, error)
}

// tunnelPool is the narrow read-only contract the health handler depends on.
// Unexported so callers can substitute fakes in tests without exposing a
// wider abstraction.
type tunnelPool interface {
	Reports() []tunnel.TunnelHealth
}

// healthResponse is the top-level JSON body for the health endpoint.
type healthResponse struct {
	Status             string  `json:"status"`
	Tunnels            []entry `json:"tunnels"`
	PendingJobsCount   int     `json:"pending_jobs_count"`
	CompletedJobsCount int     `json:"completed_jobs_count"`
	TombstoneJobsCount int     `json:"tombstone_jobs_count"`
}

// entry is the per-tunnel fragment of the health response.
type entry struct {
	ID                  string `json:"id"`
	Healthy             bool   `json:"healthy"`
	HandshakeAgeSeconds int64  `json:"handshake_age_seconds"`
}

// tunnelHealthEntry returns the response entry for one TunnelHealth at the
// given now. Pure function; deterministic; trivially testable without touching
// the handler or the pool.
func tunnelHealthEntry(h tunnel.TunnelHealth, now time.Time, maxAge time.Duration) entry {
	ts := h.LastHandshake
	if h.Err != nil || ts.IsZero() {
		return entry{
			ID:                  h.ID,
			Healthy:             false,
			HandshakeAgeSeconds: -1,
		}
	}
	age := now.Sub(ts)
	if age < 0 {
		age = 0 // future timestamp: clock skew; treat as just-handshaked
	}
	healthy := age <= maxAge
	return entry{
		ID:                  h.ID,
		Healthy:             healthy,
		HandshakeAgeSeconds: int64(age / time.Second),
	}
}

// aggregateStatus derives the overall status string from per-tunnel entries.
// Returns "ok" when all tunnels are healthy, "down" when none are (including
// the empty-pool case), and "degraded" otherwise.
func aggregateStatus(entries []entry) string {
	if len(entries) == 0 {
		return "down"
	}
	healthy := 0
	for _, e := range entries {
		if e.Healthy {
			healthy++
		}
	}
	switch {
	case healthy == len(entries):
		return "ok"
	case healthy == 0:
		return "down"
	default:
		return "degraded"
	}
}

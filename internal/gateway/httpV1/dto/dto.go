// Package dto holds the exported JSON response bodies for the v1 API. These
// shapes are the frozen wire contract documented in CLAUDE.md; their fields
// and json tags must not change without a deliberate contract revision.
package dto

// HealthResponse is the top-level JSON body for GET /v1/admin/health.
type HealthResponse struct {
	Status             string        `json:"status"`
	Tunnels            []HealthEntry `json:"tunnels"`
	PendingJobsCount   int           `json:"pending_jobs_count"`
	CompletedJobsCount int           `json:"completed_jobs_count"`
	TombstoneJobsCount int           `json:"tombstone_jobs_count"`
}

// HealthEntry is the per-tunnel fragment of HealthResponse. It never carries
// peer_endpoint, key material, the peer public key, or the underlying error
// text — only the tunnel id, health flag, and handshake age.
type HealthEntry struct {
	ID                  string `json:"id"`
	Healthy             bool   `json:"healthy"`
	HandshakeAgeSeconds int64  `json:"handshake_age_seconds"`
}

// RotateResponse is the JSON body for /v1/admin/rotate. Country and
// ActiveSessions are omitted from the encoded body unless meaningful for the
// outcome, keeping the three response shapes exactly as documented.
type RotateResponse struct {
	Status         string `json:"status"`
	Country        string `json:"country,omitempty"`
	ActiveSessions *int64 `json:"active_sessions,omitempty"`
}

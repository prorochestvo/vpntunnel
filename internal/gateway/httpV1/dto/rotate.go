package dto

// RotateResponse is the JSON body for /v1/admin/rotate. Country and
// ActiveSessions are omitted from the encoded body unless meaningful for the
// outcome, keeping the three response shapes exactly as documented.
type RotateResponse struct {
	Status         string `json:"status"`
	Country        string `json:"country,omitempty"`
	ActiveSessions *int64 `json:"active_sessions,omitempty"`
}

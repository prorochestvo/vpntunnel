// Package asyncjob defines the record types and marshalling helpers for
// async proxy jobs stored per retry-tag in bbolt.
package asyncjob

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// StatusPending indicates the job has been accepted and is waiting for the
// upstream response.
const StatusPending Status = "pending"

// StatusCompleted indicates the upstream responded successfully and the
// UpstreamResponse is populated.
const StatusCompleted Status = "completed"

// StatusFailed indicates the upstream returned an error and the
// UpstreamResponse is populated with whatever partial information is available.
const StatusFailed Status = "failed"

// StatusFailedTimeout indicates the upstream did not respond within the
// configured deadline; UpstreamResponse is populated.
const StatusFailedTimeout Status = "failed_timeout"

// StatusTombstone indicates the record has been logically deleted; EvictedAt
// is set and UpstreamResponse is stripped to keep the on-disk record small.
const StatusTombstone Status = "tombstone"

// ErrUnknownStatus is returned by Unmarshal when the stored status string does
// not match any of the five known Status values.
var ErrUnknownStatus = errors.New("asyncjob: unknown status")

// NewPendingRecord returns a Record in StatusPending state for the given tag,
// with CreatedAt and UpdatedAt set to now (UTC). EvictedAt and UpstreamResponse
// are zero / nil.
func NewPendingRecord(tag string, now time.Time) Record {
	t := now.UTC()
	return Record{
		Tag:       tag,
		Status:    StatusPending,
		CreatedAt: t,
		UpdatedAt: t,
	}
}

// Unmarshal deserialises a Record from JSON bytes produced by Marshal.
// It returns ErrUnknownStatus (sentinel) when the stored status string is not
// one of the five known values. All time.Time values in the returned Record
// are in UTC.
func Unmarshal(data []byte) (Record, error) {
	var w wireRecord
	if err := json.Unmarshal(data, &w); err != nil {
		return Record{}, fmt.Errorf("asyncjob: unmarshal record: %w", err)
	}
	if err := validateStatus(w.Status); err != nil {
		return Record{}, err
	}
	r := Record{
		Tag:              w.Tag,
		Status:           w.Status,
		CreatedAt:        time.Unix(w.CreatedAt, 0).UTC(),
		UpdatedAt:        time.Unix(w.UpdatedAt, 0).UTC(),
		UpstreamResponse: w.UpstreamResponse,
	}
	if w.EvictedAt != nil {
		r.EvictedAt = time.Unix(*w.EvictedAt, 0).UTC()
	}
	return r, nil
}

// Record is the value stored under a retry-tag key in bbolt. All time.Time
// fields are serialised as Unix seconds (int64) to keep the tombstone record
// under 120 bytes without a payload. Callers always receive time.Time values
// in UTC after unmarshal.
type Record struct {
	// Tag is the retry-tag that identifies this job.
	Tag string `json:"tag"`
	// Status is the current lifecycle state of the job.
	Status Status `json:"status"`
	// CreatedAt is the UTC time at which the job was first accepted.
	CreatedAt time.Time `json:"-"`
	// UpdatedAt is the UTC time of the most recent status transition.
	UpdatedAt time.Time `json:"-"`
	// EvictedAt is the UTC time at which the record was transitioned to
	// StatusTombstone. It is the zero value for all other statuses.
	EvictedAt time.Time `json:"-"`
	// UpstreamResponse is populated on completed, failed, and failed_timeout
	// transitions. It is nil for pending and tombstone records.
	UpstreamResponse *UpstreamResponse `json:"upstream_response,omitempty"`
}

// Marshal serialises r to JSON. Timestamps are encoded as Unix seconds (int64)
// to keep the on-disk record compact; tombstone records stay under 120 bytes.
// All timestamps are converted to UTC before encoding. Returns ErrUnknownStatus
// when r.Status is not one of the five known values.
func (r Record) Marshal() ([]byte, error) {
	if err := validateStatus(r.Status); err != nil {
		return nil, err
	}
	w := wireRecord{
		Tag:       r.Tag,
		Status:    r.Status,
		CreatedAt: r.CreatedAt.UTC().Unix(),
		UpdatedAt: r.UpdatedAt.UTC().Unix(),
	}
	if r.UpstreamResponse != nil {
		cp := *r.UpstreamResponse
		w.UpstreamResponse = &cp
	}
	if !r.EvictedAt.IsZero() {
		ts := r.EvictedAt.UTC().Unix()
		w.EvictedAt = &ts
	}
	b, err := json.Marshal(w)
	if err != nil {
		return nil, fmt.Errorf("asyncjob: marshal record: %w", err)
	}
	return b, nil
}

// Status is a typed string representing the lifecycle state of an async job.
// The five valid values are declared as Status* constants below.
type Status string

// String returns the underlying string value of s.
func (s Status) String() string { return string(s) }

// UpstreamResponse holds the HTTP response captured from the upstream for
// completed, failed, and failed_timeout records. It is omitted entirely from
// tombstone records.
type UpstreamResponse struct {
	// StatusCode is the HTTP response status code from the upstream.
	StatusCode int `json:"status_code"`
	// Header contains the upstream response headers. Keys are preserved
	// verbatim — they are not passed through http.Header.Set or CanonicalMIME
	// so header casing round-trips exactly.
	Header http.Header `json:"header"`
	// Body is the raw upstream response body. A nil value means no body was
	// recorded; an empty []byte means the upstream sent a zero-length body.
	// The distinction is preserved across marshal/unmarshal.
	Body []byte `json:"body"`
}

// maxStatusEchoLen caps the number of bytes echoed in an ErrUnknownStatus
// message to prevent a corrupt record from flooding logs with a huge string.
const maxStatusEchoLen = 32

// wireRecord is the JSON-serialisable representation of Record. Timestamps are
// stored as Unix seconds (int64) instead of RFC3339 strings to keep the
// tombstone record compact.
type wireRecord struct {
	Tag              string            `json:"tag"`
	Status           Status            `json:"status"`
	CreatedAt        int64             `json:"created_at"`
	UpdatedAt        int64             `json:"updated_at"`
	EvictedAt        *int64            `json:"evicted_at,omitempty"`
	UpstreamResponse *UpstreamResponse `json:"upstream_response,omitempty"`
}

// unmarshalStatus parses only the status field from JSON bytes produced by
// Marshal. It is cheaper than Unmarshal when the caller only needs to bucket
// a record by status (e.g. Counts) and must not allocate the full Record.
// Returns ErrUnknownStatus when the stored status string is not one of the
// five known values.
func unmarshalStatus(data []byte) (Status, error) {
	var w struct {
		Status Status `json:"status"`
	}
	if err := json.Unmarshal(data, &w); err != nil {
		return "", fmt.Errorf("asyncjob: unmarshal status: %w", err)
	}
	if err := validateStatus(w.Status); err != nil {
		return "", err
	}
	return w.Status, nil
}

// validateStatus returns ErrUnknownStatus when s is not one of the five known
// Status values.
func validateStatus(s Status) error {
	switch s {
	case StatusPending, StatusCompleted, StatusFailed, StatusFailedTimeout, StatusTombstone:
		return nil
	}
	echo := string(s)
	if len(echo) > maxStatusEchoLen {
		echo = echo[:maxStatusEchoLen] + "…"
	}
	return fmt.Errorf("%w: %q", ErrUnknownStatus, echo)
}

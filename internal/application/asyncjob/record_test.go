package asyncjob_test

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vpntunnel/internal/application/asyncjob"
)

func TestNewPendingRecord(t *testing.T) {
	t.Parallel()

	t.Run("fields are initialised correctly", func(t *testing.T) {
		t.Parallel()
		now := time.Now()
		r := asyncjob.NewPendingRecord("tag-abc", now)

		assert.Equal(t, "tag-abc", r.Tag)
		assert.Equal(t, asyncjob.StatusPending, r.Status)
		assert.Equal(t, now.UTC(), r.CreatedAt)
		assert.Equal(t, now.UTC(), r.UpdatedAt)
		assert.True(t, r.EvictedAt.IsZero())
		assert.Nil(t, r.UpstreamResponse)
	})

	t.Run("timestamps are stored in UTC", func(t *testing.T) {
		t.Parallel()
		loc, err := time.LoadLocation("America/New_York")
		require.NoError(t, err)
		now := time.Now().In(loc)
		r := asyncjob.NewPendingRecord("tag-tz", now)

		assert.Equal(t, time.UTC, r.CreatedAt.Location())
		assert.Equal(t, time.UTC, r.UpdatedAt.Location())
	})
}

func TestUnmarshal(t *testing.T) {
	t.Parallel()

	t.Run("unknown status returns ErrUnknownStatus", func(t *testing.T) {
		t.Parallel()
		data := []byte(`{"tag":"x","status":"bogus","created_at":1749981600,"updated_at":1749981600}`)
		_, err := asyncjob.Unmarshal(data)
		require.Error(t, err)
		assert.True(t, errors.Is(err, asyncjob.ErrUnknownStatus), "expected ErrUnknownStatus, got: %v", err)
	})

	t.Run("malformed JSON returns wrapped error", func(t *testing.T) {
		t.Parallel()
		_, err := asyncjob.Unmarshal([]byte(`{not valid json`))
		require.Error(t, err)
		assert.False(t, errors.Is(err, asyncjob.ErrUnknownStatus))
	})

	t.Run("empty status string returns ErrUnknownStatus", func(t *testing.T) {
		t.Parallel()
		data := []byte(`{"tag":"x","status":"","created_at":1749981600,"updated_at":1749981600}`)
		_, err := asyncjob.Unmarshal(data)
		require.Error(t, err)
		assert.True(t, errors.Is(err, asyncjob.ErrUnknownStatus))
	})

	t.Run("128-byte garbage status produces bounded error message", func(t *testing.T) {
		t.Parallel()
		garbage := make([]byte, 128)
		for i := range garbage {
			garbage[i] = 'A'
		}
		data := []byte(`{"tag":"x","status":"` + string(garbage) + `","created_at":1749981600,"updated_at":1749981600}`)
		_, err := asyncjob.Unmarshal(data)
		require.Error(t, err)
		assert.True(t, errors.Is(err, asyncjob.ErrUnknownStatus))
		assert.LessOrEqual(t, len(err.Error()), 80, "error message must be bounded, got %d chars: %s", len(err.Error()), err.Error())
	})
}
func TestRecord_Marshal(t *testing.T) {
	t.Parallel()

	// base time at second precision — wire format stores Unix seconds so
	// sub-second values would not survive the round-trip.
	now := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)

	t.Run("pending record round-trips", func(t *testing.T) {
		t.Parallel()
		r := asyncjob.NewPendingRecord("tag-123", now)
		b, err := r.Marshal()
		require.NoError(t, err)
		got, err := asyncjob.Unmarshal(b)
		require.NoError(t, err)
		assert.Equal(t, r.Tag, got.Tag)
		assert.Equal(t, r.Status, got.Status)
		assert.Equal(t, sec(r.CreatedAt), got.CreatedAt)
		assert.Equal(t, sec(r.UpdatedAt), got.UpdatedAt)
		assert.True(t, got.EvictedAt.IsZero())
		assert.Nil(t, got.UpstreamResponse)
	})

	t.Run("completed record with upstream response round-trips", func(t *testing.T) {
		t.Parallel()
		r := asyncjob.Record{
			Tag:       "tag-done",
			Status:    asyncjob.StatusCompleted,
			CreatedAt: now,
			UpdatedAt: now.Add(time.Second),
			UpstreamResponse: &asyncjob.UpstreamResponse{
				StatusCode: 200,
				Header: http.Header{
					"Content-Type":  {"application/json"},
					"X-Custom-Head": {"val"},
				},
				Body: []byte(`{"ok":true}`),
			},
		}
		b, err := r.Marshal()
		require.NoError(t, err)
		got, err := asyncjob.Unmarshal(b)
		require.NoError(t, err)
		assert.Equal(t, r.Tag, got.Tag)
		assert.Equal(t, r.Status, got.Status)
		assert.Equal(t, sec(r.CreatedAt), got.CreatedAt)
		assert.Equal(t, sec(r.UpdatedAt), got.UpdatedAt)
		require.NotNil(t, got.UpstreamResponse)
		assert.Equal(t, r.UpstreamResponse.StatusCode, got.UpstreamResponse.StatusCode)
		assert.Equal(t, r.UpstreamResponse.Header, got.UpstreamResponse.Header)
		assert.Equal(t, r.UpstreamResponse.Body, got.UpstreamResponse.Body)
	})

	t.Run("failed record round-trips", func(t *testing.T) {
		t.Parallel()
		r := asyncjob.Record{
			Tag:       "tag-fail",
			Status:    asyncjob.StatusFailed,
			CreatedAt: now,
			UpdatedAt: now.Add(2 * time.Second),
			UpstreamResponse: &asyncjob.UpstreamResponse{
				StatusCode: 502,
				Header:     http.Header{"X-Err": {"bad-gw"}},
				Body:       []byte("bad gateway"),
			},
		}
		b, err := r.Marshal()
		require.NoError(t, err)
		got, err := asyncjob.Unmarshal(b)
		require.NoError(t, err)
		assert.Equal(t, r.Tag, got.Tag)
		assert.Equal(t, r.Status, got.Status)
		assert.Equal(t, sec(r.CreatedAt), got.CreatedAt)
		assert.Equal(t, sec(r.UpdatedAt), got.UpdatedAt)
		require.NotNil(t, got.UpstreamResponse)
		assert.Equal(t, r.UpstreamResponse.StatusCode, got.UpstreamResponse.StatusCode)
		assert.Equal(t, r.UpstreamResponse.Header, got.UpstreamResponse.Header)
		assert.Equal(t, r.UpstreamResponse.Body, got.UpstreamResponse.Body)
	})

	t.Run("failed_timeout record round-trips", func(t *testing.T) {
		t.Parallel()
		r := asyncjob.Record{
			Tag:       "tag-timeout",
			Status:    asyncjob.StatusFailedTimeout,
			CreatedAt: now,
			UpdatedAt: now.Add(30 * time.Second),
			UpstreamResponse: &asyncjob.UpstreamResponse{
				StatusCode: 504,
				Header:     http.Header{},
				Body:       nil,
			},
		}
		b, err := r.Marshal()
		require.NoError(t, err)
		got, err := asyncjob.Unmarshal(b)
		require.NoError(t, err)
		assert.Equal(t, asyncjob.StatusFailedTimeout, got.Status)
		require.NotNil(t, got.UpstreamResponse)
		assert.Equal(t, 504, got.UpstreamResponse.StatusCode)
		assert.Equal(t, r.UpstreamResponse.Header, got.UpstreamResponse.Header)
		assert.Nil(t, got.UpstreamResponse.Body)
	})

	t.Run("tombstone record round-trips", func(t *testing.T) {
		t.Parallel()
		evictedAt := now.Add(time.Minute)
		r := asyncjob.Record{
			Tag:       "tag-dead",
			Status:    asyncjob.StatusTombstone,
			CreatedAt: now,
			UpdatedAt: evictedAt,
			EvictedAt: evictedAt,
		}
		b, err := r.Marshal()
		require.NoError(t, err)
		got, err := asyncjob.Unmarshal(b)
		require.NoError(t, err)
		assert.Equal(t, r.Tag, got.Tag)
		assert.Equal(t, asyncjob.StatusTombstone, got.Status)
		assert.Equal(t, sec(r.EvictedAt), got.EvictedAt)
		assert.Nil(t, got.UpstreamResponse)
	})

	t.Run("timestamps are normalised to UTC on marshal", func(t *testing.T) {
		t.Parallel()
		loc, err := time.LoadLocation("Europe/Moscow")
		require.NoError(t, err)
		local := time.Date(2026, 6, 15, 13, 0, 0, 0, loc)
		r := asyncjob.Record{
			Tag:       "tag-tz",
			Status:    asyncjob.StatusPending,
			CreatedAt: local,
			UpdatedAt: local,
		}
		b, err := r.Marshal()
		require.NoError(t, err)
		got, err := asyncjob.Unmarshal(b)
		require.NoError(t, err)
		assert.Equal(t, time.UTC, got.CreatedAt.Location())
		assert.Equal(t, time.UTC, got.UpdatedAt.Location())
		assert.Equal(t, local.UTC().Truncate(time.Second), got.CreatedAt)
	})

	t.Run("empty body is distinct from nil body after round-trip", func(t *testing.T) {
		t.Parallel()
		withEmpty := asyncjob.Record{
			Tag:       "tag-emptybody",
			Status:    asyncjob.StatusCompleted,
			CreatedAt: now,
			UpdatedAt: now,
			UpstreamResponse: &asyncjob.UpstreamResponse{
				StatusCode: 204,
				Header:     http.Header{},
				Body:       []byte{},
			},
		}
		withNil := asyncjob.Record{
			Tag:       "tag-nilbody",
			Status:    asyncjob.StatusCompleted,
			CreatedAt: now,
			UpdatedAt: now,
			UpstreamResponse: &asyncjob.UpstreamResponse{
				StatusCode: 204,
				Header:     http.Header{},
				Body:       nil,
			},
		}

		bEmpty, err := withEmpty.Marshal()
		require.NoError(t, err)
		gotEmpty, err := asyncjob.Unmarshal(bEmpty)
		require.NoError(t, err)
		require.NotNil(t, gotEmpty.UpstreamResponse)
		assert.NotNil(t, gotEmpty.UpstreamResponse.Body, "empty body should not unmarshal as nil")
		assert.Empty(t, gotEmpty.UpstreamResponse.Body)

		bNil, err := withNil.Marshal()
		require.NoError(t, err)
		gotNil, err := asyncjob.Unmarshal(bNil)
		require.NoError(t, err)
		require.NotNil(t, gotNil.UpstreamResponse)
		assert.Nil(t, gotNil.UpstreamResponse.Body, "nil body should unmarshal as nil")
	})

	t.Run("http.Header case is preserved verbatim after round-trip", func(t *testing.T) {
		t.Parallel()
		r := asyncjob.Record{
			Tag:       "tag-header-case",
			Status:    asyncjob.StatusCompleted,
			CreatedAt: now,
			UpdatedAt: now,
			UpstreamResponse: &asyncjob.UpstreamResponse{
				StatusCode: 200,
				Header: http.Header{
					"x-custom-lowercase": {"val1"},
					"X-Mixed-Case":       {"val2"},
					"ALLCAPS":            {"val3"},
				},
				Body: nil,
			},
		}
		b, err := r.Marshal()
		require.NoError(t, err)
		got, err := asyncjob.Unmarshal(b)
		require.NoError(t, err)
		require.NotNil(t, got.UpstreamResponse)
		assert.Equal(t, []string{"val1"}, got.UpstreamResponse.Header["x-custom-lowercase"])
		assert.Equal(t, []string{"val2"}, got.UpstreamResponse.Header["X-Mixed-Case"])
		assert.Equal(t, []string{"val3"}, got.UpstreamResponse.Header["ALLCAPS"])
	})

	t.Run("tombstone record marshals within size bound", func(t *testing.T) {
		t.Parallel()
		evictedAt := now.Add(time.Minute)
		r := asyncjob.Record{
			Tag:       "abc12345",
			Status:    asyncjob.StatusTombstone,
			CreatedAt: now,
			UpdatedAt: evictedAt,
			EvictedAt: evictedAt,
		}
		b, err := r.Marshal()
		require.NoError(t, err)
		assert.LessOrEqual(t, len(b), 120, "tombstone record must be <= 120 bytes, got %d: %s", len(b), b)
	})

	t.Run("invalid status returns ErrUnknownStatus", func(t *testing.T) {
		t.Parallel()
		r := asyncjob.Record{
			Tag:       "tag-bad-status",
			Status:    asyncjob.Status("evil"),
			CreatedAt: now,
			UpdatedAt: now,
		}
		_, err := r.Marshal()
		require.Error(t, err)
		assert.True(t, errors.Is(err, asyncjob.ErrUnknownStatus), "expected ErrUnknownStatus, got: %v", err)
	})
}

func TestStatus_String(t *testing.T) {
	t.Parallel()

	cases := []struct {
		status asyncjob.Status
		want   string
	}{
		{asyncjob.StatusPending, "pending"},
		{asyncjob.StatusCompleted, "completed"},
		{asyncjob.StatusFailed, "failed"},
		{asyncjob.StatusFailedTimeout, "failed_timeout"},
		{asyncjob.StatusTombstone, "tombstone"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.want, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, tc.status.String())
		})
	}
}

// sec returns t truncated to second precision (the wire format stores Unix
// seconds, so sub-second precision is lost on round-trip).
func sec(t time.Time) time.Time { return t.UTC().Truncate(time.Second) }

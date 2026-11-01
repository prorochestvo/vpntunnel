package asyncjob

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// compile-time assertions: panicStore and errorTransitionStore satisfy Store.
var _ Store = (*bboltStore)(nil)
var _ Store = (*panicStore)(nil)
var _ Store = (*errorTransitionStore)(nil)

func TestGC_Run(t *testing.T) {
	t.Parallel()

	const (
		pendingTimeout = 5 * time.Minute
		completeTTL    = 30 * time.Minute
		tombstoneTTL   = 24 * time.Hour
	)

	t.Run("ctx cancellation exits Run and returns ctx.Err", func(t *testing.T) {
		t.Parallel()
		s := openGCStore(t)
		ctx, cancel := context.WithCancel(t.Context())
		g := NewGC(GCConfig{
			Store:          s,
			Logger:         discardLogger(),
			PendingTimeout: pendingTimeout,
			CompleteTTL:    completeTTL,
			TombstoneTTL:   tombstoneTTL,
			Now:            time.Now,
			TickInterval:   50 * time.Millisecond,
		})
		done := make(chan error, 1)
		go func() { done <- g.Run(ctx) }()
		cancel()
		select {
		case err := <-done:
			assert.ErrorIs(t, err, context.Canceled)
		case <-time.After(2 * time.Second):
			t.Fatal("Run did not return within 2s after ctx cancellation")
		}
	})

	t.Run("Run executes initial pass before first tick", func(t *testing.T) {
		t.Parallel()
		s := openGCStore(t)

		base := time.Now().UTC().Truncate(time.Second)
		// seed a pending record old enough to be timed out.
		seedWithStatusAt(t, s, "pre-tick-tag", StatusPending, base.Add(-pendingTimeout-time.Second))

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		g := NewGC(GCConfig{
			Store:          s,
			Logger:         discardLogger(),
			PendingTimeout: pendingTimeout,
			CompleteTTL:    completeTTL,
			TombstoneTTL:   tombstoneTTL,
			Now:            fixedClock(base),
			// use a long tick so we can observe the pre-tick pass.
			TickInterval: 10 * time.Second,
		})

		done := make(chan error, 1)
		go func() { done <- g.Run(ctx) }()

		// wait briefly for the initial pass to execute.
		require.Eventually(t, func() bool {
			got, found, err := s.Get("pre-tick-tag")
			if err != nil || !found {
				return false
			}
			return got.Status == StatusFailedTimeout
		}, 2*time.Second, 20*time.Millisecond, "initial pass must transition pending record before first tick")

		cancel()
		<-done
	})
}
func TestGC_PassOnce(t *testing.T) {
	t.Parallel()

	const (
		pendingTimeout = 5 * time.Minute
		completeTTL    = 30 * time.Minute
		tombstoneTTL   = 24 * time.Hour
	)

	t.Run("empty bucket no-op emits heartbeat log", func(t *testing.T) {
		t.Parallel()
		s := openGCStore(t)
		now := time.Now().UTC()

		ch := &captureHandler{}
		g := NewGC(GCConfig{
			Store:          s,
			Logger:         slog.New(ch),
			PendingTimeout: pendingTimeout,
			CompleteTTL:    completeTTL,
			TombstoneTTL:   tombstoneTTL,
			Now:            fixedClock(now),
		})
		require.NotPanics(t, g.passOnce)

		recs := ch.all()
		require.Len(t, recs, 1, "exactly one log record must be emitted")
		assert.Equal(t, "async_gc_pass", recs[0].Message)
		assert.Equal(t, slog.LevelInfo, recs[0].Level)

		// verify all three counters are zero.
		var timedOut, tombstoned, deleted int
		recs[0].Attrs(func(a slog.Attr) bool {
			switch a.Key {
			case "timed_out":
				timedOut = int(a.Value.Int64())
			case "tombstoned":
				tombstoned = int(a.Value.Int64())
			case "deleted":
				deleted = int(a.Value.Int64())
			}
			return true
		})
		assert.Equal(t, 0, timedOut)
		assert.Equal(t, 0, tombstoned)
		assert.Equal(t, 0, deleted)
	})

	t.Run("pending past pending_timeout transitions to failed_timeout", func(t *testing.T) {
		t.Parallel()
		s := openGCStore(t)

		base := time.Now().UTC().Truncate(time.Second)
		// record is older than pendingTimeout.
		seedWithStatusAt(t, s, "tag-timeout", StatusPending, base.Add(-pendingTimeout-time.Second))

		now := base
		g := NewGC(GCConfig{
			Store:          s,
			Logger:         discardLogger(),
			PendingTimeout: pendingTimeout,
			CompleteTTL:    completeTTL,
			TombstoneTTL:   tombstoneTTL,
			Now:            fixedClock(now),
		})
		g.passOnce()

		got, found, err := s.Get("tag-timeout")
		require.NoError(t, err)
		require.True(t, found)
		assert.Equal(t, StatusFailedTimeout, got.Status)
		assert.Nil(t, got.UpstreamResponse, "payload must remain absent for pending→failed_timeout")
	})

	t.Run("pending before pending_timeout stays pending", func(t *testing.T) {
		t.Parallel()
		s := openGCStore(t)

		base := time.Now().UTC().Truncate(time.Second)
		// record is newer than pendingTimeout.
		seedWithStatusAt(t, s, "tag-young", StatusPending, base.Add(-pendingTimeout+time.Minute))

		g := NewGC(GCConfig{
			Store:          s,
			Logger:         discardLogger(),
			PendingTimeout: pendingTimeout,
			CompleteTTL:    completeTTL,
			TombstoneTTL:   tombstoneTTL,
			Now:            fixedClock(base),
		})
		g.passOnce()

		got, found, err := s.Get("tag-young")
		require.NoError(t, err)
		require.True(t, found)
		assert.Equal(t, StatusPending, got.Status)
	})

	t.Run("completed past complete_ttl tombstones and strips UpstreamResponse", func(t *testing.T) {
		t.Parallel()
		s := openGCStore(t)

		base := time.Now().UTC().Truncate(time.Second)
		seedWithStatusAt(t, s, "tag-completed", StatusCompleted, base.Add(-completeTTL-time.Second))

		g := NewGC(GCConfig{
			Store:          s,
			Logger:         discardLogger(),
			PendingTimeout: pendingTimeout,
			CompleteTTL:    completeTTL,
			TombstoneTTL:   tombstoneTTL,
			Now:            fixedClock(base),
		})
		g.passOnce()

		got, found, err := s.Get("tag-completed")
		require.NoError(t, err)
		require.True(t, found)
		assert.Equal(t, StatusTombstone, got.Status)
		assert.Nil(t, got.UpstreamResponse, "UpstreamResponse must be stripped on tombstone transition")
		assert.False(t, got.EvictedAt.IsZero(), "EvictedAt must be stamped")
	})

	t.Run("failed past complete_ttl tombstones", func(t *testing.T) {
		t.Parallel()
		s := openGCStore(t)

		base := time.Now().UTC().Truncate(time.Second)
		seedWithStatusAt(t, s, "tag-failed", StatusFailed, base.Add(-completeTTL-time.Second))

		g := NewGC(GCConfig{
			Store:          s,
			Logger:         discardLogger(),
			PendingTimeout: pendingTimeout,
			CompleteTTL:    completeTTL,
			TombstoneTTL:   tombstoneTTL,
			Now:            fixedClock(base),
		})
		g.passOnce()

		got, found, err := s.Get("tag-failed")
		require.NoError(t, err)
		require.True(t, found)
		assert.Equal(t, StatusTombstone, got.Status)
		assert.Nil(t, got.UpstreamResponse)
	})

	t.Run("failed_timeout past complete_ttl tombstones", func(t *testing.T) {
		t.Parallel()
		s := openGCStore(t)

		base := time.Now().UTC().Truncate(time.Second)
		// seed as pending first, then manually transition to failed_timeout via Put.
		rec := Record{
			Tag:       "tag-failed-timeout",
			Status:    StatusFailedTimeout,
			CreatedAt: base.Add(-completeTTL - time.Second),
			UpdatedAt: base.Add(-completeTTL - time.Second),
			UpstreamResponse: &UpstreamResponse{
				StatusCode: 502,
				Header:     nil,
				Body:       []byte("upstream error"),
			},
		}
		require.NoError(t, s.Put(rec.Tag, rec))

		g := NewGC(GCConfig{
			Store:          s,
			Logger:         discardLogger(),
			PendingTimeout: pendingTimeout,
			CompleteTTL:    completeTTL,
			TombstoneTTL:   tombstoneTTL,
			Now:            fixedClock(base),
		})
		g.passOnce()

		got, found, err := s.Get("tag-failed-timeout")
		require.NoError(t, err)
		require.True(t, found)
		assert.Equal(t, StatusTombstone, got.Status)
		assert.Nil(t, got.UpstreamResponse)
	})

	t.Run("tombstone past tombstone_ttl is deleted", func(t *testing.T) {
		t.Parallel()
		s := openGCStore(t)

		base := time.Now().UTC().Truncate(time.Second)
		seedTombstoneAt(t, s, "tag-tombstone", base.Add(-tombstoneTTL-time.Second))

		g := NewGC(GCConfig{
			Store:          s,
			Logger:         discardLogger(),
			PendingTimeout: pendingTimeout,
			CompleteTTL:    completeTTL,
			TombstoneTTL:   tombstoneTTL,
			Now:            fixedClock(base),
		})
		g.passOnce()

		_, found, err := s.Get("tag-tombstone")
		require.NoError(t, err)
		assert.False(t, found, "tombstone past TTL must be deleted from the store")
	})

	t.Run("three records straddling all three transitions in one pass observe correct final states", func(t *testing.T) {
		t.Parallel()
		s := openGCStore(t)

		base := time.Now().UTC().Truncate(time.Second)

		// record 1: pending old enough to time out.
		seedWithStatusAt(t, s, "rec-pending", StatusPending, base.Add(-pendingTimeout-time.Second))

		// record 2: completed old enough to tombstone.
		seedWithStatusAt(t, s, "rec-completed", StatusCompleted, base.Add(-completeTTL-time.Second))

		// record 3: tombstone old enough to delete.
		seedTombstoneAt(t, s, "rec-tombstone", base.Add(-tombstoneTTL-time.Second))

		// record 4: pending with age that exceeds BOTH pendingTimeout AND
		// completeTTL. Batch 1 stamps UpdatedAt=now; batch 2 then sees a freshly
		// stamped record and must NOT transition it to tombstone in the same pass.
		seedWithStatusAt(t, s, "rec-double-age", StatusPending, base.Add(-completeTTL-time.Second))

		g := NewGC(GCConfig{
			Store:          s,
			Logger:         discardLogger(),
			PendingTimeout: pendingTimeout,
			CompleteTTL:    completeTTL,
			TombstoneTTL:   tombstoneTTL,
			Now:            fixedClock(base),
		})
		g.passOnce()

		// rec-pending must now be failed_timeout, not tombstone — one pass only
		// advances it one state at a time per the fixed batch ordering.
		gotPending, found, err := s.Get("rec-pending")
		require.NoError(t, err)
		require.True(t, found, "rec-pending must still exist")
		assert.Equal(t, StatusFailedTimeout, gotPending.Status,
			"pending→failed_timeout in pass 1; tombstone must wait for pass 2")

		// rec-completed must now be tombstone with stripped payload.
		gotCompleted, found, err := s.Get("rec-completed")
		require.NoError(t, err)
		require.True(t, found, "rec-completed must exist as tombstone")
		assert.Equal(t, StatusTombstone, gotCompleted.Status)
		assert.Nil(t, gotCompleted.UpstreamResponse)

		// rec-tombstone must be deleted.
		_, found, err = s.Get("rec-tombstone")
		require.NoError(t, err)
		assert.False(t, found, "tombstone past TTL must be deleted")

		// rec-double-age: batch 1 transitioned it to failed_timeout with
		// UpdatedAt=now. Batch 2 sees UpdatedAt=now, which is NOT old enough
		// for completeTTL, so it must stay at failed_timeout this pass.
		gotDouble, found, err := s.Get("rec-double-age")
		require.NoError(t, err)
		require.True(t, found)
		assert.Equal(t, StatusFailedTimeout, gotDouble.Status,
			"double-age pending must reach failed_timeout, not skip directly to tombstone")
		assert.Nil(t, gotDouble.UpstreamResponse)
	})

	t.Run("panic in batch1 is recovered, heartbeat emits, batch3 still runs", func(t *testing.T) {
		t.Parallel()
		delegate := openGCStore(t)
		ps := &panicStore{delegate: delegate, panicOnce: true}

		base := time.Now().UTC().Truncate(time.Second)

		// seed a tombstone old enough that batch 3 (delete) would remove it —
		// this lets us verify batch 3 ran despite the batch 1 panic.
		seedTombstoneAt(t, delegate, "rec-tombstone-delete", base.Add(-tombstoneTTL-time.Second))

		ch := &captureHandler{}
		g := NewGC(GCConfig{
			Store:          ps,
			Logger:         slog.New(ch),
			PendingTimeout: pendingTimeout,
			CompleteTTL:    completeTTL,
			TombstoneTTL:   tombstoneTTL,
			Now:            fixedClock(base),
		})

		require.NotPanics(t, g.passOnce, "passOnce must recover from BatchTransition panic")
		assert.True(t, ps.panicCalled, "panic must have been triggered in batch 1")

		// heartbeat must always fire, even when batch 1 panicked.
		recs := ch.all()
		var heartbeat *slog.Record
		for i := range recs {
			if recs[i].Message == "async_gc_pass" {
				heartbeat = &recs[i]
				break
			}
		}
		require.NotNil(t, heartbeat, "heartbeat log (async_gc_pass) must emit even after batch panic")

		// batch 3 (delete) must have still run — tombstone record gone.
		_, found, err := delegate.Get("rec-tombstone-delete")
		require.NoError(t, err)
		assert.False(t, found, "batch3 (delete) must run even when batch1 panicked")
	})

	t.Run("error in batch1 does not abort batch3 (delete still runs)", func(t *testing.T) {
		t.Parallel()
		delegate := openGCStore(t)
		es := &errorTransitionStore{delegate: delegate, errOnce: true}

		base := time.Now().UTC().Truncate(time.Second)
		// seed a tombstone past tombstone_ttl so batch 3 would delete it.
		seedTombstoneAt(t, delegate, "rec-delete", base.Add(-tombstoneTTL-time.Second))

		g := NewGC(GCConfig{
			Store:          es,
			Logger:         discardLogger(),
			PendingTimeout: pendingTimeout,
			CompleteTTL:    completeTTL,
			TombstoneTTL:   tombstoneTTL,
			Now:            fixedClock(base),
		})
		require.NotPanics(t, g.passOnce)
		assert.True(t, es.errTriggered, "error must have been triggered in batch 1")

		_, found, err := delegate.Get("rec-delete")
		require.NoError(t, err)
		assert.False(t, found, "delete batch must run even when transition batch errors")
	})
}

// panicStore is a test-only fake that panics on the first call to
// BatchTransition and then delegates to a real store for subsequent calls.
// This lets us verify that a batch-level panic is recovered and the remaining
// batches in the same pass still execute.
type panicStore struct {
	delegate    Store
	panicOnce   bool
	panicCalled bool
}

func (p *panicStore) Get(tag string) (Record, bool, error) { return p.delegate.Get(tag) }
func (p *panicStore) Put(tag string, rec Record) error     { return p.delegate.Put(tag, rec) }
func (p *panicStore) CompareAndSwapStatus(tag string, expected Status, mutate func(Record) Record) (Record, error) {
	return p.delegate.CompareAndSwapStatus(tag, expected, mutate)
}
func (p *panicStore) Counts() (JobCounts, error) { return p.delegate.Counts() }
func (p *panicStore) Close() error               { return p.delegate.Close() }

func (p *panicStore) BatchTransition(filter func(Record) bool, mutate func(Record) Record) (int, error) {
	if p.panicOnce && !p.panicCalled {
		p.panicCalled = true
		panic("injected test panic")
	}
	return p.delegate.BatchTransition(filter, mutate)
}

func (p *panicStore) BatchDelete(filter func(Record) bool) (int, error) {
	return p.delegate.BatchDelete(filter)
}

// errorTransitionStore is a test-only fake that returns an error on the first
// BatchTransition call only, then delegates to the real store. This lets us
// verify that a batch-level error does not abort subsequent batches.
type errorTransitionStore struct {
	delegate     Store
	errOnce      bool
	errTriggered bool
}

func (e *errorTransitionStore) Get(tag string) (Record, bool, error) { return e.delegate.Get(tag) }
func (e *errorTransitionStore) Put(tag string, rec Record) error     { return e.delegate.Put(tag, rec) }
func (e *errorTransitionStore) CompareAndSwapStatus(tag string, expected Status, mutate func(Record) Record) (Record, error) {
	return e.delegate.CompareAndSwapStatus(tag, expected, mutate)
}
func (e *errorTransitionStore) Counts() (JobCounts, error) { return e.delegate.Counts() }
func (e *errorTransitionStore) Close() error               { return e.delegate.Close() }

func (e *errorTransitionStore) BatchTransition(filter func(Record) bool, mutate func(Record) Record) (int, error) {
	if e.errOnce && !e.errTriggered {
		e.errTriggered = true
		return 0, errors.New("injected batch transition error")
	}
	return e.delegate.BatchTransition(filter, mutate)
}

func (e *errorTransitionStore) BatchDelete(filter func(Record) bool) (int, error) {
	return e.delegate.BatchDelete(filter)
}

// captureHandler is a test-only slog.Handler that records every log record.
type captureHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *captureHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }
func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r)
	return nil
}
func (h *captureHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(_ string) slog.Handler      { return h }

func (h *captureHandler) all() []slog.Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]slog.Record, len(h.records))
	copy(out, h.records)
	return out
}

// openGCStore creates a bbolt-backed Store in a fresh temp directory.
func openGCStore(t *testing.T) Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "jobs.db")
	s, err := NewStore(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	return s
}

// fixedClock returns a func() time.Time that always returns t.
func fixedClock(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

// seedWithStatus creates a Record in the given status directly via Put.
// For tombstone records, EvictedAt must be set by the caller.
func seedWithStatusAt(t *testing.T, s Store, tag string, status Status, updatedAt time.Time) Record {
	t.Helper()
	rec := Record{
		Tag:       tag,
		Status:    status,
		CreatedAt: updatedAt,
		UpdatedAt: updatedAt,
	}
	if status == StatusCompleted || status == StatusFailed || status == StatusFailedTimeout {
		rec.UpstreamResponse = &UpstreamResponse{
			StatusCode: 200,
			Header:     http.Header{"X-Test": []string{"1"}},
			Body:       []byte("body"),
		}
	}
	require.NoError(t, s.Put(tag, rec))
	return rec
}

// seedTombstoneAt creates a tombstone record with the given evictedAt time.
func seedTombstoneAt(t *testing.T, s Store, tag string, evictedAt time.Time) Record {
	t.Helper()
	rec := Record{
		Tag:       tag,
		Status:    StatusTombstone,
		CreatedAt: evictedAt,
		UpdatedAt: evictedAt,
		EvictedAt: evictedAt,
	}
	require.NoError(t, s.Put(tag, rec))
	return rec
}

// discardLogger returns a *slog.Logger that discards all output. Keeps test
// output clean while still exercising the log paths.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(&bytes.Buffer{}, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

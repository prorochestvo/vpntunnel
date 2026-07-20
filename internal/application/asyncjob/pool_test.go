package asyncjob_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vpntunnel/internal/application/asyncjob"
)

// compile-time assertion that fakeForwarder satisfies asyncjob.Forwarder.
var _ asyncjob.Forwarder = (asyncjob.ForwarderFunc)(nil)
var _ asyncjob.Forwarder = (*fakeForwarder)(nil)

// fakeForwarder is a test double for asyncjob.Forwarder.
// When block is non-nil the goroutine waits for it to be closed before
// returning. When errOut is non-nil it is returned as the forwarding error.
type fakeForwarder struct {
	mu     sync.Mutex
	block  chan struct{} // if non-nil, Forward waits until this is closed
	resp   asyncjob.UpstreamResponse
	errOut error
	calls  int
}

func (f *fakeForwarder) Forward(_ context.Context, req *http.Request) (asyncjob.UpstreamResponse, error) {
	// always drain + close the body as the contract requires.
	if req.Body != nil {
		_, _ = io.Copy(io.Discard, req.Body)
		_ = req.Body.Close()
	}

	f.mu.Lock()
	blk := f.block
	f.calls++
	f.mu.Unlock()

	if blk != nil {
		<-blk
	}

	f.mu.Lock()
	resp := f.resp
	err := f.errOut
	f.mu.Unlock()
	return resp, err
}

func (f *fakeForwarder) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// compile-time assertion that blockingForwarder satisfies asyncjob.Forwarder.
var _ asyncjob.Forwarder = (*blockingForwarder)(nil)

// blockingForwarder blocks until either the block channel is closed OR the
// request context is cancelled. It models a real upstream call that honours
// context cancellation so the pool's ctx cancel propagates correctly.
type blockingForwarder struct {
	block chan struct{}
}

func (b *blockingForwarder) Forward(ctx context.Context, req *http.Request) (asyncjob.UpstreamResponse, error) {
	if req.Body != nil {
		_, _ = io.Copy(io.Discard, req.Body)
		_ = req.Body.Close()
	}
	select {
	case <-b.block:
		return asyncjob.UpstreamResponse{StatusCode: 200}, nil
	case <-ctx.Done():
		return asyncjob.UpstreamResponse{}, ctx.Err()
	}
}

// newPool creates a Pool backed by a temp bbolt store and registers cleanup
// via t.Cleanup. The pool's Shutdown is NOT called in cleanup — tests that
// care about cleanup do it explicitly so they can inspect the result.
func newPool(t *testing.T, fwd asyncjob.Forwarder, maxConcurrent int) (*asyncjob.Pool, asyncjob.Store) {
	t.Helper()
	if maxConcurrent == 0 {
		maxConcurrent = 4
	}
	path := filepath.Join(t.TempDir(), "jobs.db")
	store, err := asyncjob.NewStore(path)
	require.NoError(t, err, "NewStore must succeed for temp file")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	pool := asyncjob.NewPool(store, fwd, maxConcurrent, logger)
	return pool, store
}

// drainPool shuts the pool down with a 3-second timeout and asserts no error.
func drainPool(t *testing.T, pool *asyncjob.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	require.NoError(t, pool.Shutdown(ctx))
}

// makeReq builds a minimal *http.Request suitable for passing to SubmitOrFetch.
func makeReq(t *testing.T, body string) *http.Request {
	t.Helper()
	var bodyReader io.ReadCloser
	if body != "" {
		bodyReader = io.NopCloser(strings.NewReader(body))
	} else {
		bodyReader = http.NoBody
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://example.com/", bodyReader)
	require.NoError(t, err)
	return req
}

// waitForStatus polls the store until the record for tag reaches wantStatus or
// the deadline expires.
func waitForStatus(t *testing.T, store asyncjob.Store, tag string, wantStatus asyncjob.Status, timeout time.Duration) asyncjob.Record {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		rec, found, err := store.Get(tag)
		require.NoError(t, err)
		if found && rec.Status == wantStatus {
			return rec
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for tag %q to reach status %q", tag, wantStatus)
	return asyncjob.Record{}
}

func TestPool_SubmitOrFetch(t *testing.T) {
	t.Parallel()

	t.Run("first submit creates pending and returns OutcomePending", func(t *testing.T) {
		t.Parallel()

		fwd := &fakeForwarder{block: make(chan struct{})}
		pool, store := newPool(t, fwd, 4)

		outcome, err := pool.SubmitOrFetch(t.Context(), "tag-first", makeReq(t, "body"))
		require.NoError(t, err)
		assert.IsType(t, asyncjob.OutcomePending{}, outcome)

		rec, found, err := store.Get("tag-first")
		require.NoError(t, err)
		require.True(t, found, "pending record must exist in store after submit")
		assert.Equal(t, asyncjob.StatusPending, rec.Status)

		close(fwd.block)
		drainPool(t, pool)
		require.NoError(t, store.Close())
	})

	t.Run("second submit with same tag while in-flight returns OutcomePending without dispatching a second worker", func(t *testing.T) {
		t.Parallel()

		fwd := &fakeForwarder{block: make(chan struct{})}
		pool, store := newPool(t, fwd, 4)

		outcome1, err := pool.SubmitOrFetch(t.Context(), "tag-inflight", makeReq(t, ""))
		require.NoError(t, err)
		assert.IsType(t, asyncjob.OutcomePending{}, outcome1)

		// second submit for the same in-flight tag — must not dispatch another worker.
		outcome2, err := pool.SubmitOrFetch(t.Context(), "tag-inflight", makeReq(t, ""))
		require.NoError(t, err)
		assert.IsType(t, asyncjob.OutcomePending{}, outcome2)

		close(fwd.block)
		drainPool(t, pool)
		require.NoError(t, store.Close())

		assert.Equal(t, 1, fwd.callCount(), "Forwarder must be called exactly once for the same tag")
	})

	t.Run("completion path transitions record to completed and stores the response", func(t *testing.T) {
		t.Parallel()

		wantResp := asyncjob.UpstreamResponse{StatusCode: 200, Body: []byte("hello")}
		fwd := &fakeForwarder{resp: wantResp}
		pool, store := newPool(t, fwd, 4)

		_, err := pool.SubmitOrFetch(t.Context(), "tag-completion", makeReq(t, "body"))
		require.NoError(t, err)

		rec := waitForStatus(t, store, "tag-completion", asyncjob.StatusCompleted, 3*time.Second)
		require.NotNil(t, rec.UpstreamResponse)
		assert.Equal(t, 200, rec.UpstreamResponse.StatusCode)
		assert.Equal(t, []byte("hello"), rec.UpstreamResponse.Body)

		drainPool(t, pool)
		require.NoError(t, store.Close())
	})

	t.Run("retry after completion returns OutcomeCompleted with the upstream response", func(t *testing.T) {
		t.Parallel()

		wantResp := asyncjob.UpstreamResponse{StatusCode: 201, Body: []byte("created")}
		fwd := &fakeForwarder{resp: wantResp}
		pool, store := newPool(t, fwd, 4)

		_, err := pool.SubmitOrFetch(t.Context(), "tag-retry", makeReq(t, ""))
		require.NoError(t, err)

		// wait for the worker to finish.
		waitForStatus(t, store, "tag-retry", asyncjob.StatusCompleted, 3*time.Second)

		// second call with the same tag — must return OutcomeCompleted.
		outcome, err := pool.SubmitOrFetch(t.Context(), "tag-retry", makeReq(t, ""))
		require.NoError(t, err)
		completed, ok := outcome.(asyncjob.OutcomeCompleted)
		require.True(t, ok, "expected OutcomeCompleted, got %T", outcome)
		assert.Equal(t, 201, completed.Response.StatusCode)
		assert.Equal(t, []byte("created"), completed.Response.Body)

		drainPool(t, pool)
		require.NoError(t, store.Close())
	})

	t.Run("tombstoned tag returns OutcomeTombstoned", func(t *testing.T) {
		t.Parallel()

		fwd := &fakeForwarder{}
		pool, store := newPool(t, fwd, 4)

		// plant a tombstone record directly in the store.
		tombstone := asyncjob.Record{
			Tag:       "tag-tombstone",
			Status:    asyncjob.StatusTombstone,
			CreatedAt: time.Now().UTC(),
			UpdatedAt: time.Now().UTC(),
			EvictedAt: time.Now().UTC(),
		}
		require.NoError(t, store.Put("tag-tombstone", tombstone))

		outcome, err := pool.SubmitOrFetch(t.Context(), "tag-tombstone", makeReq(t, ""))
		require.NoError(t, err)
		assert.IsType(t, asyncjob.OutcomeTombstoned{}, outcome)
		assert.Equal(t, 0, fwd.callCount(), "Forwarder must not be called for a tombstoned tag")

		drainPool(t, pool)
		require.NoError(t, store.Close())
	})

	t.Run("queue full at maxConcurrent returns OutcomeQueueFull without writing a pending record", func(t *testing.T) {
		t.Parallel()

		// one slot only; block the worker so we can fill it.
		block := make(chan struct{})
		fwd := &fakeForwarder{block: block}
		pool, store := newPool(t, fwd, 1)

		// fill the single slot.
		outcome1, err := pool.SubmitOrFetch(t.Context(), "tag-slot", makeReq(t, ""))
		require.NoError(t, err)
		assert.IsType(t, asyncjob.OutcomePending{}, outcome1)

		// second submit must see a full queue and must NOT write a record.
		outcome2, err := pool.SubmitOrFetch(t.Context(), "tag-overflow", makeReq(t, ""))
		require.NoError(t, err)
		assert.IsType(t, asyncjob.OutcomeQueueFull{}, outcome2)

		_, found, err := store.Get("tag-overflow")
		require.NoError(t, err)
		assert.False(t, found, "no pending record must be written for a queue-full outcome")

		close(block)
		drainPool(t, pool)
		require.NoError(t, store.Close())
	})

	t.Run("forwarder error transitions record to failed with synthetic 502 response", func(t *testing.T) {
		t.Parallel()

		fwd := &fakeForwarder{errOut: errors.New("upstream dial failed")}
		pool, store := newPool(t, fwd, 4)

		_, err := pool.SubmitOrFetch(t.Context(), "tag-fwd-error", makeReq(t, ""))
		require.NoError(t, err)

		rec := waitForStatus(t, store, "tag-fwd-error", asyncjob.StatusFailed, 3*time.Second)
		require.NotNil(t, rec.UpstreamResponse, "UpstreamResponse must be populated on failure")
		assert.Equal(t, 502, rec.UpstreamResponse.StatusCode)
		assert.Equal(t, []byte("upstream error"), rec.UpstreamResponse.Body)
		// the raw error string must not appear in the synthetic body.
		assert.NotContains(t, string(rec.UpstreamResponse.Body), "dial")

		drainPool(t, pool)
		require.NoError(t, store.Close())
	})

	t.Run("body over 10 MB returns ErrBodyTooLarge and no slot is held", func(t *testing.T) {
		t.Parallel()

		fwd := &fakeForwarder{}
		// only 1 slot so we can verify the slot is released on error.
		pool, store := newPool(t, fwd, 1)

		// 10 MB + 1 byte exceeds the cap.
		oversize := bytes.Repeat([]byte("x"), 10*1024*1024+1)
		req := makeReq(t, "")
		req.Body = io.NopCloser(bytes.NewReader(oversize))

		_, err := pool.SubmitOrFetch(t.Context(), "tag-body-too-large", req)
		require.ErrorIs(t, err, asyncjob.ErrBodyTooLarge)

		// the slot must have been released; a subsequent submit must succeed.
		block := make(chan struct{})
		fwd.mu.Lock()
		fwd.block = block
		fwd.mu.Unlock()

		outcome, err := pool.SubmitOrFetch(t.Context(), "tag-after-large", makeReq(t, "small"))
		require.NoError(t, err)
		assert.IsType(t, asyncjob.OutcomePending{}, outcome, "slot must be free after ErrBodyTooLarge")

		// no record must be written for the oversized tag.
		_, found, err := store.Get("tag-body-too-large")
		require.NoError(t, err)
		assert.False(t, found, "no record must be written when body exceeds cap")

		close(block)
		drainPool(t, pool)
		require.NoError(t, store.Close())
	})

	t.Run("pre-existing failed record returns OutcomeCompleted with the stored response", func(t *testing.T) {
		t.Parallel()

		fwd := &fakeForwarder{}
		pool, store := newPool(t, fwd, 4)

		storedResp := &asyncjob.UpstreamResponse{StatusCode: 502, Body: []byte("upstream error")}
		rec := asyncjob.Record{
			Tag:              "tag-pre-failed",
			Status:           asyncjob.StatusFailed,
			CreatedAt:        time.Now().UTC(),
			UpdatedAt:        time.Now().UTC(),
			UpstreamResponse: storedResp,
		}
		require.NoError(t, store.Put("tag-pre-failed", rec))

		outcome, err := pool.SubmitOrFetch(t.Context(), "tag-pre-failed", makeReq(t, ""))
		require.NoError(t, err)
		completed, ok := outcome.(asyncjob.OutcomeCompleted)
		require.True(t, ok, "expected OutcomeCompleted for a failed record, got %T", outcome)
		assert.Equal(t, 502, completed.Response.StatusCode)
		assert.Equal(t, []byte("upstream error"), completed.Response.Body)
		assert.Equal(t, 0, fwd.callCount(), "Forwarder must not be called for a pre-existing failed record")

		drainPool(t, pool)
		require.NoError(t, store.Close())
	})

	t.Run("pre-existing failed_timeout record returns OutcomeCompleted with the stored response", func(t *testing.T) {
		t.Parallel()

		fwd := &fakeForwarder{}
		pool, store := newPool(t, fwd, 4)

		storedResp := &asyncjob.UpstreamResponse{StatusCode: 502, Body: []byte("upstream error")}
		rec := asyncjob.Record{
			Tag:              "tag-pre-failed-timeout",
			Status:           asyncjob.StatusFailedTimeout,
			CreatedAt:        time.Now().UTC(),
			UpdatedAt:        time.Now().UTC(),
			UpstreamResponse: storedResp,
		}
		require.NoError(t, store.Put("tag-pre-failed-timeout", rec))

		outcome, err := pool.SubmitOrFetch(t.Context(), "tag-pre-failed-timeout", makeReq(t, ""))
		require.NoError(t, err)
		completed, ok := outcome.(asyncjob.OutcomeCompleted)
		require.True(t, ok, "expected OutcomeCompleted for a failed_timeout record, got %T", outcome)
		assert.Equal(t, 502, completed.Response.StatusCode)
		assert.Equal(t, []byte("upstream error"), completed.Response.Body)
		assert.Equal(t, 0, fwd.callCount(), "Forwarder must not be called for a pre-existing failed_timeout record")

		drainPool(t, pool)
		require.NoError(t, store.Close())
	})

	t.Run("SubmitOrFetch after Shutdown returns ErrPoolClosed", func(t *testing.T) {
		t.Parallel()

		fwd := &fakeForwarder{}
		pool, store := newPool(t, fwd, 4)

		drainPool(t, pool)

		_, err := pool.SubmitOrFetch(t.Context(), "tag-after-shutdown", makeReq(t, ""))
		require.ErrorIs(t, err, asyncjob.ErrPoolClosed)

		require.NoError(t, store.Close())
	})
}

func TestPool_Shutdown(t *testing.T) {
	t.Parallel()

	t.Run("Shutdown waits for in-flight workers to drain", func(t *testing.T) {
		t.Parallel()

		block := make(chan struct{})
		wantResp := asyncjob.UpstreamResponse{StatusCode: 200, Body: []byte("ok")}
		fwd := &fakeForwarder{block: block, resp: wantResp}
		pool, store := newPool(t, fwd, 4)

		_, err := pool.SubmitOrFetch(t.Context(), "tag-drain", makeReq(t, "body"))
		require.NoError(t, err)

		// unblock the worker after a short delay so Shutdown must actually wait.
		go func() {
			time.Sleep(50 * time.Millisecond)
			close(block)
		}()

		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		shutErr := pool.Shutdown(ctx)
		require.NoError(t, shutErr, "Shutdown must return nil when workers drain in time")

		// after Shutdown the record must be in completed state.
		rec, found, err := store.Get("tag-drain")
		require.NoError(t, err)
		require.True(t, found)
		assert.Equal(t, asyncjob.StatusCompleted, rec.Status)

		require.NoError(t, store.Close())
	})

	t.Run("Shutdown with cancelled ctx returns ctx.Err()", func(t *testing.T) {
		t.Parallel()

		// block channel that is never explicitly closed —
		// the worker exits only when the pool's internal ctx is cancelled.
		block := make(chan struct{})
		fwd := &blockingForwarder{block: block}
		pool, store := newPool(t, fwd, 4)

		_, err := pool.SubmitOrFetch(t.Context(), "tag-cancel", makeReq(t, ""))
		require.NoError(t, err)

		// already-expired context — Shutdown should return immediately.
		deadCtx, deadCancel := context.WithCancel(context.Background())
		deadCancel() // cancelled before Shutdown is called

		shutErr := pool.Shutdown(deadCtx)
		require.ErrorIs(t, shutErr, context.Canceled)

		// Shutdown cancelled the pool's internal ctx, so the blocked worker will
		// exit via ctx.Done(). Because Shutdown is now idempotent (sync.Once), a
		// second call would return the cached context.Canceled — we cannot use
		// drainPool here. Give the worker a short grace period to finish on its
		// own before closing the store.
		time.Sleep(200 * time.Millisecond)
		require.NoError(t, store.Close())
	})

	t.Run("Shutdown is idempotent — second call returns same error without panic", func(t *testing.T) {
		t.Parallel()

		fwd := &fakeForwarder{}
		pool, store := newPool(t, fwd, 4)

		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()

		err1 := pool.Shutdown(ctx)
		require.NoError(t, err1, "first Shutdown must succeed")

		// second call must return the same result (nil) without panicking or
		// spawning a second wg.Wait goroutine.
		err2 := pool.Shutdown(ctx)
		require.NoError(t, err2, "second Shutdown must return the same nil error")

		require.NoError(t, store.Close())
	})
}

// TestPool_GoroutineLeak verifies that no goroutines leak after Shutdown. This
// test is NOT marked parallel so the NumGoroutine baseline is stable — parallel
// siblings would otherwise skew the count unpredictably. The baseline is
// captured BEFORE newPool so that bbolt's own internal goroutines are not
// counted as pool-introduced goroutines; the post-drain delta reflects only
// what the pool started.
func TestPool_GoroutineLeak(t *testing.T) {
	baseline := runtime.NumGoroutine()

	block := make(chan struct{})
	fwd := &fakeForwarder{block: block}
	pool, store := newPool(t, fwd, 4)

	_, err := pool.SubmitOrFetch(t.Context(), "tag-leak-check", makeReq(t, ""))
	require.NoError(t, err)

	// goroutine count should be elevated while the worker is blocked.
	close(block)
	drainPool(t, pool)
	require.NoError(t, store.Close())

	// brief yield so the runtime can reclaim goroutines.
	time.Sleep(30 * time.Millisecond)
	after := runtime.NumGoroutine()
	assert.InDelta(t, baseline, after, 2, "goroutine leak: baseline=%d after=%d", baseline, after)
}

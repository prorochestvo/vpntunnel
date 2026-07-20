package asyncjob

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// ErrBodyTooLarge is returned by SubmitOrFetch when the inbound request body
// exceeds the 10 MB cap enforced before the body bytes are handed to the
// worker goroutine. The error is wrapped with the (truncated) tag so callers
// can use errors.Is to match on it while still getting tag context from the
// message.
var ErrBodyTooLarge = errors.New("asyncjob: request body exceeds 10 MB limit")

// ErrPoolClosed is returned by SubmitOrFetch when the pool's context has
// already been cancelled (i.e. Shutdown has been called). Dispatching a new
// worker after cancellation would be immediately doomed.
var ErrPoolClosed = errors.New("asyncjob: pool is shut down")

// Outcome is a sealed discriminated union returned by SubmitOrFetch. The only
// valid concrete types are OutcomePending, OutcomeCompleted, OutcomeTombstoned,
// and OutcomeQueueFull. The unexported method prevents external packages from
// adding new variants.
type Outcome interface{ isOutcome() }

// OutcomePending is returned when the job has been accepted but the upstream
// has not yet responded, or when a second submit arrives for the same tag
// while the first worker is still in-flight.
type OutcomePending struct{}

func (OutcomePending) isOutcome() {}

// OutcomeCompleted is returned when the upstream has already responded
// (status completed, failed, or failed_timeout). Response holds the stored
// upstream result.
type OutcomeCompleted struct{ Response UpstreamResponse }

func (OutcomeCompleted) isOutcome() {}

// OutcomeTombstoned is returned when the tag has been logically deleted. The
// caller should treat the job as permanently gone.
type OutcomeTombstoned struct{}

func (OutcomeTombstoned) isOutcome() {}

// OutcomeQueueFull is returned when all semaphore slots are occupied and the
// new job cannot be accepted. No record is written; no slot is held.
type OutcomeQueueFull struct{}

func (OutcomeQueueFull) isOutcome() {}

// Forwarder executes a single proxy request against the upstream and returns
// the materialised response. The pool owns req.Body; the implementation MUST
// close it. The ctx passed here is the pool's own context (not the inbound
// request ctx, which may already be cancelled by the time the worker runs).
type Forwarder interface {
	Forward(ctx context.Context, req *http.Request) (UpstreamResponse, error)
}

// ForwarderFunc is an adapter that lets a plain function satisfy Forwarder.
// It is intended for wiring the sync tunnelForwarder into the async pool in
// main.go via a closure that captures the pre-resolved tunnel ID, dialer, and
// resolver.
type ForwarderFunc func(ctx context.Context, req *http.Request) (UpstreamResponse, error)

// Forward calls f.
func (f ForwarderFunc) Forward(ctx context.Context, req *http.Request) (UpstreamResponse, error) {
	return f(ctx, req)
}

// Pool is a semaphore-bounded async job executor. It accepts inbound HTTP
// requests keyed by a retry-tag, dispatches them to a Forwarder on a
// background goroutine, and persists the result in a Store. Callers retrieve
// previously submitted jobs by passing the same tag again via SubmitOrFetch.
//
// Pool must be created via NewPool. The zero value is not usable. Caller must
// call Shutdown when done to drain in-flight workers.
type Pool struct {
	store        Store
	forwarder    Forwarder
	slots        chan struct{}
	logger       *slog.Logger
	wg           sync.WaitGroup
	ctx          context.Context //nolint:containedctx // pool owns its lifetime ctx
	cancel       context.CancelFunc
	shutdownOnce sync.Once
	shutdownErr  error
}

// NewPool creates a Pool that forwards requests via forwarder, persists state
// in store, and limits concurrency to maxConcurrent in-flight jobs. logger is
// used for internal diagnostic messages; pass slog.Default() when no custom
// logger is required.
//
// The pool creates its own context via context.WithCancel(context.Background()).
// That context is cancelled when Shutdown is called. The caller must call
// Shutdown to release goroutines started by the pool.
func NewPool(store Store, forwarder Forwarder, maxConcurrent int, logger *slog.Logger) *Pool {
	if logger == nil {
		logger = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Pool{
		store:     store,
		forwarder: forwarder,
		slots:     make(chan struct{}, maxConcurrent),
		logger:    logger,
		ctx:       ctx,
		cancel:    cancel,
	}
}

// SubmitOrFetch either fetches the current state for tag or submits req as a
// new async job. The four possible outcomes are:
//
//   - OutcomePending  — job is in-flight (existing or freshly submitted).
//   - OutcomeCompleted — job finished; Response holds the upstream result.
//   - OutcomeTombstoned — tag was logically deleted; treat as permanently gone.
//   - OutcomeQueueFull — all semaphore slots are occupied; caller should retry.
//
// When a new job is accepted SubmitOrFetch drains req.Body synchronously (up
// to 10 MB) and returns before the upstream call completes. The pool owns
// req.Body after the call returns; callers must not close or read it again.
//
// Returns ErrBodyTooLarge when the body exceeds 10 MB. Returns a wrapped
// error for store failures or body-drain failures.
func (p *Pool) SubmitOrFetch(ctx context.Context, tag string, req *http.Request) (Outcome, error) {
	if p.ctx.Err() != nil {
		return nil, ErrPoolClosed
	}

	rec, found, err := p.store.Get(tag)
	if err != nil {
		return nil, fmt.Errorf("asyncjob: get tag: %w", err)
	}
	if found {
		return outcomeForRecord(rec), nil
	}

	// try to claim a slot without blocking.
	select {
	case p.slots <- struct{}{}:
	default:
		return OutcomeQueueFull{}, nil
	}

	// slot is claimed from here; release it on any early-return error path.
	body, err := drainBody(req)
	if err != nil {
		<-p.slots
		if errors.Is(err, ErrBodyTooLarge) {
			echo := tag
			if len(echo) > maxTagEchoLenLog {
				echo = echo[:maxTagEchoLenLog]
			}
			return nil, fmt.Errorf("asyncjob: tag %q: %w", echo, ErrBodyTooLarge)
		}
		return nil, fmt.Errorf("asyncjob: drain request body: %w", err)
	}

	pending := NewPendingRecord(tag, time.Now())
	if err := p.store.Put(tag, pending); err != nil {
		<-p.slots
		return nil, fmt.Errorf("asyncjob: write pending record: %w", err)
	}

	freshReq := cloneRequest(p.ctx, req, body)

	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		defer func() { <-p.slots }()
		p.runWorker(tag, freshReq)
	}()

	return OutcomePending{}, nil
}

// Shutdown cancels the pool's internal context (signalling all in-flight
// forwarder calls) and waits for all worker goroutines to exit. If the
// supplied ctx expires before the workers drain, Shutdown returns ctx.Err().
// Shutdown is idempotent: subsequent calls return the same error as the first
// call without blocking.
func (p *Pool) Shutdown(ctx context.Context) error {
	p.shutdownOnce.Do(func() {
		p.cancel()
		done := make(chan struct{})
		go func() { p.wg.Wait(); close(done) }()
		select {
		case <-done:
			p.shutdownErr = nil
		case <-ctx.Done():
			p.shutdownErr = ctx.Err()
		}
	})
	return p.shutdownErr
}

// maxBodyBytes is the hard cap on the request body size accepted by SubmitOrFetch.
const maxBodyBytes = 10 * 1024 * 1024 // 10 MB

// maxTagEchoLenLog caps the number of bytes echoed from a tag in pool log
// messages to prevent a large client-supplied tag from flooding the log.
const maxTagEchoLenLog = 64

// outcomeForRecord maps an existing Record to the appropriate Outcome variant.
func outcomeForRecord(rec Record) Outcome {
	switch rec.Status {
	case StatusPending:
		return OutcomePending{}
	case StatusCompleted, StatusFailed, StatusFailedTimeout:
		resp := UpstreamResponse{}
		if rec.UpstreamResponse != nil {
			resp = *rec.UpstreamResponse
		}
		return OutcomeCompleted{Response: resp}
	case StatusTombstone:
		return OutcomeTombstoned{}
	default:
		// validateStatus rejects unknown values at unmarshal time, so this
		// branch should be unreachable in practice.
		return OutcomePending{}
	}
}

// drainBody reads req.Body fully into a byte slice, enforcing the 10 MB cap.
// It always closes req.Body before returning, regardless of the outcome.
// Returns ErrBodyTooLarge when the body exceeds maxBodyBytes.
func drainBody(req *http.Request) ([]byte, error) {
	if req.Body == nil {
		return nil, nil
	}
	defer req.Body.Close()

	// read up to cap+1 bytes so we can detect the oversize case.
	limited := io.LimitReader(req.Body, maxBodyBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if len(data) > maxBodyBytes {
		return nil, ErrBodyTooLarge
	}
	return data, nil
}

// cloneRequest builds a fresh *http.Request suitable for passing to the
// Forwarder. It inherits the method, URL, and headers from src, but its
// context is poolCtx (the pool's own context, not the inbound request ctx).
// The body is reconstructed from the pre-drained body bytes.
//
// src.Body is set to nil before Clone so that src.Clone does not attempt to
// copy an already-closed body reader — drainBody has already consumed and
// closed it.
func cloneRequest(poolCtx context.Context, src *http.Request, body []byte) *http.Request {
	src.Body = nil
	src.ContentLength = 0
	r := src.Clone(poolCtx)
	if len(body) > 0 {
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
	} else {
		r.Body = http.NoBody
	}
	return r
}

// runWorker calls the Forwarder and CAS-transitions the stored record to
// completed or failed. A CAS race (e.g. the GC pass already transitioned to
// failed_timeout) is logged at debug level and not treated as fatal.
func (p *Pool) runWorker(tag string, req *http.Request) {
	resp, fwdErr := p.forwarder.Forward(p.ctx, req)

	echo := tag
	if len(echo) > maxTagEchoLenLog {
		echo = echo[:maxTagEchoLenLog]
	}

	var (
		targetStatus Status
		targetResp   UpstreamResponse
	)
	if fwdErr == nil {
		targetStatus = StatusCompleted
		targetResp = resp
	} else {
		// log before writing the synthetic 502 so the operator has a breadcrumb
		// tying a failed record to a time and tag. fwdErr.Error() is intentionally
		// not logged — it may contain host:port or upstream data.
		p.logger.Debug("submit_or_fetch: forwarder failed, writing synthetic 502",
			"tag", echo,
		)
		targetStatus = StatusFailed
		targetResp = UpstreamResponse{
			StatusCode: 502,
			Header:     nil,
			Body:       []byte("upstream error"),
		}
	}

	_, casErr := p.store.CompareAndSwapStatus(tag, StatusPending, func(r Record) Record {
		r.Status = targetStatus
		r.UpdatedAt = time.Now().UTC()
		r.UpstreamResponse = &targetResp
		return r
	})
	if casErr != nil {
		errLabel := "unknown"
		switch {
		case errors.Is(casErr, ErrStatusMismatch):
			errLabel = "status_mismatch"
		case errors.Is(casErr, ErrNotFound):
			errLabel = "not_found"
		}
		p.logger.Debug("submit_or_fetch: cas race on completion",
			"tag", echo,
			"prev", string(StatusPending),
			"cas_err", errLabel,
		)
	}
}

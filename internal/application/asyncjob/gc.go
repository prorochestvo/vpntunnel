package asyncjob

import (
	"context"
	"log/slog"
	"time"
)

// GC runs periodic garbage-collection passes over an asyncjob Store. Each pass
// transitions stale records in three ordered steps: pending → failed_timeout,
// terminal → tombstone (stripping the response payload), tombstone → DELETE.
//
// Create via NewGC. The zero value is not usable. Call Run to start the loop.
type GC struct {
	store          Store
	logger         *slog.Logger
	pendingTimeout time.Duration
	completeTTL    time.Duration
	tombstoneTTL   time.Duration
	now            func() time.Time
	tickInterval   time.Duration
}

// GCConfig holds the configuration for a GC instance.
//
// Store is required. Logger defaults to slog.Default() when nil. Now defaults
// to time.Now when nil. TickInterval defaults to 60 seconds when zero.
type GCConfig struct {
	// Store is the persistence layer the GC operates on. Required.
	Store Store
	// Logger receives diagnostic messages from each GC pass. Defaults to
	// slog.Default() when nil.
	Logger *slog.Logger
	// PendingTimeout is the age threshold after which a pending record is
	// transitioned to failed_timeout. Measured from UpdatedAt.
	PendingTimeout time.Duration
	// CompleteTTL is the age threshold after which a completed, failed, or
	// failed_timeout record is transitioned to tombstone. Measured from UpdatedAt.
	CompleteTTL time.Duration
	// TombstoneTTL is the age threshold after which a tombstone record is
	// permanently deleted from the store. Measured from EvictedAt.
	TombstoneTTL time.Duration
	// Now returns the current time. Defaults to time.Now when nil. Injectable
	// for deterministic tests.
	Now func() time.Time
	// TickInterval controls how often the GC executes a pass. Defaults to
	// 60 seconds when zero. Injectable for fast-cycling tests.
	TickInterval time.Duration
}

// NewGC creates a GC from cfg, applying defaults for nil Logger, nil Now, and
// zero TickInterval. cfg.Store must not be nil.
func NewGC(cfg GCConfig) *GC {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	nowFn := cfg.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	tick := cfg.TickInterval
	if tick == 0 {
		tick = 60 * time.Second
	}
	return &GC{
		store:          cfg.Store,
		logger:         logger,
		pendingTimeout: cfg.PendingTimeout,
		completeTTL:    cfg.CompleteTTL,
		tombstoneTTL:   cfg.TombstoneTTL,
		now:            nowFn,
		tickInterval:   tick,
	}
}

// Run starts the GC tick loop. It runs one pass immediately, then repeats on
// each ticker fire until ctx is cancelled. Returns ctx.Err() when the context
// is cancelled — the caller can therefore distinguish a clean shutdown
// (context.Canceled from context.WithCancel) from a deadline exceeded
// (context.DeadlineExceeded from context.WithDeadline/Timeout). Each batch
// within a pass is individually wrapped in a recover so that a corrupt record
// or a panicking Store method in one batch cannot prevent subsequent batches
// from running. The recovered panic is logged at ERROR level; the runtime
// stack is printed to stderr by the Go runtime, not captured here, to avoid
// interpolating an untrusted panic value into a structured log attribute.
func (g *GC) Run(ctx context.Context) error {
	ticker := time.NewTicker(g.tickInterval)
	defer ticker.Stop()

	g.passOnce()

	for {
		select {
		case <-ticker.C:
			g.passOnce()
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// passOnce executes one full GC sweep in three ordered batch operations. The
// order is load-bearing: timeout before tombstone before delete ensures that a
// record does not skip a state (e.g. a freshly timed-out record that also
// satisfies completeTTL stays in failed_timeout until the next pass moves it to
// tombstone, then the pass after that deletes it).
//
// now is snapshotted once at the top of the pass so all three batches use the
// same logical clock — no batch can observe a different "now" than another
// within the same pass.
func (g *GC) passOnce() {
	now := g.now()

	timedOut := g.safeRunBatchTransition(
		"pending_timeout",
		func(r Record) bool {
			return r.Status == StatusPending && now.Sub(r.UpdatedAt) > g.pendingTimeout
		},
		func(r Record) Record {
			r.Status = StatusFailedTimeout
			r.UpdatedAt = now.UTC()
			return r
		},
	)

	tombstoned := g.safeRunBatchTransition(
		"complete_ttl",
		func(r Record) bool {
			return (r.Status == StatusCompleted || r.Status == StatusFailed || r.Status == StatusFailedTimeout) &&
				now.Sub(r.UpdatedAt) > g.completeTTL
		},
		func(r Record) Record {
			r.Status = StatusTombstone
			r.UpstreamResponse = nil // strip payload to keep the on-disk record small
			r.EvictedAt = now.UTC()
			r.UpdatedAt = now.UTC()
			return r
		},
	)

	deleted := g.safeRunBatchDelete(
		"tombstone_ttl",
		func(r Record) bool {
			return r.Status == StatusTombstone && now.Sub(r.EvictedAt) > g.tombstoneTTL
		},
	)

	g.logger.Info("async_gc_pass",
		"timed_out", timedOut,
		"tombstoned", tombstoned,
		"deleted", deleted,
	)
}

// safeRunBatchTransition wraps runBatchTransition in a per-batch recover so
// that a panic in one batch logs ERROR and returns 0 without aborting the
// subsequent batches in the same pass.
func (g *GC) safeRunBatchTransition(op string, filter func(Record) bool, mutate func(Record) Record) (n int) {
	defer func() {
		if r := recover(); r != nil {
			g.logger.Error("async_gc_pass_panic",
				"op", op,
				slog.Any("recovered", true),
			)
			n = 0
		}
	}()
	return g.runBatchTransition(op, filter, mutate)
}

// safeRunBatchDelete wraps runBatchDelete in a per-batch recover so that a
// panic in the delete batch logs ERROR and returns 0 without aborting the
// heartbeat log that follows.
func (g *GC) safeRunBatchDelete(op string, filter func(Record) bool) (n int) {
	defer func() {
		if r := recover(); r != nil {
			g.logger.Error("async_gc_pass_panic",
				"op", op,
				slog.Any("recovered", true),
			)
			n = 0
		}
	}()
	return g.runBatchDelete(op, filter)
}

// runBatchTransition calls store.BatchTransition and logs any error at ERROR
// level. It returns the number of records mutated (0 on error). The op label
// names the GC phase for the error log; err is passed directly via
// slog.Any("err", err) because the only realistic source is bbolt I/O and
// does not contain client-controlled content.
func (g *GC) runBatchTransition(op string, filter func(Record) bool, mutate func(Record) Record) int {
	n, err := g.store.BatchTransition(filter, mutate)
	if err != nil {
		g.logger.Error("async_gc_pass: batch transition failed",
			"op", op,
			slog.Any("err", err),
		)
		return 0
	}
	return n
}

// runBatchDelete calls store.BatchDelete and logs any error at ERROR level.
// It returns the number of records deleted (0 on error). Same rationale for
// slog.Any("err", err) as runBatchTransition.
func (g *GC) runBatchDelete(op string, filter func(Record) bool) int {
	n, err := g.store.BatchDelete(filter)
	if err != nil {
		g.logger.Error("async_gc_pass: batch delete failed",
			"op", op,
			slog.Any("err", err),
		)
		return 0
	}
	return n
}

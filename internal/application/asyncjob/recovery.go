package asyncjob

import (
	"fmt"
	"log/slog"
)

// RunRecovery scans the jobs bucket once, deletes every record with
// StatusPending in a single transaction via Store.BatchDelete, and logs an
// INFO line "restart_pending_dropped count=N". It is intended to run
// synchronously from main.go before the API listener binds so that orphaned
// pending records from a previous process crash do not linger.
//
// When log is nil, slog.Default() is used — the same behaviour as NewPool and
// NewGC.
//
// Returns the wrapped bbolt error from the store on failure; nil on success
// including the empty-bucket case (N=0, the heartbeat log is still emitted).
//
// RunRecovery does not start any goroutines; it returns once the bucket walk
// and delete are complete.
func RunRecovery(store Store, log *slog.Logger) error {
	if log == nil {
		log = slog.Default()
	}

	filter := func(r Record) bool {
		return r.Status == StatusPending
	}

	n, err := store.BatchDelete(filter)
	if err != nil {
		log.Error("restart_pending_dropped", slog.Any("err", err))
		return fmt.Errorf("asyncjob: recovery: %w", err)
	}

	log.Info("restart_pending_dropped", "count", n)
	return nil
}

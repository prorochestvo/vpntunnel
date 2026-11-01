package asyncjob_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vpntunnel/internal/application/asyncjob"
)

// compile-time assertion that *recoveryFakeStore satisfies asyncjob.Store.
var _ asyncjob.Store = (*recoveryFakeStore)(nil)

func TestRunRecovery(t *testing.T) {
	t.Parallel()

	t.Run("empty bucket returns nil and emits count=0", func(t *testing.T) {
		t.Parallel()
		store := openStore(t)
		var buf bytes.Buffer
		log := newCapturingLogger(&buf)

		err := asyncjob.RunRecovery(store, log)
		require.NoError(t, err)

		lines := parseLogLines(t, &buf)
		require.Len(t, lines, 1, "expected exactly one log line")
		assert.Equal(t, "INFO", lines[0]["level"])
		assert.Equal(t, "restart_pending_dropped", lines[0]["msg"])
		// JSON numbers decode to float64.
		assert.Equal(t, float64(0), lines[0]["count"])
	})

	t.Run("all-pending bucket deletes everything and returns nil", func(t *testing.T) {
		t.Parallel()
		store := openStore(t)
		var buf bytes.Buffer
		log := newCapturingLogger(&buf)

		seedSimpleRecord(t, store, "tag-a", asyncjob.StatusPending)
		seedSimpleRecord(t, store, "tag-b", asyncjob.StatusPending)
		seedSimpleRecord(t, store, "tag-c", asyncjob.StatusPending)

		err := asyncjob.RunRecovery(store, log)
		require.NoError(t, err)

		lines := parseLogLines(t, &buf)
		require.Len(t, lines, 1)
		assert.Equal(t, "INFO", lines[0]["level"])
		assert.Equal(t, "restart_pending_dropped", lines[0]["msg"])
		assert.Equal(t, float64(3), lines[0]["count"])

		// verify all three records are gone.
		for _, tag := range []string{"tag-a", "tag-b", "tag-c"} {
			_, found, err2 := store.Get(tag)
			require.NoError(t, err2)
			assert.False(t, found, "tag %q should have been deleted", tag)
		}
	})

	t.Run("mixed-status bucket deletes only pending and leaves the rest intact", func(t *testing.T) {
		t.Parallel()
		store := openStore(t)
		var buf bytes.Buffer
		log := newCapturingLogger(&buf)

		// seed one record per status (five total).
		type seedCase struct {
			tag    string
			status asyncjob.Status
		}
		cases := []seedCase{
			{"tag-pending", asyncjob.StatusPending},
			{"tag-completed", asyncjob.StatusCompleted},
			{"tag-failed", asyncjob.StatusFailed},
			{"tag-failed-timeout", asyncjob.StatusFailedTimeout},
			{"tag-tombstone", asyncjob.StatusTombstone},
		}
		for _, c := range cases {
			seedSimpleRecord(t, store, c.tag, c.status)
		}

		err := asyncjob.RunRecovery(store, log)
		require.NoError(t, err)

		lines := parseLogLines(t, &buf)
		require.Len(t, lines, 1)
		assert.Equal(t, float64(1), lines[0]["count"], "only the pending record should be deleted")

		// pending must be gone.
		_, found, err2 := store.Get("tag-pending")
		require.NoError(t, err2)
		assert.False(t, found, "pending record should have been deleted")

		// all non-pending records must survive, unmodified.
		surviving := []seedCase{
			{"tag-completed", asyncjob.StatusCompleted},
			{"tag-failed", asyncjob.StatusFailed},
			{"tag-failed-timeout", asyncjob.StatusFailedTimeout},
			{"tag-tombstone", asyncjob.StatusTombstone},
		}
		for _, c := range surviving {
			rec, found2, err3 := store.Get(c.tag)
			require.NoError(t, err3)
			require.True(t, found2, "record %q should still exist", c.tag)
			assert.Equal(t, c.status, rec.Status, "status of %q should be unchanged", c.tag)
		}
	})

	t.Run("nil logger defaults to slog.Default()", func(t *testing.T) {
		t.Parallel()
		store := openStore(t)

		seedSimpleRecord(t, store, "tag-x", asyncjob.StatusPending)

		// must not panic; we cannot capture slog.Default() output without
		// replacing it globally (which is not safe in a parallel test), so we
		// just assert the return value is correct.
		err := asyncjob.RunRecovery(store, nil)
		require.NoError(t, err)

		_, found, err2 := store.Get("tag-x")
		require.NoError(t, err2)
		assert.False(t, found, "pending record should have been deleted even with nil logger")
	})

	t.Run("store error propagation", func(t *testing.T) {
		t.Parallel()
		sentinel := errors.New("bbolt: disk full")
		fakeStore := &recoveryFakeStore{batchDeleteErr: sentinel}

		var buf bytes.Buffer
		log := newCapturingLogger(&buf)

		err := asyncjob.RunRecovery(fakeStore, log)
		require.Error(t, err)
		assert.ErrorIs(t, err, sentinel, "returned error must wrap the store error")

		lines := parseLogLines(t, &buf)
		require.Len(t, lines, 1, "expected exactly one error log line")
		assert.Equal(t, "ERROR", lines[0]["level"])
		assert.Equal(t, "restart_pending_dropped", lines[0]["msg"])
		// count attribute is not emitted on the error path.
		_, hasCount := lines[0]["count"]
		assert.False(t, hasCount, "count should not be present in the error log line")
		// err attribute must be present and contain the store error text.
		errVal, hasErr := lines[0]["err"]
		assert.True(t, hasErr, "err attribute must be present in the error log line")
		assert.Contains(t, fmt.Sprint(errVal), sentinel.Error(), "err attribute must contain the store error text")
	})
}

// recoveryFakeStore is a minimal Store stub used only by the store-error
// subtest. Every method except BatchDelete panics to catch unintended calls.
type recoveryFakeStore struct {
	batchDeleteErr error
}

func (s *recoveryFakeStore) Get(_ string) (asyncjob.Record, bool, error) {
	panic("recoveryFakeStore.Get: not expected in recovery tests")
}

func (s *recoveryFakeStore) Put(_ string, _ asyncjob.Record) error {
	panic("recoveryFakeStore.Put: not expected in recovery tests")
}

func (s *recoveryFakeStore) CompareAndSwapStatus(_ string, _ asyncjob.Status, _ func(asyncjob.Record) asyncjob.Record) (asyncjob.Record, error) {
	panic("recoveryFakeStore.CompareAndSwapStatus: not expected in recovery tests")
}

func (s *recoveryFakeStore) BatchTransition(_ func(asyncjob.Record) bool, _ func(asyncjob.Record) asyncjob.Record) (int, error) {
	panic("recoveryFakeStore.BatchTransition: not expected in recovery tests")
}

func (s *recoveryFakeStore) BatchDelete(_ func(asyncjob.Record) bool) (int, error) {
	// filter is intentionally ignored — this fake is used only for error-path
	// tests; the success-path uses a real store.
	if s.batchDeleteErr != nil {
		return 0, s.batchDeleteErr
	}
	return 0, nil
}

func (s *recoveryFakeStore) Counts() (asyncjob.JobCounts, error) {
	panic("recoveryFakeStore.Counts: not expected in recovery tests")
}

func (s *recoveryFakeStore) Close() error { return nil }

// newCapturingLogger returns a *slog.Logger backed by a JSON handler writing
// to buf. The caller can decode log lines from buf to assert structured
// attributes.
func newCapturingLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// parseLogLines decodes every newline-terminated JSON object from buf into a
// slice of map[string]any for attribute inspection.
func parseLogLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var lines []map[string]any
	dec := json.NewDecoder(buf)
	for dec.More() {
		var m map[string]any
		require.NoError(t, dec.Decode(&m))
		lines = append(lines, m)
	}
	return lines
}

// seedSimpleRecord writes a record with the given status into store.
func seedSimpleRecord(t *testing.T, s asyncjob.Store, tag string, status asyncjob.Status) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	rec := asyncjob.Record{
		Tag:       tag,
		Status:    status,
		CreatedAt: now,
		UpdatedAt: now,
	}
	require.NoError(t, s.Put(tag, rec))
}

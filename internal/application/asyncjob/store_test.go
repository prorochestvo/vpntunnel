package asyncjob_test

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"go.etcd.io/bbolt"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vpntunnel/internal/application/asyncjob"
)

// openStore creates a new bbolt-backed store in a unique temp file and
// registers t.Cleanup to close it. Each caller gets its own isolated database.
func openStore(t *testing.T) asyncjob.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "jobs.db")
	s, err := asyncjob.NewStore(path)
	require.NoError(t, err, "NewStore should succeed for a fresh temp file")
	t.Cleanup(func() {
		require.NoError(t, s.Close())
	})
	return s
}

// seedRecord puts a Record directly into the store and returns it.
func seedRecord(t *testing.T, s asyncjob.Store, tag string, status asyncjob.Status) asyncjob.Record {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	rec := asyncjob.Record{
		Tag:       tag,
		Status:    status,
		CreatedAt: now,
		UpdatedAt: now,
	}
	require.NoError(t, s.Put(tag, rec))
	return rec
}

func TestNewStore(t *testing.T) {
	t.Parallel()

	t.Run("opens successfully for a fresh temp file", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "jobs.db")
		s, err := asyncjob.NewStore(path)
		require.NoError(t, err)
		require.NoError(t, s.Close())
	})

	t.Run("returns error when parent directory does not exist", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "nonexistent", "jobs.db")
		_, err := asyncjob.NewStore(path)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "asyncjob:")
	})
}

func TestStore_Get(t *testing.T) {
	t.Parallel()

	t.Run("absent tag returns false with nil error", func(t *testing.T) {
		t.Parallel()
		s := openStore(t)
		_, found, err := s.Get("no-such-tag")
		require.NoError(t, err)
		assert.False(t, found)
	})

	t.Run("put then get round-trips the record", func(t *testing.T) {
		t.Parallel()
		s := openStore(t)
		now := time.Now().UTC().Truncate(time.Second)
		want := asyncjob.Record{
			Tag:       "tag-roundtrip",
			Status:    asyncjob.StatusPending,
			CreatedAt: now,
			UpdatedAt: now,
		}
		require.NoError(t, s.Put(want.Tag, want))
		got, found, err := s.Get(want.Tag)
		require.NoError(t, err)
		require.True(t, found)
		assert.Equal(t, want.Tag, got.Tag)
		assert.Equal(t, want.Status, got.Status)
		assert.Equal(t, want.CreatedAt, got.CreatedAt)
		assert.Equal(t, want.UpdatedAt, got.UpdatedAt)
		assert.Nil(t, got.UpstreamResponse)
	})

	t.Run("corrupt stored bytes return wrapped ErrUnknownStatus with tag", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "jobs.db")

		// seed bad data directly via bbolt before opening through the Store.
		rawDB, err := bbolt.Open(path, 0600, &bbolt.Options{Timeout: 5 * time.Second})
		require.NoError(t, err)
		require.NoError(t, rawDB.Update(func(tx *bbolt.Tx) error {
			b, err := tx.CreateBucketIfNotExists([]byte("jobs"))
			if err != nil {
				return err
			}
			// bad status byte injected directly.
			return b.Put([]byte("corrupt-tag"), []byte(`{"tag":"corrupt-tag","status":"garbage","created_at":1749981600,"updated_at":1749981600}`))
		}))
		require.NoError(t, rawDB.Close())

		s, err := asyncjob.NewStore(path)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, s.Close()) })

		_, _, err = s.Get("corrupt-tag")
		require.Error(t, err)
		assert.True(t, errors.Is(err, asyncjob.ErrUnknownStatus), "expected ErrUnknownStatus, got: %v", err)
		assert.Contains(t, err.Error(), "corrupt-tag", "error must name the offending tag")
	})
}

func TestStore_Put(t *testing.T) {
	t.Parallel()

	t.Run("overwrites an existing record", func(t *testing.T) {
		t.Parallel()
		s := openStore(t)
		now := time.Now().UTC().Truncate(time.Second)
		r := asyncjob.Record{Tag: "tag-overwrite", Status: asyncjob.StatusPending, CreatedAt: now, UpdatedAt: now}
		require.NoError(t, s.Put(r.Tag, r))

		r.Status = asyncjob.StatusCompleted
		r.UpdatedAt = now.Add(time.Second)
		require.NoError(t, s.Put(r.Tag, r))

		got, found, err := s.Get(r.Tag)
		require.NoError(t, err)
		require.True(t, found)
		assert.Equal(t, asyncjob.StatusCompleted, got.Status)
	})
}

func TestStore_CompareAndSwapStatus(t *testing.T) {
	t.Parallel()

	t.Run("succeeds when status matches expected", func(t *testing.T) {
		t.Parallel()
		s := openStore(t)
		now := time.Now().UTC().Truncate(time.Second)
		seedRecord(t, s, "cas-tag", asyncjob.StatusPending)

		transitioned, err := s.CompareAndSwapStatus(
			"cas-tag",
			asyncjob.StatusPending,
			func(r asyncjob.Record) asyncjob.Record {
				r.Status = asyncjob.StatusCompleted
				r.UpdatedAt = now.Add(time.Second)
				return r
			},
		)
		require.NoError(t, err)
		assert.Equal(t, asyncjob.StatusCompleted, transitioned.Status)

		got, found, err := s.Get("cas-tag")
		require.NoError(t, err)
		require.True(t, found)
		assert.Equal(t, asyncjob.StatusCompleted, got.Status)
	})

	t.Run("returns ErrStatusMismatch when status does not match", func(t *testing.T) {
		t.Parallel()
		s := openStore(t)
		seedRecord(t, s, "cas-mismatch", asyncjob.StatusCompleted)

		_, err := s.CompareAndSwapStatus(
			"cas-mismatch",
			asyncjob.StatusPending, // wrong expected
			func(r asyncjob.Record) asyncjob.Record { return r },
		)
		require.Error(t, err)
		assert.True(t, errors.Is(err, asyncjob.ErrStatusMismatch), "expected ErrStatusMismatch, got: %v", err)
	})

	t.Run("returns ErrNotFound when tag is absent", func(t *testing.T) {
		t.Parallel()
		s := openStore(t)

		_, err := s.CompareAndSwapStatus(
			"no-such-tag",
			asyncjob.StatusPending,
			func(r asyncjob.Record) asyncjob.Record { return r },
		)
		require.Error(t, err)
		assert.True(t, errors.Is(err, asyncjob.ErrNotFound), "expected ErrNotFound, got: %v", err)
	})

	t.Run("mutate fn is not called when status mismatches", func(t *testing.T) {
		t.Parallel()
		s := openStore(t)
		seedRecord(t, s, "cas-no-mutate", asyncjob.StatusCompleted)

		mutateCalled := false
		_, err := s.CompareAndSwapStatus(
			"cas-no-mutate",
			asyncjob.StatusPending,
			func(r asyncjob.Record) asyncjob.Record {
				mutateCalled = true
				return r
			},
		)
		require.Error(t, err)
		assert.False(t, mutateCalled, "mutate fn must not be called on status mismatch")
	})

	t.Run("sequential CAS contention: second CAS sees updated status", func(t *testing.T) {
		t.Parallel()
		s := openStore(t)
		now := time.Now().UTC().Truncate(time.Second)
		seedRecord(t, s, "cas-contention", asyncjob.StatusPending)

		// first writer succeeds.
		_, err := s.CompareAndSwapStatus(
			"cas-contention",
			asyncjob.StatusPending,
			func(r asyncjob.Record) asyncjob.Record {
				r.Status = asyncjob.StatusCompleted
				r.UpdatedAt = now.Add(time.Second)
				return r
			},
		)
		require.NoError(t, err)

		// second writer expects pending but record is now completed.
		_, err = s.CompareAndSwapStatus(
			"cas-contention",
			asyncjob.StatusPending,
			func(r asyncjob.Record) asyncjob.Record { return r },
		)
		require.Error(t, err)
		assert.True(t, errors.Is(err, asyncjob.ErrStatusMismatch))
	})
}

func TestStore_BatchTransition(t *testing.T) {
	t.Parallel()

	t.Run("empty bucket returns zero count", func(t *testing.T) {
		t.Parallel()
		s := openStore(t)
		n, err := s.BatchTransition(
			func(r asyncjob.Record) bool { return true },
			func(r asyncjob.Record) asyncjob.Record { return r },
		)
		require.NoError(t, err)
		assert.Equal(t, 0, n)
	})

	t.Run("transitions N matching records and skips non-matching", func(t *testing.T) {
		t.Parallel()
		s := openStore(t)
		now := time.Now().UTC().Truncate(time.Second)

		// 3 pending + 2 completed.
		for i := range 3 {
			seedRecord(t, s, "pending-"+string(rune('A'+i)), asyncjob.StatusPending)
		}
		for i := range 2 {
			seedRecord(t, s, "done-"+string(rune('A'+i)), asyncjob.StatusCompleted)
		}

		n, err := s.BatchTransition(
			func(r asyncjob.Record) bool { return r.Status == asyncjob.StatusPending },
			func(r asyncjob.Record) asyncjob.Record {
				r.Status = asyncjob.StatusTombstone
				r.UpdatedAt = now.Add(time.Minute)
				r.EvictedAt = now.Add(time.Minute)
				return r
			},
		)
		require.NoError(t, err)
		assert.Equal(t, 3, n)

		// verify pending records are now tombstones.
		for i := range 3 {
			tag := "pending-" + string(rune('A'+i))
			got, found, err := s.Get(tag)
			require.NoError(t, err, "Get(%q)", tag)
			require.True(t, found, "tag %q must exist", tag)
			assert.Equal(t, asyncjob.StatusTombstone, got.Status, "tag %q must be tombstone", tag)
		}
		// verify completed records are untouched.
		for i := range 2 {
			tag := "done-" + string(rune('A'+i))
			got, found, err := s.Get(tag)
			require.NoError(t, err, "Get(%q)", tag)
			require.True(t, found)
			assert.Equal(t, asyncjob.StatusCompleted, got.Status)
		}
	})

	t.Run("filter matches nothing returns zero and leaves records intact", func(t *testing.T) {
		t.Parallel()
		s := openStore(t)
		seedRecord(t, s, "untouched-1", asyncjob.StatusPending)
		seedRecord(t, s, "untouched-2", asyncjob.StatusCompleted)
		n, err := s.BatchTransition(
			func(r asyncjob.Record) bool { return false },
			func(r asyncjob.Record) asyncjob.Record {
				t.Error("mutate must not be called when filter always returns false")
				return r
			},
		)
		require.NoError(t, err)
		assert.Equal(t, 0, n)
		got1, found, err := s.Get("untouched-1")
		require.NoError(t, err)
		require.True(t, found)
		assert.Equal(t, asyncjob.StatusPending, got1.Status)
	})
}

func TestStore_BatchDelete(t *testing.T) {
	t.Parallel()

	t.Run("empty bucket returns zero count", func(t *testing.T) {
		t.Parallel()
		s := openStore(t)
		n, err := s.BatchDelete(func(r asyncjob.Record) bool { return true })
		require.NoError(t, err)
		assert.Equal(t, 0, n)
	})

	t.Run("deletes matching records and leaves others intact", func(t *testing.T) {
		t.Parallel()
		s := openStore(t)

		seedRecord(t, s, "to-delete-1", asyncjob.StatusTombstone)
		seedRecord(t, s, "to-delete-2", asyncjob.StatusTombstone)
		seedRecord(t, s, "keep-1", asyncjob.StatusPending)

		n, err := s.BatchDelete(func(r asyncjob.Record) bool {
			return r.Status == asyncjob.StatusTombstone
		})
		require.NoError(t, err)
		assert.Equal(t, 2, n)

		_, found, err := s.Get("to-delete-1")
		require.NoError(t, err)
		assert.False(t, found)

		_, found, err = s.Get("to-delete-2")
		require.NoError(t, err)
		assert.False(t, found)

		_, found, err = s.Get("keep-1")
		require.NoError(t, err)
		assert.True(t, found)
	})
}

func TestStore_Counts(t *testing.T) {
	t.Parallel()

	t.Run("empty bucket returns all zeros", func(t *testing.T) {
		t.Parallel()
		s := openStore(t)
		counts, err := s.Counts()
		require.NoError(t, err)
		assert.Equal(t, 0, counts.Pending)
		assert.Equal(t, 0, counts.Completed)
		assert.Equal(t, 0, counts.Tombstone)
	})

	t.Run("counts mixed-status records correctly", func(t *testing.T) {
		t.Parallel()
		s := openStore(t)

		seedRecord(t, s, "p1", asyncjob.StatusPending)
		seedRecord(t, s, "p2", asyncjob.StatusPending)
		seedRecord(t, s, "c1", asyncjob.StatusCompleted)
		seedRecord(t, s, "ts1", asyncjob.StatusTombstone)
		// failed and failed_timeout are intentionally excluded from counts.
		seedRecord(t, s, "f1", asyncjob.StatusFailed)
		seedRecord(t, s, "ft1", asyncjob.StatusFailedTimeout)

		counts, err := s.Counts()
		require.NoError(t, err)
		assert.Equal(t, 2, counts.Pending, "pending count")
		assert.Equal(t, 1, counts.Completed, "completed count")
		assert.Equal(t, 1, counts.Tombstone, "tombstone count")
	})

	t.Run("corrupt record in bucket returns wrapped error", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "jobs.db")

		rawDB, err := bbolt.Open(path, 0600, &bbolt.Options{Timeout: 5 * time.Second})
		require.NoError(t, err)
		require.NoError(t, rawDB.Update(func(tx *bbolt.Tx) error {
			b, err := tx.CreateBucketIfNotExists([]byte("jobs"))
			if err != nil {
				return err
			}
			return b.Put([]byte("bad-tag"), []byte(`{"tag":"bad-tag","status":"oops","created_at":1,"updated_at":1}`))
		}))
		require.NoError(t, rawDB.Close())

		s, err := asyncjob.NewStore(path)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, s.Close()) })

		_, err = s.Counts()
		require.Error(t, err)
		assert.True(t, errors.Is(err, asyncjob.ErrUnknownStatus))
		// tag bytes must not appear in the error string — the tag is
		// client-controlled and must not leak into operational logs.
		assert.NotContains(t, err.Error(), "bad-tag")
		assert.Contains(t, err.Error(), "counts scan")
	})
}

func TestStore_Close(t *testing.T) {
	t.Parallel()

	t.Run("close is idempotent — second call returns nil", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "jobs.db")
		s, err := asyncjob.NewStore(path)
		require.NoError(t, err)

		require.NoError(t, s.Close())
		// second Close must not panic or return a non-nil error.
		require.NoError(t, s.Close())
	})
}

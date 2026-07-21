package asyncjob

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"go.etcd.io/bbolt"
)

// ErrNotFound is returned by CompareAndSwapStatus when the tag does not exist
// in the store.
var ErrNotFound = errors.New("asyncjob: tag not found")

// ErrStatusMismatch is returned by CompareAndSwapStatus when the on-disk
// status does not equal the expected value. Callers can use errors.Is to
// distinguish a lost race from a missing record.
var ErrStatusMismatch = errors.New("asyncjob: status mismatch")

// JobCounts holds the count of records in each of the three counted states.
// Records in StatusFailed or StatusFailedTimeout are not included.
type JobCounts struct {
	Pending   int
	Completed int
	Tombstone int
}

// Store defines the persistence operations for async job records keyed by
// retry-tag. All methods are safe for concurrent use.
//
// Get returns the record for tag. The bool is false (and error is nil) when
// the tag is absent.
//
// Put writes rec under tag, overwriting any existing record.
//
// CompareAndSwapStatus atomically replaces the record for tag only when its
// current on-disk status equals expected. It returns ErrStatusMismatch when
// the status does not match and ErrNotFound when the tag is absent. When CAS
// succeeds it returns the record produced by mutate.
//
// BatchTransition calls mutate on every record for which filter returns true,
// writing the result back in one transaction. It returns the number of records
// mutated.
//
// BatchDelete removes every record for which filter returns true in one
// transaction. It returns the number of records deleted.
//
// Counts returns the count of records in StatusPending, StatusCompleted, and
// StatusTombstone states. Records in StatusFailed or StatusFailedTimeout are
// not counted.
//
// Close releases the file lock and closes the database. It is safe to call
// Close more than once; subsequent calls return the same error as the first
// call and are otherwise no-ops.
type Store interface {
	Get(tag string) (Record, bool, error)
	Put(tag string, rec Record) error
	CompareAndSwapStatus(tag string, expected Status, mutate func(Record) Record) (Record, error)
	BatchTransition(filter func(Record) bool, mutate func(Record) Record) (int, error)
	BatchDelete(filter func(Record) bool) (int, error)
	Counts() (JobCounts, error)
	Close() error
}

// NewStore opens or creates the bbolt database at path and returns a Store
// backed by it. The parent directory must already exist; NewStore surfaces a
// wrapped error if it does not. The file is opened with mode 0600. An
// open-timeout of 5 s is applied so that a lock held by another process fails
// fast rather than blocking indefinitely.
//
// The caller is responsible for calling Close when the store is no longer
// needed.
func NewStore(path string) (Store, error) {
	opts := &bbolt.Options{
		Timeout: 5 * time.Second,
	}
	db, err := bbolt.Open(path, 0600, opts)
	if err != nil {
		return nil, fmt.Errorf("asyncjob: open store at %q: %w", path, err)
	}
	if err := db.Update(func(tx *bbolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(bucketName)
		return err
	}); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("asyncjob: create bucket: %w", err)
	}
	return &bboltStore{db: db}, nil
}

// bboltStore is the concrete bbolt-backed implementation of Store.
type bboltStore struct {
	db        *bbolt.DB
	closeOnce sync.Once
	closeErr  error
}

// Close closes the underlying bbolt database, releasing the file lock.
// It is safe to call Close more than once; subsequent calls return the same
// error as the first call and are otherwise no-ops.
func (s *bboltStore) Close() error {
	s.closeOnce.Do(func() {
		s.closeErr = s.db.Close()
	})
	return s.closeErr
}

// Counts returns the number of records currently in StatusPending,
// StatusCompleted, and StatusTombstone states. Records in StatusFailed or
// StatusFailedTimeout are not included. It uses unmarshalStatus to read only
// the status field, avoiding allocation of the full UpstreamResponse for
// every record on the scan.
func (s *bboltStore) Counts() (JobCounts, error) {
	var counts JobCounts
	err := s.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketName).ForEach(func(k, v []byte) error {
			st, err := unmarshalStatus(v)
			if err != nil {
				return fmt.Errorf("asyncjob: counts scan: %w", err)
			}
			switch st {
			case StatusPending:
				counts.Pending++
			case StatusCompleted:
				counts.Completed++
			case StatusTombstone:
				counts.Tombstone++
			}
			return nil
		})
	})
	return counts, err
}

// Get returns the record stored under tag. The bool is false and err is nil
// when the tag is absent. Returns a wrapped error containing the tag when the
// stored bytes cannot be deserialised.
func (s *bboltStore) Get(tag string) (Record, bool, error) {
	var rec Record
	var found bool
	err := s.db.View(func(tx *bbolt.Tx) error {
		v := tx.Bucket(bucketName).Get([]byte(tag))
		if v == nil {
			return nil
		}
		found = true
		// copy v out of the transaction before returning.
		cp := make([]byte, len(v))
		copy(cp, v)
		var err error
		rec, err = Unmarshal(cp)
		if err != nil {
			return fmt.Errorf("asyncjob: tag %q: %w", tag, err)
		}
		return nil
	})
	if err != nil {
		return Record{}, false, err
	}
	return rec, found, nil
}

// Put serialises rec and writes it under tag. It overwrites any existing
// record for the same tag.
func (s *bboltStore) Put(tag string, rec Record) error {
	data, err := rec.Marshal()
	if err != nil {
		return fmt.Errorf("asyncjob: put tag %q: %w", tag, err)
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketName).Put([]byte(tag), data)
	})
}

// CompareAndSwapStatus atomically transitions the record for tag from expected
// to the value returned by mutate. It reads the record inside the Update
// transaction to avoid a race window with concurrent writers. Returns
// ErrNotFound when the tag is absent, ErrStatusMismatch when the current
// status differs from expected.
func (s *bboltStore) CompareAndSwapStatus(tag string, expected Status, mutate func(Record) Record) (Record, error) {
	var updated Record
	err := s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketName)
		v := b.Get([]byte(tag))
		if v == nil {
			return ErrNotFound
		}
		// copy before the transaction closes.
		cp := make([]byte, len(v))
		copy(cp, v)
		rec, err := Unmarshal(cp)
		if err != nil {
			return fmt.Errorf("asyncjob: tag %q: %w", tag, err)
		}
		if rec.Status != expected {
			return fmt.Errorf("%w: have %q, expected %q", ErrStatusMismatch, rec.Status, expected)
		}
		updated = mutate(rec)
		data, err := updated.Marshal()
		if err != nil {
			return fmt.Errorf("asyncjob: put tag %q after CAS: %w", tag, err)
		}
		return b.Put([]byte(tag), data)
	})
	if err != nil {
		return Record{}, err
	}
	return updated, nil
}

// BatchTransition applies mutate to every record for which filter returns true.
// All mutations execute inside a single bbolt.Update transaction. It first
// collects the keys of matching records (cursor scan), then applies mutations
// in a second pass over those keys to avoid cursor invalidation after writes.
// Returns the number of records mutated.
func (s *bboltStore) BatchTransition(filter func(Record) bool, mutate func(Record) Record) (int, error) {
	var count int
	err := s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketName)

		// pass 1: collect keys of matching records.
		matched := make([][]byte, 0, b.Stats().KeyN)
		if err := b.ForEach(func(k, v []byte) error {
			rec, err := Unmarshal(v)
			if err != nil {
				return fmt.Errorf("asyncjob: tag %q: %w", tagForError(k), err)
			}
			if filter(rec) {
				kc := make([]byte, len(k))
				copy(kc, k)
				matched = append(matched, kc)
			}
			return nil
		}); err != nil {
			return err
		}

		// pass 2: re-read, mutate, and write back.
		for _, k := range matched {
			v := b.Get(k)
			if v == nil {
				// deleted between scans — skip silently.
				continue
			}
			cp := make([]byte, len(v))
			copy(cp, v)
			rec, err := Unmarshal(cp)
			if err != nil {
				return fmt.Errorf("asyncjob: tag %q: %w", tagForError(k), err)
			}
			out := mutate(rec)
			data, err := out.Marshal()
			if err != nil {
				return fmt.Errorf("asyncjob: put tag %q in batch: %w", tagForError(k), err)
			}
			if err := b.Put(k, data); err != nil {
				return err
			}
			count++
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return count, nil
}

// BatchDelete removes every record for which filter returns true. All deletions
// execute inside a single bbolt.Update transaction. It first collects matching
// keys and then deletes them to avoid cursor invalidation after mutations.
// Returns the number of records deleted.
func (s *bboltStore) BatchDelete(filter func(Record) bool) (int, error) {
	var count int
	err := s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketName)

		// pass 1: collect keys to delete.
		toDelete := make([][]byte, 0, b.Stats().KeyN)
		if err := b.ForEach(func(k, v []byte) error {
			rec, err := Unmarshal(v)
			if err != nil {
				return fmt.Errorf("asyncjob: tag %q: %w", tagForError(k), err)
			}
			if filter(rec) {
				kc := make([]byte, len(k))
				copy(kc, k)
				toDelete = append(toDelete, kc)
			}
			return nil
		}); err != nil {
			return err
		}

		// pass 2: delete.
		for _, k := range toDelete {
			if err := b.Delete(k); err != nil {
				return err
			}
			count++
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return count, nil
}

// maxTagEchoLen caps the number of bytes echoed from a client-supplied tag in
// error strings to prevent a large tag from flooding logs.
const maxTagEchoLen = 128

// bucketName is the bbolt bucket used to store all job records.
var bucketName = []byte("jobs")

// tagForError returns a string safe to embed in an error message. If k exceeds
// maxTagEchoLen bytes the excess is replaced with an ellipsis.
func tagForError(k []byte) string {
	if len(k) > maxTagEchoLen {
		return string(k[:maxTagEchoLen]) + "…"
	}
	return string(k)
}

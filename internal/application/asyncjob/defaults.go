package asyncjob

import "time"

// DefaultMaxConcurrentJobs is the semaphore size passed as NewPool's maxConcurrent
// argument when no explicit value is configured. It limits the number of in-flight
// async jobs at any one time.
const DefaultMaxConcurrentJobs = 100

// DefaultPendingTimeout is the age past which a pending record is transitioned to
// failed_timeout by the GC. Measured from UpdatedAt.
const DefaultPendingTimeout = 5 * time.Minute

// DefaultCompleteTTL is the age past which a completed, failed, or failed_timeout
// record is transitioned to tombstone by the GC. Measured from UpdatedAt.
const DefaultCompleteTTL = 1 * time.Hour

// DefaultTombstoneTTL is the age past which a tombstone record is permanently
// deleted from the store by the GC. Measured from EvictedAt.
const DefaultTombstoneTTL = 24 * time.Hour

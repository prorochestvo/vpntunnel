package asyncjob_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"vpntunnel/internal/asyncjob"
)

func TestDefaults(t *testing.T) {
	t.Parallel()

	t.Run("DefaultMaxConcurrentJobs is 100", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, 100, asyncjob.DefaultMaxConcurrentJobs)
	})

	t.Run("DefaultPendingTimeout is 5m", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, 5*time.Minute, asyncjob.DefaultPendingTimeout)
	})

	t.Run("DefaultCompleteTTL is 1h", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, 1*time.Hour, asyncjob.DefaultCompleteTTL)
	})

	t.Run("DefaultTombstoneTTL is 24h", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, 24*time.Hour, asyncjob.DefaultTombstoneTTL)
	})
}

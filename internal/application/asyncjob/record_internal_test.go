package asyncjob

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUnmarshalStatus(t *testing.T) {
	t.Parallel()

	t.Run("returns correct status for all known values", func(t *testing.T) {
		t.Parallel()
		cases := []Status{
			StatusPending,
			StatusCompleted,
			StatusFailed,
			StatusFailedTimeout,
			StatusTombstone,
		}
		for _, want := range cases {
			want := want
			t.Run(string(want), func(t *testing.T) {
				t.Parallel()
				now := time.Now().UTC().Truncate(time.Second)
				rec := Record{
					Tag:       "tag",
					Status:    want,
					CreatedAt: now,
					UpdatedAt: now,
				}
				data, err := rec.Marshal()
				require.NoError(t, err)
				got, err := unmarshalStatus(data)
				require.NoError(t, err)
				assert.Equal(t, want, got)
			})
		}
	})

	t.Run("returns ErrUnknownStatus for unknown status", func(t *testing.T) {
		t.Parallel()
		data := []byte(`{"tag":"x","status":"bogus","created_at":1,"updated_at":1}`)
		_, err := unmarshalStatus(data)
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrUnknownStatus))
	})

	t.Run("returns error for malformed JSON", func(t *testing.T) {
		t.Parallel()
		_, err := unmarshalStatus([]byte(`{not json`))
		require.Error(t, err)
		assert.False(t, errors.Is(err, ErrUnknownStatus))
	})
}

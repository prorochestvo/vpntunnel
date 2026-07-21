package rotation

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// compile-time assertion: NoopRotator must satisfy Rotator.
var _ Rotator = NoopRotator{}

func TestNoopRotator_Rotate(t *testing.T) {
	t.Parallel()

	res, err := NoopRotator{}.Rotate(t.Context(), false)
	require.NoError(t, err)
	assert.Equal(t, RotationUnavailable, res.Outcome)
	assert.Empty(t, res.Country)
	assert.Zero(t, res.ActiveSessions)
}

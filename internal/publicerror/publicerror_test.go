package publicerror_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"httpproxy/internal/publicerror"
)

func TestError(t *testing.T) {
	t.Parallel()

	t.Run("error returns details", func(t *testing.T) {
		t.Parallel()
		e := publicerror.New("something went wrong for the user")
		assert.Equal(t, "something went wrong for the user", e.Error())
		assert.Equal(t, "something went wrong for the user", e.Details())
	})

	t.Run("errors.As unwraps wrapped public error", func(t *testing.T) {
		t.Parallel()
		orig := publicerror.New("x")
		wrapped := fmt.Errorf("wrap: %w", orig)

		var target *publicerror.Error
		require.True(t, errors.As(wrapped, &target))
		assert.Equal(t, "x", target.Details())
	})

	t.Run("Is returns false for nil and plain error", func(t *testing.T) {
		t.Parallel()

		pe, ok := publicerror.Is(nil)
		assert.False(t, ok)
		assert.Nil(t, pe)

		pe, ok = publicerror.Is(errors.New("plain"))
		assert.False(t, ok)
		assert.Nil(t, pe)
	})
}

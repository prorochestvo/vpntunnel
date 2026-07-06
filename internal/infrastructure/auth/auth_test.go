package auth_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vpntunnel/internal/infrastructure/auth"
)

func TestBearerVerifier_Verify(t *testing.T) {
	t.Parallel()

	const token = "super-secret-xk3m9v"
	var v auth.Verifier = auth.NewBearerVerifier(token)

	t.Run("valid token returns true", func(t *testing.T) {
		t.Parallel()
		assert.True(t, v.Verify("Bearer "+token))
	})

	t.Run("missing header (empty string) returns false", func(t *testing.T) {
		t.Parallel()
		assert.False(t, v.Verify(""))
	})

	t.Run("Basic scheme returns false", func(t *testing.T) {
		t.Parallel()
		assert.False(t, v.Verify("Basic dXNlcjpwYXNz"))
	})

	t.Run("lowercase bearer scheme returns true", func(t *testing.T) {
		t.Parallel()
		assert.True(t, v.Verify("bearer "+token))
	})

	t.Run("uppercase BEARER scheme returns true", func(t *testing.T) {
		t.Parallel()
		assert.True(t, v.Verify("BEARER "+token))
	})

	t.Run("multiple spaces between scheme and token returns true", func(t *testing.T) {
		t.Parallel()
		// strings.Fields collapses all runs of whitespace
		assert.True(t, v.Verify("Bearer  "+token))
	})

	t.Run("no space between scheme and token returns false", func(t *testing.T) {
		t.Parallel()
		// "Bearer<token>" is a single field; len(parts) == 1 → false
		assert.False(t, v.Verify("Bearer"+token))
	})

	t.Run("Bearer with no token returns false", func(t *testing.T) {
		t.Parallel()
		assert.False(t, v.Verify("Bearer "))
	})

	t.Run("wrong token of same length returns false", func(t *testing.T) {
		t.Parallel()
		// same length as token, different bytes
		wrong := token[:len(token)-1] + "X"
		assert.False(t, v.Verify("Bearer "+wrong))
	})

	t.Run("wrong token of different length returns false", func(t *testing.T) {
		t.Parallel()
		assert.False(t, v.Verify("Bearer short"))
	})

	t.Run("whitespace-only header returns false", func(t *testing.T) {
		t.Parallel()
		assert.False(t, v.Verify("   "))
	})

	t.Run("extra token after valid token returns false", func(t *testing.T) {
		t.Parallel()
		// three fields: Bearer, token, extra — len(parts) == 3 → false
		assert.False(t, v.Verify("Bearer "+token+" extra"))
	})
}

func TestNewBearerVerifier(t *testing.T) {
	t.Parallel()

	t.Run("panics on empty token", func(t *testing.T) {
		t.Parallel()
		require.Panics(t, func() {
			auth.NewBearerVerifier("")
		})
	})

	t.Run("returns non-nil verifier for non-empty token", func(t *testing.T) {
		t.Parallel()
		v := auth.NewBearerVerifier("tok")
		require.NotNil(t, v)
	})
}

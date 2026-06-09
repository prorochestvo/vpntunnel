package main

import (
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunHealthcheck(t *testing.T) {
	t.Parallel()

	t.Run("returns 0 when port is listening", func(t *testing.T) {
		t.Parallel()

		ln, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		t.Cleanup(func() { _ = ln.Close() })

		code := runHealthcheck(ln.Addr().String())
		assert.Equal(t, 0, code)
	})

	t.Run("returns 1 when nothing is listening", func(t *testing.T) {
		t.Parallel()

		// port 1 is a privileged port that is never bound by userspace
		// listeners; it provides a reliable connection-refused result.
		code := runHealthcheck("127.0.0.1:1")
		assert.Equal(t, 1, code)
	})

	t.Run("returns 1 when addr is malformed", func(t *testing.T) {
		t.Parallel()

		code := runHealthcheck("not:a:valid:addr")
		assert.Equal(t, 1, code)
	})
}

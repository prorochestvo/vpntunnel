package handlers

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
)

var _ error = dialTimeoutErr{}

// dialTimeoutErr is a minimal timeout error for constructing *net.OpError test
// fixtures without involving actual network I/O.
type dialTimeoutErr struct{}

func (dialTimeoutErr) Error() string   { return "i/o timeout" }
func (dialTimeoutErr) Timeout() bool   { return true }
func (dialTimeoutErr) Temporary() bool { return true }

func TestClassifyDoError(t *testing.T) {
	t.Parallel()

	t.Run("dial timeout returns upstream dial timeout", func(t *testing.T) {
		t.Parallel()
		err := &url.Error{
			Op:  "Get",
			URL: "https://example.com/path",
			Err: &net.OpError{
				Op:  "dial",
				Net: "tcp",
				Err: dialTimeoutErr{},
			},
		}
		opMsg := classifyDoError(err)
		assert.Equal(t, "upstream dial timeout", opMsg)
	})

	t.Run("dial connection refused returns upstream connection refused", func(t *testing.T) {
		t.Parallel()
		err := &url.Error{
			Op:  "Get",
			URL: "https://example.com/path",
			Err: &net.OpError{
				Op:  "dial",
				Net: "tcp",
				Err: errors.New("connection refused"),
			},
		}
		opMsg := classifyDoError(err)
		assert.Equal(t, "upstream connection refused", opMsg)
	})

	t.Run("read timeout returns upstream read timeout", func(t *testing.T) {
		t.Parallel()
		err := &url.Error{
			Op:  "Get",
			URL: "https://example.com/path",
			Err: &net.OpError{
				Op:  "read",
				Net: "tcp",
				Err: dialTimeoutErr{},
			},
		}
		opMsg := classifyDoError(err)
		assert.Equal(t, "upstream read timeout", opMsg)
	})

	t.Run("read non-timeout returns upstream read failed", func(t *testing.T) {
		t.Parallel()
		err := &url.Error{
			Op:  "Get",
			URL: "https://example.com/path",
			Err: &net.OpError{
				Op:  "read",
				Net: "tcp",
				Err: errors.New("connection reset by peer"),
			},
		}
		opMsg := classifyDoError(err)
		assert.Equal(t, "upstream read failed", opMsg)
	})

	t.Run("write returns upstream write failed", func(t *testing.T) {
		t.Parallel()
		err := &url.Error{
			Op:  "Get",
			URL: "https://example.com/path",
			Err: &net.OpError{
				Op:  "write",
				Net: "tcp",
				Err: errors.New("broken pipe"),
			},
		}
		opMsg := classifyDoError(err)
		assert.Equal(t, "upstream write failed", opMsg)
	})

	t.Run("tls handshake failure returns upstream tls handshake failed", func(t *testing.T) {
		t.Parallel()
		err := &url.Error{
			Op:  "Get",
			URL: "https://example.com/path",
			Err: tls.RecordHeaderError{
				Msg:          "first record does not look like a TLS handshake",
				RecordHeader: [5]byte{0x48, 0x54, 0x54, 0x50, 0x2f}, // "HTTP/"
			},
		}
		opMsg := classifyDoError(err)
		assert.Equal(t, "upstream tls handshake failed", opMsg)
	})

	t.Run("generic url.Error without OpError or TLS returns upstream error", func(t *testing.T) {
		t.Parallel()
		err := &url.Error{
			Op:  "Get",
			URL: "https://example.com/path",
			Err: errors.New("some unknown transport error"),
		}
		opMsg := classifyDoError(err)
		assert.Equal(t, "upstream error", opMsg)
	})

	t.Run("unknown error shape returns upstream error", func(t *testing.T) {
		t.Parallel()
		err := errors.New("something completely unknown from the stack")
		opMsg := classifyDoError(err)
		assert.Equal(t, "upstream error", opMsg)
	})

	t.Run("wrapped url.Error is found via errors.As", func(t *testing.T) {
		t.Parallel()
		// verify errors.As is used (not type assertion) by wrapping the url.Error.
		inner := &url.Error{
			Op:  "Get",
			URL: "https://example.com/path",
			Err: &net.OpError{
				Op:  "dial",
				Net: "tcp",
				Err: dialTimeoutErr{},
			},
		}
		wrapped := fmt.Errorf("outer: %w", inner)
		opMsg := classifyDoError(wrapped)
		assert.Equal(t, "upstream dial timeout", opMsg)
	})
}

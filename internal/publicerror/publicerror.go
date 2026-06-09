// Package publicerror provides Error, the contract for errors whose message is
// safe to surface directly to the proxy's HTTP client.
//
// Service-layer code creates public errors via New when it wants the exact
// message forwarded to the caller. The HTTP transport layer inspects each
// error with errors.As and sends Details() to the client on match; plain
// errors get a generic fallback message.
package publicerror

import "errors"

// New returns a new Error with the given human-readable details.
func New(details string) *Error {
	return &Error{details: details}
}

// Is reports whether err is (or wraps) a *Error, and returns it. The second
// return is false for nil and for errors that are not *Error.
func Is(err error) (*Error, bool) {
	if err == nil {
		return nil, false
	}
	var pe *Error
	if errors.As(err, &pe) {
		return pe, true
	}
	return nil, false
}

// Error carries a user-safe error message.
type Error struct {
	details string
}

// Error implements the error interface; returns the details string verbatim.
func (e *Error) Error() string { return e.details }

// Details returns the message that is safe to send to the HTTP client.
func (e *Error) Details() string { return e.details }

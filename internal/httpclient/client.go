// Package httpclient builds the clients the CLI uses to send writes.
package httpclient

import (
	"errors"
	"fmt"
	"net/http"
	"time"
)

// RefusedRedirectError is a redirect left unfollowed because it would have
// resent a write as a GET.
type RefusedRedirectError struct {
	StatusCode int
}

func (e *RefusedRedirectError) Error() string {
	// A 303 usually answers a completed write, so this can't claim either way.
	return fmt.Sprintf("HTTP %d redirect not followed because it would resend the write as a GET with no body; the write may or may not have been applied", e.StatusCode)
}

// New returns a client that fails with RefusedRedirectError on any redirect
// that would change the request's method. A zero timeout means no timeout.
func New(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, CheckRedirect: keepMethod}
}

func keepMethod(req *http.Request, via []*http.Request) error {
	// Setting CheckRedirect replaces net/http's default limit.
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	if req.Method != via[len(via)-1].Method {
		return &RefusedRedirectError{StatusCode: req.Response.StatusCode}
	}
	return nil
}

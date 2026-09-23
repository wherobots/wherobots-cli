package executor

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// maxErrorBodyBytes bounds how much of an error response is kept for the
// message; storage error bodies are small XML documents.
const maxErrorBodyBytes = 64 * 1024

// StorageError is a failed transfer against presigned storage. It never holds
// the response body, which can reveal internal storage locations.
type StorageError struct {
	Op         string // "download" or "upload"
	StatusCode int
	Code       string // storage error code such as NoSuchKey; may be empty
}

func (e *StorageError) Error() string {
	msg := e.Op + " failed with HTTP " + strconv.Itoa(e.StatusCode)
	if e.Code != "" {
		msg += " (" + e.Code + ")"
	}
	return msg
}

// storageCodePattern accepts only a short plain word, so nothing else from the body leaks.
var storageCodePattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// newStorageError reads at most maxErrorBodyBytes of resp and keeps only the XML <Code>.
func newStorageError(op string, resp *http.Response) *StorageError {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
	var parsed struct {
		Code string `xml:"Code"`
	}
	code := ""
	if xml.Unmarshal(body, &parsed) == nil {
		if c := strings.TrimSpace(parsed.Code); storageCodePattern.MatchString(c) {
			code = c
		}
	}
	return &StorageError{Op: op, StatusCode: resp.StatusCode, Code: code}
}

// ResolveRedirectWithReauth sends an authenticated API request without
// following redirects and returns the absolute Location of the 3xx answer.
// A 401 gets the same single refresh-and-replay as DoWithReauth.
func ResolveRedirectWithReauth(client *http.Client, req *http.Request, creds Credentials) (string, error) {
	noFollow := noRedirectClient(client)
	location, err := redirectLocation(noFollow, req)
	var httpErr *HTTPError
	if err == nil || !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusUnauthorized {
		return location, err
	}

	retry, ok, retryErr := refreshedReplay(req, creds)
	if retryErr != nil {
		return "", retryErr
	}
	if !ok {
		return "", err
	}
	return redirectLocation(noFollow, retry)
}

// noRedirectClient copies client (keeping its transport and timeout) so a
// redirect is returned to the caller instead of followed with our headers.
func noRedirectClient(client *http.Client) *http.Client {
	if client == nil {
		client = http.DefaultClient
	}
	copied := *client
	copied.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &copied
}

func redirectLocation(client *http.Client, req *http.Request) (string, error) {
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("execute request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
	if err != nil {
		return "", fmt.Errorf("read response body: %w", err)
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		location := strings.TrimSpace(resp.Header.Get("Location"))
		if location == "" {
			return "", fmt.Errorf("HTTP %d redirect is missing a Location header", resp.StatusCode)
		}
		target, err := req.URL.Parse(location)
		if err != nil {
			return "", fmt.Errorf("invalid redirect Location: %w", err)
		}
		return target.String(), nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", &HTTPError{StatusCode: resp.StatusCode, Body: body}
	}
	return "", fmt.Errorf("expected a redirect to the file's storage location, got HTTP %d", resp.StatusCode)
}

// StreamFromURL fetches rawURL with a fresh GET that carries no auth or
// client headers (the URL is presigned) and copies the body to w verbatim.
func StreamFromURL(ctx context.Context, client *http.Client, rawURL string, w io.Writer) (int64, error) {
	if client == nil {
		return 0, fmt.Errorf("http client is required")
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || !parsed.IsAbs() {
		return 0, fmt.Errorf("invalid download URL")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return 0, fmt.Errorf("build download request: %w", err)
	}
	// An explicit Accept-Encoding stops Go's transport from gunzipping a
	// Content-Encoding: gzip object, so the stored bytes land unchanged.
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("download request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, newStorageError("download", resp)
	}

	written, err := io.Copy(w, resp.Body)
	if err != nil {
		return written, fmt.Errorf("download interrupted after %d bytes: %w", written, err)
	}
	return written, nil
}

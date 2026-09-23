package executor

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// storageRecorder is a mock presigned-storage server that records the
// headers of every request it receives.
type storageRecorder struct {
	mu      sync.Mutex
	headers []http.Header
	body    string
}

func (s *storageRecorder) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.headers = append(s.headers, r.Header.Clone())
		s.mu.Unlock()
		_, _ = w.Write([]byte(s.body))
	}
}

func assertNoCredentialHeaders(t *testing.T, headers []http.Header) {
	t.Helper()
	if len(headers) != 1 {
		t.Fatalf("storage requests = %d, want 1", len(headers))
	}
	for _, name := range []string{"x-api-key", "Authorization", clientHeaderName} {
		if v := headers[0].Get(name); v != "" {
			t.Fatalf("storage request carried %s = %q; it must carry no Wherobots headers", name, v)
		}
	}
}

func TestDownloadFollowsRedirectWithoutCredentials(t *testing.T) {
	t.Parallel()

	storage := &storageRecorder{body: "col1,col2\n1,2\n"}
	storageServer := httptest.NewServer(storage.handler())
	defer storageServer.Close()

	var apiKeySeen string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiKeySeen = r.Header.Get("x-api-key")
		http.Redirect(w, r, storageServer.URL+"/bucket/key?X-Amz-Signature=abc", http.StatusTemporaryRedirect)
	}))
	defer api.Close()

	creds := apiKeyCreds("secret-key")
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, api.URL+"/storage/s/files/a.csv", nil)
	_ = creds.Apply(req.Context(), req)
	req.Header.Set(clientHeaderName, "client=cli;ver=dev")

	location, err := ResolveRedirectWithReauth(api.Client(), req, creds)
	if err != nil {
		t.Fatalf("ResolveRedirectWithReauth() error = %v", err)
	}
	if apiKeySeen != "secret-key" {
		t.Fatalf("API request x-api-key = %q, want the key", apiKeySeen)
	}
	if !strings.HasPrefix(location, storageServer.URL+"/bucket/key") {
		t.Fatalf("location = %q", location)
	}

	var out bytes.Buffer
	n, err := StreamFromURL(context.Background(), &http.Client{}, location, &out)
	if err != nil {
		t.Fatalf("StreamFromURL() error = %v", err)
	}
	if out.String() != storage.body || n != int64(len(storage.body)) {
		t.Fatalf("body = %q (n=%d), want %q", out.String(), n, storage.body)
	}
	assertNoCredentialHeaders(t, storage.headers)
}

func TestDownloadBearerTokenNeverReachesStorage(t *testing.T) {
	t.Parallel()

	storage := &storageRecorder{body: "x"}
	storageServer := httptest.NewServer(storage.handler())
	defer storageServer.Close()

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, storageServer.URL+"/obj", http.StatusTemporaryRedirect)
	}))
	defer api.Close()

	creds := &fakeCreds{header: "Authorization", value: "Bearer tok"}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, api.URL+"/f", nil)
	_ = creds.Apply(req.Context(), req)

	location, err := ResolveRedirectWithReauth(api.Client(), req, creds)
	if err != nil {
		t.Fatalf("ResolveRedirectWithReauth() error = %v", err)
	}
	if _, err := StreamFromURL(context.Background(), &http.Client{}, location, &bytes.Buffer{}); err != nil {
		t.Fatalf("StreamFromURL() error = %v", err)
	}
	assertNoCredentialHeaders(t, storage.headers)
}

func TestResolveRedirectRefreshesOnceAfter401(t *testing.T) {
	t.Parallel()

	var calls int
	var authHeaders []string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		authHeaders = append(authHeaders, r.Header.Get("Authorization"))
		if calls == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Location", "https://storage.example.com/obj")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer api.Close()

	creds := &fakeCreds{header: "Authorization", value: "Bearer stale", refreshOK: true}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, api.URL+"/f", nil)
	_ = creds.Apply(req.Context(), req)
	creds.value = "Bearer fresh"

	location, err := ResolveRedirectWithReauth(api.Client(), req, creds)
	if err != nil {
		t.Fatalf("ResolveRedirectWithReauth() error = %v", err)
	}
	if location != "https://storage.example.com/obj" {
		t.Fatalf("location = %q", location)
	}
	if calls != 2 || creds.refreshCalls != 1 {
		t.Fatalf("calls = %d, refreshCalls = %d; want 2 and 1", calls, creds.refreshCalls)
	}
	if authHeaders[1] != "Bearer fresh" {
		t.Fatalf("replay Authorization = %q, want fresh bearer", authHeaders[1])
	}
}

func TestResolveRedirectReturnsHTTPErrorOn404(t *testing.T) {
	t.Parallel()

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer api.Close()

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, api.URL+"/f", nil)
	_, err := ResolveRedirectWithReauth(api.Client(), req, apiKeyCreds("k"))
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusNotFound {
		t.Fatalf("err = %v, want HTTP 404", err)
	}
}

func TestResolveRedirectRejectsNonRedirectSuccess(t *testing.T) {
	t.Parallel()

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("{}"))
	}))
	defer api.Close()

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, api.URL+"/f", nil)
	if _, err := ResolveRedirectWithReauth(api.Client(), req, apiKeyCreds("k")); err == nil {
		t.Fatalf("expected an error for a 200 without redirect")
	}
}

func TestStreamFromURLKeepsGzipBytes(t *testing.T) {
	t.Parallel()
	var zipped bytes.Buffer
	zw := gzip.NewWriter(&zipped)
	_, _ = zw.Write([]byte("hello, compressed"))
	_ = zw.Close()

	acceptEncoding := make(chan string, 1)
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		acceptEncoding <- r.Header.Get("Accept-Encoding")
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(zipped.Bytes())
	}))
	defer storage.Close()

	var out bytes.Buffer
	if _, err := StreamFromURL(context.Background(), &http.Client{}, storage.URL+"/obj.gz", &out); err != nil {
		t.Fatalf("StreamFromURL() error = %v", err)
	}
	if !bytes.Equal(out.Bytes(), zipped.Bytes()) {
		t.Fatalf("got %d bytes, want the %d stored gzip bytes unchanged", out.Len(), zipped.Len())
	}
	if got := <-acceptEncoding; got != "identity" {
		t.Fatalf("Accept-Encoding = %q, want identity", got)
	}
}

func TestStreamFromURLReportsStorageError(t *testing.T) {
	t.Parallel()

	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("<Error><Code>AccessDenied</Code><Message>Access Denied for org-42/user-7</Message></Error>"))
	}))
	defer storage.Close()

	_, err := StreamFromURL(context.Background(), &http.Client{}, storage.URL+"/obj", &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "HTTP 403") || !strings.Contains(err.Error(), "AccessDenied") {
		t.Fatalf("err = %v, want HTTP 403 with the AccessDenied code", err)
	}
	if strings.Contains(err.Error(), "Access Denied for") || strings.Contains(err.Error(), "org-42") {
		t.Fatalf("err = %v, must not include the storage body's message", err)
	}
}

func TestStreamFromURLReportsMissingObjectWithoutBody(t *testing.T) {
	t.Parallel()

	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>NoSuchKey</Code><Message>The specified key does not exist.</Message><Key>org-42/user-7/missing.csv</Key></Error>`))
	}))
	defer storage.Close()

	_, err := StreamFromURL(context.Background(), &http.Client{}, storage.URL+"/obj", &bytes.Buffer{})
	var storageErr *StorageError
	if !errors.As(err, &storageErr) || storageErr.StatusCode != http.StatusNotFound || storageErr.Code != "NoSuchKey" {
		t.Fatalf("err = %#v, want a StorageError of 404 with code NoSuchKey", err)
	}
	if got := err.Error(); got != "download failed with HTTP 404 (NoSuchKey)" {
		t.Fatalf("err = %q, want no key or message", got)
	}
}

func TestStorageErrorDropsCodeThatIsNotAPlainWord(t *testing.T) {
	t.Parallel()

	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("<Error><Code>bad code org-42/user-7</Code></Error>"))
	}))
	defer storage.Close()

	_, err := StreamFromURL(context.Background(), &http.Client{}, storage.URL+"/obj", &bytes.Buffer{})
	if err == nil || err.Error() != "download failed with HTTP 500" {
		t.Fatalf("err = %v, want only the status", err)
	}
}

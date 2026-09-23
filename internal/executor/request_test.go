package executor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"wherobots/cli/internal/auth"
	"wherobots/cli/internal/config"
	"wherobots/cli/internal/spec"
)

const sampleErrorEnvelope = `{"errors":[{"code":"BAD_REQUEST_ERROR","message":"Bad Request","details":"InvalidInputException (No storage source found for bucket: qni7xwfc8m)","path":"/files/upload-url","suggestion":"Update your request and try again.","documentation_url":null,"field":null}],"requestId":"3629d5f8a10139a4867c043509678f05"}`

func TestHTTPErrorFormatsStandardEnvelope(t *testing.T) {
	t.Parallel()

	err := &HTTPError{StatusCode: 400, Body: []byte(sampleErrorEnvelope)}
	got := err.Error()

	if !strings.HasPrefix(got, "request failed with HTTP 400") {
		t.Fatalf("expected 'request failed with HTTP 400' prefix, got %q", got)
	}
	for _, want := range []string{
		"BAD_REQUEST_ERROR",
		"Bad Request",
		"InvalidInputException (No storage source found for bucket: qni7xwfc8m)",
		"Update your request and try again.",
		"3629d5f8a10139a4867c043509678f05",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected output to contain %q, got %q", want, got)
		}
	}
	if !strings.Contains(got, "\n") {
		t.Fatalf("expected multi-line output, got %q", got)
	}
	if strings.Contains(got, "documentation") || strings.Contains(got, "field") {
		t.Fatalf("expected empty envelope fields to be omitted, got %q", got)
	}
}

func TestHTTPErrorFormatsMultipleEnvelopeErrors(t *testing.T) {
	t.Parallel()

	body := `{"errors":[{"code":"FIRST_ERROR","message":"first message"},{"code":"SECOND_ERROR","message":"second message","field":"name"}],"requestId":"req-1"}`
	got := (&HTTPError{StatusCode: 422, Body: []byte(body)}).Error()

	for _, want := range []string{"FIRST_ERROR", "first message", "SECOND_ERROR", "second message", "name", "req-1"} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected output to contain %q, got %q", want, got)
		}
	}
}

func TestHTTPErrorFallsBackOnNonJSONBody(t *testing.T) {
	t.Parallel()

	got := (&HTTPError{StatusCode: 502, Body: []byte("upstream timeout")}).Error()
	if got != "request failed with HTTP 502: upstream timeout" {
		t.Fatalf("expected raw fallback, got %q", got)
	}
}

func TestHTTPErrorFallsBackOnEmptyBody(t *testing.T) {
	t.Parallel()

	got := (&HTTPError{StatusCode: 500}).Error()
	if got != "request failed with HTTP 500" {
		t.Fatalf("expected bare fallback, got %q", got)
	}
}

func TestHTTPErrorFallsBackOnNonEnvelopeJSON(t *testing.T) {
	t.Parallel()

	got := (&HTTPError{StatusCode: 400, Body: []byte(`{"foo":"bar"}`)}).Error()
	if got != `request failed with HTTP 400: {"foo":"bar"}` {
		t.Fatalf("expected raw fallback, got %q", got)
	}
}

func TestHTTPErrorFallsBackOnErrorsArrayWithUnexpectedShape(t *testing.T) {
	t.Parallel()

	// An "errors" array whose items are not envelope objects (e.g. plain
	// strings) must not be mistaken for the standard envelope — rendering
	// it would drop all detail.
	body := `{"errors":["boom","bang"]}`
	got := (&HTTPError{StatusCode: 400, Body: []byte(body)}).Error()
	if got != `request failed with HTTP 400: `+body {
		t.Fatalf("expected raw fallback, got %q", got)
	}
}

func TestAPIErrorDetailsExtractsDetailsFromWrappedError(t *testing.T) {
	t.Parallel()

	httpErr := &HTTPError{StatusCode: 400, Body: []byte(sampleErrorEnvelope)}
	wrapped := errors.Join(errors.New("context"), httpErr)

	detail, ok := APIErrorDetails(wrapped)
	if !ok {
		t.Fatal("expected details to be found")
	}
	if !strings.Contains(detail, "No storage source found for bucket: qni7xwfc8m") {
		t.Fatalf("unexpected detail %q", detail)
	}

	if _, ok := APIErrorDetails(errors.New("plain")); ok {
		t.Fatal("expected no details for non-HTTP error")
	}
	if _, ok := APIErrorDetails(&HTTPError{StatusCode: 502, Body: []byte("upstream timeout")}); ok {
		t.Fatal("expected no details for non-envelope body")
	}
}

func TestJSONErrorReturnsBodyAndUnwrapsHTTPError(t *testing.T) {
	t.Parallel()

	httpErr := &HTTPError{StatusCode: 400, Body: []byte(sampleErrorEnvelope)}
	jsonErr := NewJSONError(httpErr)

	if got := jsonErr.Error(); got != sampleErrorEnvelope {
		t.Fatalf("expected raw JSON body, got %q", got)
	}

	var unwrapped *HTTPError
	if !errors.As(jsonErr, &unwrapped) {
		t.Fatal("expected errors.As to recover *HTTPError")
	}
	if unwrapped.StatusCode != 400 {
		t.Fatalf("expected status 400, got %d", unwrapped.StatusCode)
	}
}

func TestJSONErrorFallsBackOnNonJSONBody(t *testing.T) {
	t.Parallel()

	jsonErr := NewJSONError(&HTTPError{StatusCode: 502, Body: []byte("upstream timeout")})
	if got := jsonErr.Error(); got != "request failed with HTTP 502: upstream timeout" {
		t.Fatalf("expected raw fallback, got %q", got)
	}
}

// fakeCreds is a scriptable Credentials implementation. The real resolver's
// behavior is covered by internal/auth tests.
type fakeCreds struct {
	header       string // header name to set
	value        string
	applyErr     error
	refreshOK    bool
	refreshErr   error
	applyCalls   int
	refreshCalls int
}

func (f *fakeCreds) Apply(_ context.Context, req *http.Request) error {
	f.applyCalls++
	if f.applyErr != nil {
		return f.applyErr
	}
	req.Header.Set(f.header, f.value)
	return nil
}

func (f *fakeCreds) ForceRefresh(_ context.Context) (bool, error) {
	f.refreshCalls++
	return f.refreshOK, f.refreshErr
}

func apiKeyCreds(key string) *fakeCreds {
	return &fakeCreds{header: "x-api-key", value: key}
}

func TestBuildRequestInjectsPathQueryBodyAndAuth(t *testing.T) {
	t.Parallel()

	runtimeSpec := &spec.RuntimeSpec{BaseURL: "https://api.example.com"}
	op := &spec.Operation{
		Method:         "POST",
		Path:           "/users/{id}",
		PathParamOrder: []string{"id"},
		QueryParams: []spec.Parameter{
			{Name: "expand", Location: "query", Required: true},
		},
		RequestBody: &spec.RequestBodyInfo{
			Required:    true,
			ContentType: "application/json",
		},
	}

	req, err := BuildRequest(
		context.Background(),
		apiKeyCreds("abc123"),
		runtimeSpec,
		op,
		[]string{"u-1"},
		[]QueryPair{{Key: "expand", Value: "true"}},
		`{"name":"alice"}`,
	)
	if err != nil {
		t.Fatalf("BuildRequest() error = %v", err)
	}
	if req.URL.String() != "https://api.example.com/users/u-1?expand=true" {
		t.Fatalf("url = %s", req.URL.String())
	}
	if got := req.Header.Get("x-api-key"); got != "abc123" {
		t.Fatalf("x-api-key = %q, want %q", got, "abc123")
	}
	if got := req.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
}

func TestBuildRequestInjectsWherobotsClientHeader(t *testing.T) {
	// Not parallel: mutates the package-level Version var.
	prev := Version
	Version = "1.2.3"
	t.Cleanup(func() { Version = prev })

	creds := apiKeyCreds("abc123")
	runtimeSpec := &spec.RuntimeSpec{BaseURL: "https://api.example.com"}
	op := &spec.Operation{
		Method:      "GET",
		Path:        "/job-runs",
		CommandPath: []string{"job-runs", "list"},
	}

	req, err := BuildRequest(context.Background(), creds, runtimeSpec, op, nil, nil, "")
	if err != nil {
		t.Fatalf("BuildRequest() error = %v", err)
	}
	if got, want := req.Header.Get("X-Wherobots-Client"), "client=cli;ver=1.2.3;cmd=job-runs.list"; got != want {
		t.Fatalf("X-Wherobots-Client = %q, want %q", got, want)
	}
}

func TestBuildRequestClientHeaderPrefersContextCommand(t *testing.T) {
	// Not parallel: mutates the package-level Version var.
	prev := Version
	Version = "1.2.3"
	t.Cleanup(func() { Version = prev })

	creds := apiKeyCreds("abc123")
	runtimeSpec := &spec.RuntimeSpec{BaseURL: "https://api.example.com"}
	// op.CommandPath is the shared api-tree name; the context override must win.
	op := &spec.Operation{
		Method:      "POST",
		Path:        "/runs",
		CommandPath: []string{"runs", "create"},
	}

	ctx := WithCommand(context.Background(), "job-runs.create")
	req, err := BuildRequest(ctx, creds, runtimeSpec, op, nil, nil, `{}`)
	if err != nil {
		t.Fatalf("BuildRequest() error = %v", err)
	}
	if got, want := req.Header.Get("X-Wherobots-Client"), "client=cli;ver=1.2.3;cmd=job-runs.create"; got != want {
		t.Fatalf("X-Wherobots-Client = %q, want %q", got, want)
	}
}

func TestBuildRequestClientHeaderFallsBackToCommandPathWithoutContext(t *testing.T) {
	// Not parallel: mutates the package-level Version var.
	prev := Version
	Version = "1.2.3"
	t.Cleanup(func() { Version = prev })

	creds := apiKeyCreds("abc123")
	runtimeSpec := &spec.RuntimeSpec{BaseURL: "https://api.example.com"}
	op := &spec.Operation{
		Method:      "POST",
		Path:        "/runs",
		CommandPath: []string{"runs", "create"},
	}

	// No context command set: fall back to op.CommandPath.
	req, err := BuildRequest(context.Background(), creds, runtimeSpec, op, nil, nil, `{}`)
	if err != nil {
		t.Fatalf("BuildRequest() error = %v", err)
	}
	if got, want := req.Header.Get("X-Wherobots-Client"), "client=cli;ver=1.2.3;cmd=runs.create"; got != want {
		t.Fatalf("X-Wherobots-Client = %q, want %q", got, want)
	}
}

func TestBuildRequestWherobotsClientHeaderOmitsCommandWhenEmpty(t *testing.T) {
	// Not parallel: mutates the package-level Version var.
	prev := Version
	Version = "4.5.6"
	t.Cleanup(func() { Version = prev })

	creds := apiKeyCreds("abc123")
	runtimeSpec := &spec.RuntimeSpec{BaseURL: "https://api.example.com"}
	op := &spec.Operation{Method: "GET", Path: "/job-runs"}

	req, err := BuildRequest(context.Background(), creds, runtimeSpec, op, nil, nil, "")
	if err != nil {
		t.Fatalf("BuildRequest() error = %v", err)
	}
	if got, want := req.Header.Get("X-Wherobots-Client"), "client=cli;ver=4.5.6"; got != want {
		t.Fatalf("X-Wherobots-Client = %q, want %q", got, want)
	}
}

func TestBuildClientHeader(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		version string
		command string
		want    string
	}{
		{
			name:    "version and command",
			version: "1.2.3",
			command: "job-runs.list",
			want:    "client=cli;ver=1.2.3;cmd=job-runs.list",
		},
		{
			name:    "empty command omits cmd",
			version: "1.2.3",
			command: "",
			want:    "client=cli;ver=1.2.3",
		},
		{
			name:    "empty version falls back to dev",
			version: "",
			command: "job-runs.list",
			want:    "client=cli;ver=dev;cmd=job-runs.list",
		},
		{
			name:    "sanitizes separators out of values",
			version: "1,2;3",
			command: "a;b,c.list",
			want:    "client=cli;ver=1_2_3;cmd=a_b_c.list",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := buildClientHeader(tc.version, tc.command); got != tc.want {
				t.Fatalf("buildClientHeader(%q, %q) = %q, want %q", tc.version, tc.command, got, tc.want)
			}
		})
	}
}

func TestBuildRequestUsesResolverForOAuthBearer(t *testing.T) {
	t.Parallel()

	runtimeSpec := &spec.RuntimeSpec{BaseURL: "https://api.example.com"}
	op := &spec.Operation{Method: "GET", Path: "/users"}

	creds := &fakeCreds{header: "Authorization", value: "Bearer token-1"}
	req, err := BuildRequest(context.Background(), creds, runtimeSpec, op, nil, nil, "")
	if err != nil {
		t.Fatalf("BuildRequest() error = %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer token-1" {
		t.Fatalf("Authorization = %q", got)
	}
}

func TestBuildRequestMissingRequiredQueryReturnsError(t *testing.T) {
	t.Parallel()

	runtimeSpec := &spec.RuntimeSpec{BaseURL: "https://api.example.com"}
	op := &spec.Operation{
		Method:      "GET",
		Path:        "/users",
		QueryParams: []spec.Parameter{{Name: "limit", Location: "query", Required: true}},
	}

	_, err := BuildRequest(context.Background(), apiKeyCreds("abc123"), runtimeSpec, op, nil, nil, "")
	if err == nil || !strings.Contains(err.Error(), `missing required query parameter "limit"`) {
		t.Fatalf("expected required query error, got %v", err)
	}
}

func TestBuildRequestNoCredentialsReturnsResolverError(t *testing.T) {
	t.Parallel()

	runtimeSpec := &spec.RuntimeSpec{BaseURL: "https://api.example.com"}
	op := &spec.Operation{Method: "GET", Path: "/users"}

	creds := &fakeCreds{applyErr: errors.New("no credentials found")}
	_, err := BuildRequest(context.Background(), creds, runtimeSpec, op, nil, nil, "")
	if err == nil || !strings.Contains(err.Error(), "no credentials found") {
		t.Fatalf("expected credential error, got %v", err)
	}
}

func TestBuildRequestRealResolverErrorMentionsBothCredentialRoutes(t *testing.T) {
	t.Parallel()

	runtimeSpec := &spec.RuntimeSpec{BaseURL: "https://api.example.com"}
	op := &spec.Operation{Method: "GET", Path: "/users"}

	resolver := auth.NewResolver(config.Config{
		OpenAPIURL:      "https://api.cloud.wherobots.com/openapi.json",
		OAuthDomain:     "https://login.cloud.wherobots.com",
		CredentialsPath: filepath.Join(t.TempDir(), "credentials.json"),
	})
	_, err := BuildRequest(context.Background(), resolver, runtimeSpec, op, nil, nil, "")
	if err == nil {
		t.Fatalf("expected error")
	}
	for _, want := range []string{"WHEROBOTS_API_KEY", "wherobots auth login"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error should contain %q, got: %v", want, err)
		}
	}
}

func TestDoWithReauthReplaysOnceAfter401(t *testing.T) {
	t.Parallel()

	var calls int
	var authHeaders, bodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(body))
		authHeaders = append(authHeaders, r.Header.Get("Authorization"))
		if calls == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	creds := &fakeCreds{header: "Authorization", value: "Bearer stale", refreshOK: true}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, server.URL, strings.NewReader(`{"name":"alice"}`))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	if applyErr := creds.Apply(req.Context(), req); applyErr != nil {
		t.Fatalf("Apply() error = %v", applyErr)
	}
	creds.value = "Bearer fresh" // what the refresh would install

	body, err := DoWithReauth(server.Client(), req, creds)
	if err != nil {
		t.Fatalf("DoWithReauth() error = %v", err)
	}
	if string(body) != `{"ok":true}` {
		t.Fatalf("body = %s", body)
	}
	if creds.refreshCalls != 1 {
		t.Fatalf("refreshCalls = %d, want 1", creds.refreshCalls)
	}
	if calls != 2 {
		t.Fatalf("server calls = %d, want 2", calls)
	}
	if authHeaders[1] != "Bearer fresh" {
		t.Fatalf("replay Authorization = %q, want fresh bearer", authHeaders[1])
	}
	if bodies[1] != `{"name":"alice"}` {
		t.Fatalf("replay body = %q, want original body re-sent", bodies[1])
	}
}

func TestDoWithReauthDoesNotReplayForAPIKey(t *testing.T) {
	t.Parallel()

	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	creds := apiKeyCreds("key-1") // ForceRefresh reports false: nothing to refresh
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL, nil)

	_, err := DoWithReauth(server.Client(), req, creds)
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusUnauthorized {
		t.Fatalf("err = %v, want the original 401", err)
	}
	if calls != 1 {
		t.Fatalf("server calls = %d, want 1 (no replay)", calls)
	}
}

func TestDoWithReauthSurfacesRefreshError(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	creds := &fakeCreds{header: "Authorization", value: "Bearer stale", refreshErr: errors.New("session has expired")}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL, nil)

	_, err := DoWithReauth(server.Client(), req, creds)
	if err == nil || !strings.Contains(err.Error(), "session has expired") {
		t.Fatalf("err = %v, want actionable refresh error", err)
	}
}

func TestDoWithReauthPassesThroughNon401Errors(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	creds := &fakeCreds{header: "Authorization", value: "Bearer t", refreshOK: true}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL, nil)

	_, err := DoWithReauth(server.Client(), req, creds)
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusInternalServerError {
		t.Fatalf("err = %v, want HTTP 500 passthrough", err)
	}
	if creds.refreshCalls != 0 {
		t.Fatalf("refreshCalls = %d, want 0", creds.refreshCalls)
	}
}

func TestBuildRequestKeepsSlashesInMultiSegmentPathValue(t *testing.T) {
	t.Parallel()

	runtimeSpec := &spec.RuntimeSpec{BaseURL: "https://api.example.com"}
	op := &spec.Operation{
		Method:         "GET",
		Path:           "/storage/{storage_id}/files/{path}",
		PathParamOrder: []string{"storage_id", "path"},
	}

	req, err := BuildRequestMultiSegment(context.Background(), apiKeyCreds("k"), runtimeSpec, op,
		[]string{"path"}, []string{"user_files::aws-us-west-2", "reports/q 3#draft/a?b.csv"}, nil, "")
	if err != nil {
		t.Fatalf("BuildRequest() error = %v", err)
	}
	want := "https://api.example.com/storage/user_files::aws-us-west-2/files/reports/q%203%23draft/a%3Fb.csv"
	if got := req.URL.String(); got != want {
		t.Fatalf("url = %s, want %s", got, want)
	}
	if strings.Contains(req.URL.String(), "%2F") {
		t.Fatalf("url must not encode / as %%2F: %s", req.URL.String())
	}
}

func TestBuildRequestKeepsTrailingSlashOnFolderPathValue(t *testing.T) {
	t.Parallel()

	runtimeSpec := &spec.RuntimeSpec{BaseURL: "https://api.example.com"}
	op := &spec.Operation{
		Method:         "PUT",
		Path:           "/storage/{storage_id}/directories/{path}",
		PathParamOrder: []string{"storage_id", "path"},
	}

	req, err := BuildRequestMultiSegment(context.Background(), apiKeyCreds("k"), runtimeSpec, op,
		[]string{"path"}, []string{"s", "a/b c/"}, nil, "")
	if err != nil {
		t.Fatalf("BuildRequest() error = %v", err)
	}
	if got, want := req.URL.String(), "https://api.example.com/storage/s/directories/a/b%20c/"; got != want {
		t.Fatalf("url = %s, want %s", got, want)
	}
}

func TestBuildRequestSingleSegmentPathValueEscapesSlash(t *testing.T) {
	t.Parallel()

	runtimeSpec := &spec.RuntimeSpec{BaseURL: "https://api.example.com"}
	op := &spec.Operation{
		Method:         "GET",
		Path:           "/runs/{run_id}",
		PathParamOrder: []string{"run_id"},
	}

	req, err := BuildRequest(context.Background(), apiKeyCreds("k"), runtimeSpec, op,
		[]string{"run 1#x/y"}, nil, "")
	if err != nil {
		t.Fatalf("BuildRequest() error = %v", err)
	}
	if got, want := req.URL.String(), "https://api.example.com/runs/run%201%23x%2Fy"; got != want {
		t.Fatalf("url = %s, want %s", got, want)
	}
}

func TestBuildRequestMultiSegmentOnlyKeepsSlashesInNamedParams(t *testing.T) {
	t.Parallel()

	runtimeSpec := &spec.RuntimeSpec{BaseURL: "https://api.example.com"}
	op := &spec.Operation{
		Method:         "GET",
		Path:           "/storage/{storage_id}/files/{path}",
		PathParamOrder: []string{"storage_id", "path"},
	}

	req, err := BuildRequestMultiSegment(context.Background(), apiKeyCreds("k"), runtimeSpec, op,
		[]string{"path"}, []string{"a/b", "c/d"}, nil, "")
	if err != nil {
		t.Fatalf("BuildRequestMultiSegment() error = %v", err)
	}
	if got, want := req.URL.String(), "https://api.example.com/storage/a%2Fb/files/c/d"; got != want {
		t.Fatalf("url = %s, want %s", got, want)
	}
}

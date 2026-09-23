package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/tidwall/gjson"

	"wherobots/cli/internal/spec"
)

// Version identifies the CLI build for the advisory X-Wherobots-Client header.
// main.go overwrites it with the ldflags-injected build version; the "dev"
// default keeps local and test builds working without wiring.
var Version = "dev"

type QueryPair struct {
	Key   string
	Value string
}

// commandContextKey carries the user-facing command name for the
// X-Wherobots-Client header's cmd= field. Curated commands (e.g. `job-runs`)
// reuse shared api-subtree operations whose CommandPath is the api-tree name
// (e.g. runs.create), so they inject the invoked command name here to override
// it. Dynamic api commands leave it unset and fall back to op.CommandPath.
type commandContextKey struct{}

// WithCommand returns a context that carries the user-facing command name
// (dotted, e.g. "job-runs.create") for the X-Wherobots-Client header. An empty
// name leaves the context unchanged so the op.CommandPath fallback applies.
func WithCommand(ctx context.Context, command string) context.Context {
	if command == "" {
		return ctx
	}
	return context.WithValue(ctx, commandContextKey{}, command)
}

// commandFromContext returns the command name set by WithCommand, or "".
func commandFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	name, _ := ctx.Value(commandContextKey{}).(string)
	return name
}

// clientHeaderName is the ordered, append-only client-identification header.
// The CLI is an ORIGIN client, so it emits a single hop and never appends to
// an existing value. The header is advisory only and never affects auth.
const clientHeaderName = "X-Wherobots-Client"

// clientHeaderSanitizer strips characters that would break the hop grammar
// (commas separate hops, semicolons separate fields) so they can never appear
// inside a value.
var clientHeaderSanitizer = strings.NewReplacer(",", "_", ";", "_")

// buildClientHeader renders this CLI's single origin hop:
//
//	client=cli;ver=<version>;cmd=<command>
//
// The cmd field is omitted when command is empty. Values are sanitized so no
// comma or semicolon leaks into the grammar. An empty version falls back to
// "dev" to match the package default.
func buildClientHeader(version, command string) string {
	if version == "" {
		version = "dev"
	}
	var b strings.Builder
	b.WriteString("client=cli;ver=")
	b.WriteString(clientHeaderSanitizer.Replace(version))
	if command != "" {
		b.WriteString(";cmd=")
		b.WriteString(clientHeaderSanitizer.Replace(command))
	}
	return b.String()
}

type HTTPError struct {
	StatusCode int
	Body       []byte
}

func (e *HTTPError) Error() string {
	if env, ok := parseAPIErrorEnvelope(e.Body); ok {
		return formatAPIError(e.StatusCode, env)
	}
	if len(e.Body) == 0 {
		return fmt.Sprintf("request failed with HTTP %d", e.StatusCode)
	}
	return fmt.Sprintf("request failed with HTTP %d: %s", e.StatusCode, strings.TrimSpace(string(e.Body)))
}

// apiErrorDetail mirrors one entry of the standard Wherobots API error
// envelope: {"errors":[{code,message,details,suggestion,...}],"requestId"}.
type apiErrorDetail struct {
	Code       string
	Message    string
	Details    string
	Suggestion string
	Field      string
	DocURL     string
}

type apiErrorEnvelope struct {
	Errors    []apiErrorDetail
	RequestID string
}

// parseAPIErrorEnvelope reports ok only when body is JSON containing a
// non-empty "errors" array in the standard envelope shape.
func parseAPIErrorEnvelope(body []byte) (apiErrorEnvelope, bool) {
	if !gjson.ValidBytes(body) {
		return apiErrorEnvelope{}, false
	}
	items := gjson.GetBytes(body, "errors")
	if !items.IsArray() {
		return apiErrorEnvelope{}, false
	}
	entries := items.Array()
	if len(entries) == 0 {
		return apiErrorEnvelope{}, false
	}

	env := apiErrorEnvelope{RequestID: strings.TrimSpace(gjson.GetBytes(body, "requestId").String())}
	recognized := false
	for _, item := range entries {
		detail := apiErrorDetail{
			Code:       strings.TrimSpace(item.Get("code").String()),
			Message:    strings.TrimSpace(item.Get("message").String()),
			Details:    strings.TrimSpace(item.Get("details").String()),
			Suggestion: strings.TrimSpace(item.Get("suggestion").String()),
			Field:      strings.TrimSpace(item.Get("field").String()),
			DocURL:     strings.TrimSpace(item.Get("documentation_url").String()),
		}
		if detail.Code != "" || detail.Message != "" {
			recognized = true
		}
		env.Errors = append(env.Errors, detail)
	}
	// Require at least one entry in the standard shape so foreign payloads
	// that happen to carry an "errors" array fall back to the raw body.
	if !recognized {
		return apiErrorEnvelope{}, false
	}
	return env, true
}

func formatAPIError(statusCode int, env apiErrorEnvelope) string {
	var b strings.Builder
	fmt.Fprintf(&b, "request failed with HTTP %d", statusCode)
	if len(env.Errors) == 1 && env.Errors[0].Code != "" {
		fmt.Fprintf(&b, " (%s)", env.Errors[0].Code)
	}

	appendLine := func(label, value string) {
		if value == "" {
			return
		}
		fmt.Fprintf(&b, "\n  %-11s %s", label+":", value)
	}

	for idx, detail := range env.Errors {
		if len(env.Errors) > 1 {
			fmt.Fprintf(&b, "\nerror %d (%s)", idx+1, detail.Code)
		}
		appendLine("message", detail.Message)
		appendLine("details", detail.Details)
		appendLine("suggestion", detail.Suggestion)
		appendLine("field", detail.Field)
		appendLine("docs", detail.DocURL)
	}
	appendLine("request id", env.RequestID)
	return b.String()
}

// APIErrorDetails returns the "details" of the first envelope error when err
// wraps an *HTTPError whose body is a standard API error envelope.
func APIErrorDetails(err error) (string, bool) {
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) {
		return "", false
	}
	env, ok := parseAPIErrorEnvelope(httpErr.Body)
	if !ok {
		return "", false
	}
	return env.Errors[0].Details, true
}

// JSONError preserves the raw API error body so machine-oriented commands can
// emit the JSON envelope verbatim instead of the human-readable rendering.
type JSONError struct {
	httpErr *HTTPError
}

// NewJSONError wraps an *HTTPError so Error() returns the raw JSON body.
func NewJSONError(httpErr *HTTPError) *JSONError {
	return &JSONError{httpErr: httpErr}
}

func (e *JSONError) Error() string {
	if body := strings.TrimSpace(string(e.httpErr.Body)); body != "" && gjson.Valid(body) {
		return body
	}
	return e.httpErr.Error()
}

func (e *JSONError) Unwrap() error {
	return e.httpErr
}

// Credentials applies auth to outgoing requests and recovers from a 401 by
// refreshing. Implemented by auth.Resolver; declared here so the executor
// stays decoupled from credential storage.
type Credentials interface {
	// Apply sets the auth header, refreshing stored credentials first when
	// needed. It fails when no credential is configured at all.
	Apply(ctx context.Context, req *http.Request) error
	// ForceRefresh refreshes unconditionally after a 401 and reports whether
	// a replay with fresh credentials is worthwhile.
	ForceRefresh(ctx context.Context) (bool, error)
}

// BuildRequest builds an authenticated request. Every path parameter is
// escaped as one segment, so a "/" inside a value is sent as %2F.
func BuildRequest(
	ctx context.Context,
	creds Credentials,
	runtimeSpec *spec.RuntimeSpec,
	op *spec.Operation,
	pathArgs []string,
	queryPairs []QueryPair,
	jsonBody string,
) (*http.Request, error) {
	return buildRequest(ctx, creds, runtimeSpec, op, pathArgs, queryPairs, jsonBody, nil)
}

// BuildRequestMultiSegment is BuildRequest for routes whose named path
// parameters are greedy (e.g. FastAPI {path:path}): those keep "/" as-is.
func BuildRequestMultiSegment(
	ctx context.Context,
	creds Credentials,
	runtimeSpec *spec.RuntimeSpec,
	op *spec.Operation,
	multiSegment []string,
	pathArgs []string,
	queryPairs []QueryPair,
	jsonBody string,
) (*http.Request, error) {
	keep := make(map[string]bool, len(multiSegment))
	for _, name := range multiSegment {
		keep[name] = true
	}
	return buildRequest(ctx, creds, runtimeSpec, op, pathArgs, queryPairs, jsonBody, keep)
}

func buildRequest(
	ctx context.Context,
	creds Credentials,
	runtimeSpec *spec.RuntimeSpec,
	op *spec.Operation,
	pathArgs []string,
	queryPairs []QueryPair,
	jsonBody string,
	multiSegment map[string]bool,
) (*http.Request, error) {
	if runtimeSpec == nil || op == nil {
		return nil, fmt.Errorf("missing runtime operation context")
	}
	if runtimeSpec.BaseURL == "" {
		return nil, fmt.Errorf("missing base URL (no OpenAPI servers and WHEROBOTS_API_URL has no resolvable host)")
	}
	if len(pathArgs) != len(op.PathParamOrder) {
		return nil, fmt.Errorf("expected %d path arguments, got %d", len(op.PathParamOrder), len(pathArgs))
	}

	baseURL, err := url.Parse(runtimeSpec.BaseURL)
	if err != nil || !baseURL.IsAbs() {
		return nil, fmt.Errorf("invalid base URL %q", runtimeSpec.BaseURL)
	}

	resolvedPath := op.Path
	for idx, paramName := range op.PathParamOrder {
		escaped := url.PathEscape(pathArgs[idx])
		if multiSegment[paramName] {
			escaped = escapePathValue(pathArgs[idx])
		}
		resolvedPath = strings.ReplaceAll(resolvedPath, "{"+paramName+"}", escaped)
	}
	if strings.Contains(resolvedPath, "{") {
		return nil, fmt.Errorf("unresolved path parameters in %q", resolvedPath)
	}

	relativePath, err := url.Parse(resolvedPath)
	if err != nil {
		return nil, fmt.Errorf("parse operation path %q: %w", resolvedPath, err)
	}
	fullURL := baseURL.ResolveReference(relativePath)

	requiredQuery := make(map[string]struct{}, len(op.RequiredQueryParamNames()))
	for _, name := range op.RequiredQueryParamNames() {
		requiredQuery[name] = struct{}{}
	}

	seenQuery := make(map[string]struct{}, len(queryPairs))
	queryValues := fullURL.Query()
	for _, pair := range queryPairs {
		if pair.Key == "" {
			return nil, fmt.Errorf("query key cannot be empty")
		}
		queryValues.Set(pair.Key, pair.Value)
		seenQuery[pair.Key] = struct{}{}
	}
	for required := range requiredQuery {
		if _, exists := seenQuery[required]; !exists {
			return nil, fmt.Errorf("missing required query parameter %q", required)
		}
	}
	fullURL.RawQuery = queryValues.Encode()

	body := strings.TrimSpace(jsonBody)
	if body != "" && !gjson.Valid(body) {
		return nil, fmt.Errorf("--json must be valid JSON")
	}
	if op.RequestBody != nil && op.RequestBody.Required && body == "" {
		return nil, fmt.Errorf("request body is required")
	}

	var bodyReader io.Reader
	if body != "" {
		bodyReader = strings.NewReader(body)
	}

	req, err := http.NewRequestWithContext(ctx, op.Method, fullURL.String(), bodyReader)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	req.Header.Set("Accept", "application/json")
	if body != "" {
		contentType := "application/json"
		if op.RequestBody != nil && op.RequestBody.ContentType != "" {
			contentType = op.RequestBody.ContentType
		}
		req.Header.Set("Content-Type", contentType)
	}
	// Prefer the invoked command name carried on the context (set by curated
	// commands whose shared op.CommandPath is the api-tree name); fall back to
	// the operation's own CommandPath for dynamic api commands.
	command := commandFromContext(ctx)
	if command == "" {
		command = strings.Join(op.CommandPath, ".")
	}
	req.Header.Set(clientHeaderName, buildClientHeader(Version, command))
	if err := creds.Apply(ctx, req); err != nil {
		return nil, err
	}

	return req, nil
}

// escapePathValue percent-encodes each "/"-separated segment of a path
// parameter and rejoins them with "/", so greedy server routes such as
// FastAPI's {path:path} receive real path separators rather than %2F.
func escapePathValue(value string) string {
	segments := strings.Split(value, "/")
	for i, segment := range segments {
		segments[i] = url.PathEscape(segment)
	}
	return strings.Join(segments, "/")
}

func Do(client *http.Client, req *http.Request) ([]byte, error) {
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("execute request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &HTTPError{StatusCode: resp.StatusCode, Body: body}
	}
	return body, nil
}

// DoWithReauth executes the request and, on a 401 with refreshable
// credentials, forces one refresh and replays the request once. API-key 401s
// are terminal (there is nothing to refresh).
func DoWithReauth(client *http.Client, req *http.Request, creds Credentials) ([]byte, error) {
	body, err := Do(client, req)
	var httpErr *HTTPError
	if err == nil || !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusUnauthorized {
		return body, err
	}

	retry, ok, retryErr := refreshedReplay(req, creds)
	if retryErr != nil {
		return nil, retryErr
	}
	if !ok {
		return body, err
	}
	return Do(client, retry)
}

// refreshedReplay forces one credential refresh after a 401 and returns a
// clone of req carrying the fresh credentials and a rewound body. ok is false
// when there is nothing to refresh, so the caller keeps the original 401.
func refreshedReplay(req *http.Request, creds Credentials) (*http.Request, bool, error) {
	ok, refreshErr := creds.ForceRefresh(req.Context())
	if refreshErr != nil {
		// "Session expired — sign in again" beats a bare 401.
		return nil, false, refreshErr
	}
	if !ok {
		return nil, false, nil
	}

	retry := req.Clone(req.Context())
	if req.GetBody != nil {
		retryBody, bodyErr := req.GetBody()
		if bodyErr != nil {
			return nil, false, fmt.Errorf("rewind request body for 401 retry: %w", bodyErr)
		}
		retry.Body = retryBody
	}
	if applyErr := creds.Apply(retry.Context(), retry); applyErr != nil {
		return nil, false, applyErr
	}
	return retry, true, nil
}

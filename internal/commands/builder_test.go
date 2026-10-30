package commands

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"wherobots/cli/internal/auth"
	"wherobots/cli/internal/config"
	"wherobots/cli/internal/executor"
	"wherobots/cli/internal/spec"
)

func TestRootTreeOutput(t *testing.T) {
	t.Parallel()

	cfg := config.Config{AppName: "wherobots", HTTPTimeout: time.Second}
	runtimeSpec := &spec.RuntimeSpec{
		BaseURL: "https://api.example.com",
		Operations: []*spec.Operation{
			{Method: "GET", Path: "/users"},
			{Method: "GET", Path: "/users/{id}", PathParamOrder: []string{"id"}},
		},
	}

	root := BuildRootCommand(cfg, auth.NewResolver(cfg), runtimeSpec)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"--tree"})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "wherobots\n") ||
		!strings.Contains(got, "  api\n") ||
		!strings.Contains(got, "    users\n") ||
		!strings.Contains(got, "      get ") ||
		!strings.Contains(got, "      list ") {
		t.Fatalf("tree output missing expected nodes:\n%s", got)
	}
}

func TestTreeShowsSummaryOnLeafNodes(t *testing.T) {
	t.Parallel()

	cfg := config.Config{AppName: "wherobots", HTTPTimeout: time.Second}
	runtimeSpec := &spec.RuntimeSpec{
		BaseURL: "https://api.example.com",
		Operations: []*spec.Operation{
			{Method: "GET", Path: "/catalogs", Summary: "List all catalogs"},
			{Method: "POST", Path: "/catalogs", Summary: "Create a catalog"},
		},
	}

	root := BuildRootCommand(cfg, auth.NewResolver(cfg), runtimeSpec)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"--tree"})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	tree := out.String()
	if !strings.Contains(tree, "List all catalogs") {
		t.Errorf("tree should show summary 'List all catalogs':\n%s", tree)
	}
	if !strings.Contains(tree, "Create a catalog") {
		t.Errorf("tree should show summary 'Create a catalog':\n%s", tree)
	}
	// Group node (catalogs) should not have a trailing summary
	if strings.Contains(tree, "catalogs ") {
		t.Errorf("group node 'catalogs' should not have a summary:\n%s", tree)
	}
}

func TestDescriptionAppearsInHelp(t *testing.T) {
	t.Parallel()

	cfg := config.Config{AppName: "wherobots", HTTPTimeout: time.Second}
	runtimeSpec := &spec.RuntimeSpec{
		BaseURL: "https://api.example.com",
		Operations: []*spec.Operation{
			{
				Method:      "GET",
				Path:        "/catalogs",
				Summary:     "List all catalogs",
				Description: "Returns all catalogs accessible to the current user, including managed and foreign catalogs.",
			},
		},
	}

	root := BuildRootCommand(cfg, auth.NewResolver(cfg), runtimeSpec)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"api", "catalogs", "list", "--help"})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	help := out.String()
	if !strings.Contains(help, "Returns all catalogs accessible to the current user") {
		t.Errorf("help should contain the OpenAPI description:\n%s", help)
	}
}

func TestExcludedOperationsAbsentFromTree(t *testing.T) {
	t.Parallel()

	cfg := config.Config{AppName: "wherobots", HTTPTimeout: time.Second}
	runtimeSpec := &spec.RuntimeSpec{
		BaseURL: "https://api.example.com",
		Operations: []*spec.Operation{
			{Method: "GET", Path: "/catalogs", Summary: "List catalogs"},
			{Method: "POST", Path: "/management/org", Summary: "Superuser action", Excluded: true},
			{Method: "POST", Path: "/files/upload-url", Summary: "Legacy upload", Excluded: true},
		},
	}

	root := BuildRootCommand(cfg, auth.NewResolver(cfg), runtimeSpec)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"--tree"})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	tree := out.String()

	// Visible endpoint should be present
	if !strings.Contains(tree, "catalogs") {
		t.Errorf("tree should contain 'catalogs':\n%s", tree)
	}
	// Excluded endpoints must not appear
	if strings.Contains(tree, "management") {
		t.Errorf("tree must not contain excluded 'management':\n%s", tree)
	}
	if strings.Contains(tree, "files") {
		t.Errorf("tree must not contain excluded 'files':\n%s", tree)
	}
}

func TestDryRunOutputsCurl(t *testing.T) {
	t.Parallel()

	cfg := config.Config{AppName: "wherobots", APIKey: "test-key", HTTPTimeout: time.Second}
	runtimeSpec := &spec.RuntimeSpec{
		BaseURL: "https://api.example.com",
		Operations: []*spec.Operation{
			{
				Method:         "GET",
				Path:           "/users/{id}",
				PathParams:     []spec.Parameter{{Name: "id", Location: "path", Required: true, Type: "string"}},
				PathParamOrder: []string{"id"},
				QueryParams:    []spec.Parameter{{Name: "expand", Location: "query", Required: false, Type: "boolean"}},
			},
		},
	}

	root := BuildRootCommand(cfg, auth.NewResolver(cfg), runtimeSpec)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"api", "users", "get", "--id", "42", "--expand", "true", "--dry-run"})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	got := strings.TrimSpace(out.String())
	if !strings.HasPrefix(got, "curl -X GET 'https://api.example.com/users/42?expand=true'") {
		t.Fatalf("unexpected dry-run output: %s", got)
	}
}

func TestInvalidArgsReturnsUsageHint(t *testing.T) {
	t.Parallel()

	cfg := config.Config{AppName: "wherobots", HTTPTimeout: time.Second}
	runtimeSpec := &spec.RuntimeSpec{
		BaseURL: "https://api.example.com",
		Operations: []*spec.Operation{
			{
				Method:         "GET",
				Path:           "/users/{id}",
				PathParams:     []spec.Parameter{{Name: "id", Location: "path", Required: true, Type: "string"}},
				PathParamOrder: []string{"id"},
				RequestBody: &spec.RequestBodyInfo{
					Required:   true,
					SchemaType: "object",
					Fields: []spec.BodyField{
						{Name: "name", Type: "string", Required: true},
					},
				},
			},
		},
	}

	root := BuildRootCommand(cfg, auth.NewResolver(cfg), runtimeSpec)
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"api", "users", "get"})

	err := root.Execute()
	if err == nil {
		t.Fatalf("expected Execute() error")
	}

	message := err.Error()
	if !strings.Contains(message, "Did you mean to use the body:") ||
		!strings.Contains(message, "Required Path Params: [id]") ||
		!strings.Contains(message, "Required Body Params: [name]") ||
		!strings.Contains(message, "Expected Types:") {
		t.Fatalf("expected hint in error, got: %s", message)
	}
}

func TestObjectBodyFieldSerializesAsJSONObject(t *testing.T) {
	t.Parallel()

	cfg := config.Config{AppName: "wherobots", APIKey: "test-key", HTTPTimeout: time.Second}
	runtimeSpec := &spec.RuntimeSpec{
		BaseURL: "https://api.example.com",
		Operations: []*spec.Operation{
			{
				Method: "POST",
				Path:   "/runs",
				RequestBody: &spec.RequestBodyInfo{
					Required:   true,
					SchemaType: "object",
					Fields: []spec.BodyField{
						{Name: "name", Type: "string", Required: true},
						{Name: "runPython", Type: "object", Required: true},
					},
				},
			},
		},
	}

	root := BuildRootCommand(cfg, auth.NewResolver(cfg), runtimeSpec)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{
		"api", "runs", "create",
		"--name", "ftw-top5",
		"--runpython-json", `{"uri":"s3://bucket/ftw_top5.py"}`,
		"--dry-run",
	})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	got := out.String()
	if !strings.Contains(got, `"runPython":{"uri":"s3://bucket/ftw_top5.py"}`) {
		t.Fatalf("object body field should serialize as a JSON object, got:\n%s", got)
	}
}

func TestHelpShowsTypedFlagSamples(t *testing.T) {
	t.Parallel()

	cfg := config.Config{AppName: "wherobots", HTTPTimeout: time.Second}
	runtimeSpec := &spec.RuntimeSpec{
		BaseURL: "https://api.example.com",
		Operations: []*spec.Operation{
			{
				Method:         "PATCH",
				Path:           "/users/{id}",
				PathParams:     []spec.Parameter{{Name: "id", Location: "path", Required: true, Type: "string"}},
				PathParamOrder: []string{"id"},
				QueryParams:    []spec.Parameter{{Name: "limit", Location: "query", Required: false, Type: "integer"}},
				RequestBody: &spec.RequestBodyInfo{
					Required:   true,
					SchemaType: "object",
					Fields: []spec.BodyField{
						{Name: "enabled", Type: "boolean", Required: true},
						{Name: "metadata", Type: "object", Required: false},
					},
				},
			},
		},
	}

	root := BuildRootCommand(cfg, auth.NewResolver(cfg), runtimeSpec)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"api", "users", "update", "--help"})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	help := out.String()
	if !strings.Contains(help, "--limit string") ||
		!strings.Contains(help, "sample: 0") ||
		!strings.Contains(help, "--metadata-json string") ||
		!strings.Contains(help, "--json '{\"enabled\":false}'") {
		t.Fatalf("help missing expected typed guidance:\n%s", help)
	}
}

func TestAPICommandErrorReturnsRawJSONEnvelope(t *testing.T) {
	t.Parallel()

	envelope := `{"errors":[{"code":"BAD_REQUEST_ERROR","message":"Bad Request","details":"InvalidInputException (No storage source found for bucket: qni7xwfc8m)","path":"/files/upload-url","suggestion":"Update your request and try again."}],"requestId":"req-400"}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(envelope))
	}))
	defer server.Close()

	cfg := config.Config{AppName: "wherobots", APIKey: "test-key", HTTPTimeout: time.Second}
	runtimeSpec := &spec.RuntimeSpec{
		BaseURL: server.URL,
		Operations: []*spec.Operation{
			{Method: "GET", Path: "/users"},
		},
	}

	root := BuildRootCommand(cfg, auth.NewResolver(cfg), runtimeSpec)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"api", "users", "list"})

	err := root.Execute()
	if err == nil {
		t.Fatal("expected error from 400 response")
	}
	if got := err.Error(); got != envelope {
		t.Fatalf("expected raw JSON envelope, got %q", got)
	}

	var httpErr *executor.HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected wrapped *executor.HTTPError with status 400, got %v", err)
	}
}

func TestAPICommandEmptyResponseBody(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		status  int
		wantErr string
	}{
		{name: "204 succeeds", status: http.StatusNoContent},
		{name: "202 succeeds", status: http.StatusAccepted},
		{name: "500 fails", status: http.StatusInternalServerError, wantErr: "request failed with HTTP 500"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var gotPath string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				w.WriteHeader(tt.status)
			}))
			defer server.Close()

			cfg := config.Config{AppName: "wherobots", APIKey: "test-key", HTTPTimeout: time.Second}
			runtimeSpec := &spec.RuntimeSpec{
				BaseURL: server.URL,
				Operations: []*spec.Operation{
					{
						Method:         "DELETE",
						Path:           "/triggers/{id}",
						PathParams:     []spec.Parameter{{Name: "id", Location: "path", Required: true, Type: "string"}},
						PathParamOrder: []string{"id"},
					},
				},
			}

			root := BuildRootCommand(cfg, auth.NewResolver(cfg), runtimeSpec)
			var stdout, stderr bytes.Buffer
			root.SetOut(&stdout)
			root.SetErr(&stderr)
			root.SetArgs([]string{"api", "triggers", "delete", "--id", "t-1", "--yes"})

			err := root.Execute()

			if tt.wantErr == "" && err != nil {
				t.Fatalf("Execute() error = %v", err)
			}
			if tt.wantErr != "" && (err == nil || err.Error() != tt.wantErr) {
				t.Fatalf("Execute() error = %v, want %q", err, tt.wantErr)
			}
			if gotPath != "/triggers/t-1" {
				t.Fatalf("server saw path %q, want /triggers/t-1", gotPath)
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout = %q, want empty", stdout.String())
			}
		})
	}
}

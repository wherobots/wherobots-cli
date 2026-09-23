package commands

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"wherobots/cli/internal/auth"
	"wherobots/cli/internal/config"
	"wherobots/cli/internal/spec"
)

func filesTestRuntimeSpec(baseURL string) *spec.RuntimeSpec {
	op := func(method, suffix string, query ...spec.Parameter) *spec.Operation {
		return &spec.Operation{
			Method:         method,
			Path:           "/storage/{storage_id}/" + suffix + "/{path}",
			PathParamOrder: []string{"storage_id", "path"},
			PathParams: []spec.Parameter{
				{Name: "storage_id", Location: "path", Required: true, Type: "string"},
				{Name: "path", Location: "path", Required: true, Type: "string"},
			},
			QueryParams: query,
		}
	}
	return &spec.RuntimeSpec{
		BaseURL: baseURL,
		Operations: []*spec.Operation{
			{Method: "GET", Path: "/organization"},
			op("GET", "directories",
				spec.Parameter{Name: "cursor", Location: "query", Type: "string"},
				spec.Parameter{Name: "limit", Location: "query", Type: "integer"}),
			op("PUT", "directories"),
			op("DELETE", "directories"),
			op("POST", "file-upload-url"),
			op("GET", "files"),
			op("DELETE", "files"),
			op("POST", "file-rename", spec.Parameter{Name: "new_name", Location: "query", Required: true, Type: "string"}),
		},
	}
}

func buildFilesTestRoot(baseURL string) *cobra.Command {
	cfg := config.Config{AppName: "wherobots", APIKey: "test-key", HTTPTimeout: time.Second}
	return BuildRootCommand(cfg, auth.NewResolver(cfg), filesTestRuntimeSpec(baseURL))
}

type filesRequest struct {
	Method string
	Path   string
	Query  string
	Client string
}

// filesMock is a mock API whose org default region is aws-us-west-2.
type filesMock struct {
	mu       sync.Mutex
	requests []filesRequest
	server   *httptest.Server
}

func newFilesMock(t *testing.T, handle func(w http.ResponseWriter, r *http.Request)) *filesMock {
	t.Helper()
	m := &filesMock{}
	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.requests = append(m.requests, filesRequest{r.Method, r.URL.EscapedPath(), r.URL.RawQuery, r.Header.Get("X-Wherobots-Client")})
		m.mu.Unlock()
		if r.URL.Path == "/organization" {
			_, _ = io.WriteString(w, `{"defaultRegion":"aws-us-west-2","allowedRegions":["aws-us-west-2","aws-eu-west-1"]}`)
			return
		}
		handle(w, r)
	}))
	t.Cleanup(m.server.Close)
	return m
}

func (m *filesMock) run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := buildFilesTestRoot(m.server.URL)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

func (m *filesMock) storageRequests() []filesRequest {
	var out []filesRequest
	for _, r := range m.requests {
		if strings.HasPrefix(r.Path, "/storage/") {
			out = append(out, r)
		}
	}
	return out
}

const westPrefix = "/storage/user_files::aws-us-west-2"

func TestFilesLsUsesOrgDefaultRegionAndPrintsTable(t *testing.T) {
	t.Parallel()
	m := newFilesMock(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"items":[{"name":"sub","path":"x/sub/","type":"FOLDER"},{"name":"a.csv","path":"x/a.csv","type":"FILE","size":2048,"lastModified":"2026-09-01T00:00:00Z"}],"next_page":null}`)
	})
	out, err := m.run(t, "files", "my-files", "ls", "reports")
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	for _, want := range []string{"TYPE", "SIZE", "MODIFIED", "NAME", "folder", "sub/", "2 KB", "a.csv"} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q:\n%s", want, out)
		}
	}
	reqs := m.storageRequests()
	if len(reqs) != 1 || reqs[0].Path != westPrefix+"/directories/reports/" {
		t.Fatalf("requests = %+v", reqs)
	}
	if reqs[0].Client == "" || !strings.Contains(reqs[0].Client, "cmd=files.my-files.ls") {
		t.Fatalf("X-Wherobots-Client = %q, want cmd=files.my-files.ls", reqs[0].Client)
	}
}

func TestFilesRegionFlagOverridesOrgDefault(t *testing.T) {
	t.Parallel()
	m := newFilesMock(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"items":[]}`)
	})
	out, err := m.run(t, "files", "my-files", "--region", "aws-eu-west-1", "ls", "--output", "json")
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if strings.TrimSpace(out) != "[]" {
		t.Fatalf("json output = %q, want []", out)
	}
	for _, r := range m.requests {
		if r.Path == "/organization" {
			t.Fatalf("--region given, yet /organization was fetched")
		}
	}
	reqs := m.storageRequests()
	if len(reqs) != 1 || reqs[0].Path != "/storage/user_files::aws-eu-west-1/directories/" {
		t.Fatalf("requests = %+v", reqs)
	}
}

func TestFilesMkdirNested(t *testing.T) {
	t.Parallel()
	m := newFilesMock(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	})
	if _, err := m.run(t, "files", "my-files", "mkdir", "a/b/c"); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	reqs := m.storageRequests()
	want := []string{"a/", "a/b/", "a/b/c/"}
	if len(reqs) != len(want) {
		t.Fatalf("requests = %+v", reqs)
	}
	for i, r := range reqs {
		if r.Method != http.MethodPut || r.Path != westPrefix+"/directories/"+want[i] {
			t.Fatalf("request %d = %+v", i, r)
		}
	}
}

func TestFilesUploadDownloadAndCat(t *testing.T) {
	t.Parallel()
	var (
		stored     []byte
		storageHdr http.Header
	)
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			stored, _ = io.ReadAll(r.Body)
			return
		}
		storageHdr = r.Header.Clone()
		_, _ = w.Write(stored)
	}))
	defer storage.Close()

	m := newFilesMock(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/file-upload-url/"):
			_, _ = fmt.Fprintf(w, `{"destination":"s3://b/k","uploadUrl":%q}`, storage.URL+"/put")
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/files/"):
			http.Redirect(w, r, storage.URL+"/get?sig=1", http.StatusTemporaryRedirect)
		default:
			http.NotFound(w, r)
		}
	})

	dir := t.TempDir()
	local := filepath.Join(dir, "q3.csv")
	if err := os.WriteFile(local, []byte("a,b\n1,2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.run(t, "files", "my-files", "upload", "reports/", local); err != nil {
		t.Fatalf("upload error = %v", err)
	}
	if string(stored) != "a,b\n1,2\n" {
		t.Fatalf("stored = %q", stored)
	}

	outDir := filepath.Join(dir, "out")
	if err := os.Mkdir(outDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := m.run(t, "files", "my-files", "download", "reports/q3.csv", outDir); err != nil {
		t.Fatalf("download error = %v", err)
	}
	got, err := os.ReadFile(filepath.Join(outDir, "q3.csv"))
	if err != nil || string(got) != "a,b\n1,2\n" {
		t.Fatalf("downloaded = %q, %v", got, err)
	}
	if storageHdr.Get("x-api-key") != "" || storageHdr.Get("Authorization") != "" {
		t.Fatalf("storage GET carried credentials: %v", storageHdr)
	}
	leftovers, _ := filepath.Glob(filepath.Join(outDir, ".*partial*"))
	if len(leftovers) != 0 {
		t.Fatalf("temporary files left behind: %v", leftovers)
	}

	out, err := m.run(t, "files", "my-files", "cat", "reports/q3.csv")
	if err != nil || out != "a,b\n1,2\n" {
		t.Fatalf("cat = %q, %v", out, err)
	}
}

func TestFilesDownloadFailureLeavesNoFile(t *testing.T) {
	t.Parallel()
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer storage.Close()
	m := newFilesMock(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, storage.URL+"/get", http.StatusTemporaryRedirect)
	})

	dest := filepath.Join(t.TempDir(), "x.csv")
	if _, err := m.run(t, "files", "my-files", "download", "x.csv", dest); err == nil {
		t.Fatalf("expected an error")
	}
	entries, _ := os.ReadDir(filepath.Dir(dest))
	if len(entries) != 0 {
		t.Fatalf("files left behind after a failed download: %v", entries)
	}
}

func TestFilesMvRmRmdir(t *testing.T) {
	t.Parallel()
	m := newFilesMock(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"items":[]}`)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	if _, err := m.run(t, "files", "my-files", "mv", "reports/q3.csv", "reports/final.csv"); err != nil {
		t.Fatalf("mv error = %v", err)
	}
	if _, err := m.run(t, "files", "my-files", "rm", "reports/final.csv"); err != nil {
		t.Fatalf("rm error = %v", err)
	}
	if _, err := m.run(t, "files", "my-files", "rmdir", "reports"); err != nil {
		t.Fatalf("rmdir error = %v", err)
	}

	reqs := m.storageRequests()
	want := []filesRequest{
		{Method: "POST", Path: westPrefix + "/file-rename/reports/q3.csv", Query: "new_name=final.csv"},
		{Method: "DELETE", Path: westPrefix + "/files/reports/final.csv"},
		{Method: "GET", Path: westPrefix + "/directories/reports/", Query: "limit=1"},
		{Method: "DELETE", Path: westPrefix + "/directories/reports/"},
	}
	if len(reqs) != len(want) {
		t.Fatalf("requests = %+v", reqs)
	}
	for i := range want {
		if reqs[i].Method != want[i].Method || reqs[i].Path != want[i].Path || reqs[i].Query != want[i].Query {
			t.Fatalf("request %d = %+v, want %+v", i, reqs[i], want[i])
		}
	}
}

func TestFilesMvRefusesAnotherFolder(t *testing.T) {
	t.Parallel()
	m := newFilesMock(t, func(http.ResponseWriter, *http.Request) {})
	_, err := m.run(t, "files", "my-files", "mv", "reports/q3.csv", "archive/q3.csv")
	if err == nil || !strings.Contains(err.Error(), "moves between folders are not supported") {
		t.Fatalf("err = %v", err)
	}
	if len(m.requests) != 0 {
		t.Fatalf("no request should be sent, got %+v", m.requests)
	}
}

func TestFilesNotEnabledNamesDriveAndRegion(t *testing.T) {
	t.Parallel()
	m := newFilesMock(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	_, err := m.run(t, "files", "my-files", "ls")
	if err == nil || err.Error() != "Files is not enabled for my-files in region aws-us-west-2" {
		t.Fatalf("err = %v", err)
	}
}

func TestFilesDryRunPrintsCurlAndSendsNothing(t *testing.T) {
	t.Parallel()
	m := newFilesMock(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	out, err := m.run(t, "files", "my-files", "--region", "aws-us-west-2", "--dry-run", "rm", "a/b.csv")
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if len(m.requests) != 0 {
		t.Fatalf("dry-run sent %+v", m.requests)
	}
	if !strings.Contains(out, "curl -X DELETE") || !strings.Contains(out, westPrefix+"/files/a/b.csv") {
		t.Fatalf("output = %q", out)
	}
	if strings.Contains(out, "test-key") {
		t.Fatalf("dry-run output leaked the API key: %q", out)
	}
}

func TestFilesDryRunDownloadPrintsOnlyAPIRequest(t *testing.T) {
	t.Parallel()
	m := newFilesMock(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	dest := filepath.Join(t.TempDir(), "x.csv")
	out, err := m.run(t, "files", "my-files", "--region", "r1", "--dry-run", "download", "x.csv", dest)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if strings.Count(out, "curl ") != 1 || !strings.Contains(out, "/storage/user_files::r1/files/x.csv") {
		t.Fatalf("output = %q", out)
	}
	if _, statErr := os.Stat(dest); statErr == nil {
		t.Fatalf("dry-run wrote a file")
	}
	if len(m.requests) != 0 {
		t.Fatalf("dry-run sent %+v", m.requests)
	}
}

func TestFilesArgumentErrors(t *testing.T) {
	t.Parallel()
	m := newFilesMock(t, func(http.ResponseWriter, *http.Request) {})
	cases := [][]string{
		{"files", "my-files", "upload", "reports/q3.csv"},
		{"files", "my-files", "mkdir"},
		{"files", "my-files", "rm", "a", "b"},
		{"files", "my-files", "download"},
	}
	for _, args := range cases {
		_, err := m.run(t, args...)
		if err == nil || !strings.Contains(err.Error(), "Usage:") {
			t.Errorf("%v: err = %v, want a usage error", args, err)
		}
	}
	if len(m.requests) != 0 {
		t.Fatalf("argument errors must not send requests, got %+v", m.requests)
	}
}

func TestFilesGroupSkippedWhenSpecLacksRoutes(t *testing.T) {
	t.Parallel()
	cfg := config.Config{AppName: "wherobots", APIKey: "k", HTTPTimeout: time.Second}
	root := BuildRootCommand(cfg, auth.NewResolver(cfg), &spec.RuntimeSpec{BaseURL: "https://api.example.com"})
	for _, c := range root.Commands() {
		if c.Name() == "files" {
			t.Fatalf("files group registered without storage routes")
		}
	}
}

package files

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"wherobots/cli/internal/spec"
)

type testCreds struct{}

func (testCreds) Apply(_ context.Context, req *http.Request) error {
	req.Header.Set("x-api-key", "test-key")
	return nil
}

func (testCreds) ForceRefresh(context.Context) (bool, error) { return false, nil }

func storageOp(method, suffix string, query ...spec.Parameter) *spec.Operation {
	return &spec.Operation{
		Method:         method,
		Path:           "/storage/{storage_id}/" + suffix + "/{path}",
		PathParamOrder: []string{"storage_id", "path"},
		QueryParams:    query,
	}
}

func testOperations() Operations {
	return Operations{
		ListDirectory: storageOp("GET", "directories",
			spec.Parameter{Name: "cursor", Location: "query"},
			spec.Parameter{Name: "limit", Location: "query", Type: "integer"}),
		CreateDirectory: storageOp("PUT", "directories"),
		DeleteDirectory: storageOp("DELETE", "directories"),
		CreateUploadURL: storageOp("POST", "file-upload-url"),
		DownloadFile:    storageOp("GET", "files"),
		DeleteFile:      storageOp("DELETE", "files"),
		RenameFile:      storageOp("POST", "file-rename", spec.Parameter{Name: "new_name", Location: "query", Required: true}),
	}
}

// recorded is one request seen by the mock API, with the escaped path.
type recorded struct {
	Method string
	Path   string
	Query  string
}

type mockAPI struct {
	mu       sync.Mutex
	requests []recorded
	server   *httptest.Server
}

func newMockAPI(t *testing.T, handle func(w http.ResponseWriter, r *http.Request)) *mockAPI {
	t.Helper()
	m := &mockAPI{}
	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.requests = append(m.requests, recorded{Method: r.Method, Path: r.URL.EscapedPath(), Query: r.URL.RawQuery})
		m.mu.Unlock()
		handle(w, r)
	}))
	t.Cleanup(m.server.Close)
	return m
}

func (m *mockAPI) drive(t *testing.T, dryRun *bytes.Buffer) *DriveClient {
	t.Helper()
	svc := &Service{
		Runtime:  &spec.RuntimeSpec{BaseURL: m.server.URL},
		Creds:    testCreds{},
		API:      m.server.Client(),
		Transfer: &http.Client{},
		Ops:      testOperations(),
	}
	if dryRun != nil {
		svc.DryRun = dryRun
	}
	d, err := svc.Open(MyFiles("aws-us-west-2"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	return d
}

const prefix = "/storage/user_files::aws-us-west-2"

func TestStorageIDForMyFiles(t *testing.T) {
	t.Parallel()
	id, err := MyFiles("aws-eu-west-1").StorageID()
	if err != nil || id != "user_files::aws-eu-west-1" {
		t.Fatalf("StorageID() = %q, %v", id, err)
	}
	if _, err := MyFiles("").StorageID(); err == nil {
		t.Fatalf("expected an error for an empty region")
	}
}

func TestParseRemotePathRefusesBadLevels(t *testing.T) {
	t.Parallel()
	for _, bad := range []string{"a//b", "a/./b", "../a", "a/..", "//a"} {
		if _, err := parseRemotePath(bad); err == nil {
			t.Errorf("parseRemotePath(%q) = nil error, want refusal", bad)
		}
	}
	p, err := parseRemotePath("/a/b/")
	if err != nil || p.folderPath() != "a/b/" || !p.folder {
		t.Fatalf("parseRemotePath(/a/b/) = %+v, %v", p, err)
	}
}

func TestListPaginatesWithExplicitLimit(t *testing.T) {
	t.Parallel()
	api := newMockAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("cursor") == "" {
			_, _ = fmt.Fprint(w, `{"items":[{"name":"a.csv","path":"p/a.csv","type":"FILE","size":3}],"next_page":"tok 2"}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"items":[{"name":"sub","path":"p/sub/","type":"FOLDER"}],"next_page":null}`)
	})

	entries, err := api.drive(t, nil).List(context.Background(), "/reports/2026")
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(entries) != 2 || entries[0].Name != "a.csv" || !entries[1].IsFolder() {
		t.Fatalf("entries = %+v", entries)
	}
	if len(api.requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(api.requests))
	}
	for _, req := range api.requests {
		if req.Path != prefix+"/directories/reports/2026/" {
			t.Fatalf("path = %s", req.Path)
		}
		if !strings.Contains(req.Query, "limit=1000") {
			t.Fatalf("query %q is missing limit=1000", req.Query)
		}
	}
	if !strings.Contains(api.requests[1].Query, "cursor=tok+2") {
		t.Fatalf("second query %q is missing the cursor", api.requests[1].Query)
	}
}

func TestMkdirCreatesEachLevelInOrder(t *testing.T) {
	t.Parallel()
	api := newMockAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = fmt.Fprint(w, `{"items":[{"name":"a","path":"a/","type":"FOLDER"}],"next_page":null}`)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/directories/a/") {
			w.WriteHeader(http.StatusConflict) // already exists
			return
		}
		w.WriteHeader(http.StatusCreated)
	})

	if err := api.drive(t, nil).Mkdir(context.Background(), "a/b c/d"); err != nil {
		t.Fatalf("Mkdir() error = %v", err)
	}
	want := []recorded{
		{Method: http.MethodPut, Path: prefix + "/directories/a/"},
		{Method: http.MethodGet, Path: prefix + "/directories/"},
		{Method: http.MethodPut, Path: prefix + "/directories/a/b%20c/"},
		{Method: http.MethodPut, Path: prefix + "/directories/a/b%20c/d/"},
	}
	if len(api.requests) != len(want) {
		t.Fatalf("requests = %+v", api.requests)
	}
	for i, req := range api.requests {
		if req.Method != want[i].Method || req.Path != want[i].Path {
			t.Fatalf("request %d = %s %s, want %s %s", i, req.Method, req.Path, want[i].Method, want[i].Path)
		}
	}
}

func TestMkdirRefusesWhenAFileHasTheName(t *testing.T) {
	t.Parallel()
	api := newMockAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = fmt.Fprint(w, `{"items":[{"name":"b","path":"a/b","type":"FILE","size":1}],"next_page":null}`)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/directories/a/b/") {
			w.WriteHeader(http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusCreated)
	})

	err := api.drive(t, nil).Mkdir(context.Background(), "a/b/c")
	if err == nil || !strings.Contains(err.Error(), "a file with that name already exists") {
		t.Fatalf("err = %v, want the file-exists refusal", err)
	}
	last := api.requests[len(api.requests)-1]
	if last.Method != http.MethodGet || last.Path != prefix+"/directories/a/" {
		t.Fatalf("last request = %+v, want the listing of a/ and no PUT for a/b/c/", last)
	}
}

func TestMkdirContinuesWhenAFolderAndAFileShareTheName(t *testing.T) {
	t.Parallel()
	api := newMockAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = fmt.Fprint(w, `{"items":[{"name":"b","path":"a/b","type":"FILE","size":1},{"name":"b/","path":"a/b/","type":"FOLDER"}],"next_page":null}`)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/directories/a/b/") {
			w.WriteHeader(http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusCreated)
	})

	if err := api.drive(t, nil).Mkdir(context.Background(), "a/b/c"); err != nil {
		t.Fatalf("err = %v, want nil: the folder a/b/ exists beside the file a/b", err)
	}
	last := api.requests[len(api.requests)-1]
	if last.Method != http.MethodPut || last.Path != prefix+"/directories/a/b/c/" {
		t.Fatalf("last request = %+v, want the PUT for a/b/c/", last)
	}
}

func TestErrorMappingNotSignedIn(t *testing.T) {
	t.Parallel()
	api := newMockAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	err := api.drive(t, nil).DeleteFile(context.Background(), "a.csv")
	var target *NotSignedInError
	if !errors.As(err, &target) || !strings.Contains(err.Error(), "wherobots auth login") || !strings.Contains(err.Error(), "WHEROBOTS_API_KEY") {
		t.Fatalf("err = %v, want not-signed-in with both hints", err)
	}
}

func TestErrorMappingNotEnabledWhenRootIsMissing(t *testing.T) {
	t.Parallel()
	api := newMockAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	err := api.drive(t, nil).DeleteFile(context.Background(), "a.csv")
	var target *NotEnabledError
	if !errors.As(err, &target) || err.Error() != "Files is not enabled for my-files in region aws-us-west-2" {
		t.Fatalf("err = %v, want not-enabled", err)
	}
	if len(api.requests) != 2 || api.requests[1].Path != prefix+"/directories/" {
		t.Fatalf("expected one root probe after the 404, got %+v", api.requests)
	}
}

func TestErrorMappingNoSuchFileWhenRootExists(t *testing.T) {
	t.Parallel()
	api := newMockAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == prefix+"/directories/" {
			_, _ = fmt.Fprint(w, `{"items":[],"path":"","name":""}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	err := api.drive(t, nil).DeleteFile(context.Background(), "missing.csv")
	var target *NotFoundError
	if !errors.As(err, &target) || err.Error() != "no such file or folder: missing.csv" {
		t.Fatalf("err = %v, want no-such-file", err)
	}
}

func TestDeleteDirRefusesNonEmptyUnlessRecursive(t *testing.T) {
	t.Parallel()
	api := newMockAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = fmt.Fprint(w, `{"items":[{"name":"x.csv","path":"p/x.csv","type":"FILE"}]}`)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	d := api.drive(t, nil)

	err := d.DeleteDir(context.Background(), "data", false)
	var notEmpty *FolderNotEmptyError
	if !errors.As(err, &notEmpty) {
		t.Fatalf("err = %v, want folder-not-empty", err)
	}
	for _, req := range api.requests {
		if req.Method == http.MethodDelete {
			t.Fatalf("a DELETE was sent for a non-empty folder without --recursive")
		}
	}

	if err := d.DeleteDir(context.Background(), "data", true); err != nil {
		t.Fatalf("DeleteDir(recursive) error = %v", err)
	}
	last := api.requests[len(api.requests)-1]
	if last.Method != http.MethodDelete || last.Path != prefix+"/directories/data/" {
		t.Fatalf("last request = %+v", last)
	}
}

func TestDeleteDirRefusesWhenMarkerHasNextPage(t *testing.T) {
	t.Parallel()
	api := newMockAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = fmt.Fprint(w, `{"items":[{"name":"","type":"FOLDER"}],"next_page":"c1"}`)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	err := api.drive(t, nil).DeleteDir(context.Background(), "reports", false)
	var notEmpty *FolderNotEmptyError
	if !errors.As(err, &notEmpty) {
		t.Fatalf("err = %v, want folder-not-empty", err)
	}
	for _, req := range api.requests {
		if req.Method == http.MethodDelete {
			t.Fatalf("a DELETE was sent although next_page showed more entries")
		}
	}
}

func TestDeleteFileRefusesFolderPath(t *testing.T) {
	t.Parallel()
	api := newMockAPI(t, func(http.ResponseWriter, *http.Request) {})
	err := api.drive(t, nil).DeleteFile(context.Background(), "data/")
	if err == nil || !strings.Contains(err.Error(), "rmdir") {
		t.Fatalf("err = %v, want a pointer to rmdir", err)
	}
	if len(api.requests) != 0 {
		t.Fatalf("no request should be sent, got %+v", api.requests)
	}
}

func TestUploadAppendsLocalNameToFolderTarget(t *testing.T) {
	t.Parallel()
	var putBody string
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := new(bytes.Buffer)
		_, _ = b.ReadFrom(r.Body)
		putBody = b.String()
	}))
	defer storage.Close()

	api := newMockAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `{"destination":"s3://b/k","uploadUrl":%q}`, storage.URL+"/put")
	})
	local := filepath.Join(t.TempDir(), "q3.csv")
	if err := os.WriteFile(local, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}

	target, err := api.drive(t, nil).Upload(context.Background(), "reports/", local)
	if err != nil {
		t.Fatalf("Upload() error = %v", err)
	}
	if target != "reports/q3.csv" || putBody != "hello" {
		t.Fatalf("target = %q, putBody = %q", target, putBody)
	}
	if api.requests[0].Method != http.MethodPost || api.requests[0].Path != prefix+"/file-upload-url/reports/q3.csv" {
		t.Fatalf("request = %+v", api.requests[0])
	}
}

func TestRenameTarget(t *testing.T) {
	t.Parallel()
	cases := []struct {
		src, dst, want string
		wantErr        bool
	}{
		{"reports/q3.csv", "final.csv", "final.csv", false},
		{"reports/q3.csv", "reports/final.csv", "final.csv", false},
		{"/reports/q3.csv", "reports/final.csv", "final.csv", false},
		{"q3.csv", "/final.csv", "final.csv", false},
		{"reports/q3.csv", "archive/final.csv", "", true},
		{"reports/q3.csv", "..", "", true},
		{"reports/q3.csv", "reports/", "", true},
		{"reports/", "archive", "", true},
	}
	for _, tc := range cases {
		got, err := RenameTarget(tc.src, tc.dst)
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("RenameTarget(%q, %q) = %q, %v", tc.src, tc.dst, got, err)
		}
	}
	if _, err := RenameTarget("a/x", "b/y"); err == nil || !strings.Contains(err.Error(), "moves between folders are not supported") {
		t.Fatalf("err = %v, want the between-folders message", err)
	}
}

func TestRenameRefusesAFolderSource(t *testing.T) {
	t.Parallel()
	api := newMockAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	err := api.drive(t, nil).Rename(context.Background(), "reports/", "archive")
	if err == nil || !strings.Contains(err.Error(), "names a folder") {
		t.Fatalf("err = %v, want a folder refusal", err)
	}
	if len(api.requests) != 0 {
		t.Fatalf("requests = %+v, want none", api.requests)
	}
}

func TestDryRunSendsNothing(t *testing.T) {
	t.Parallel()
	api := newMockAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	var out bytes.Buffer
	d := api.drive(t, &out)
	ctx := context.Background()

	if err := d.Mkdir(ctx, "a/b"); err != nil {
		t.Fatalf("Mkdir() error = %v", err)
	}
	if err := d.DeleteDir(ctx, "a", false); err != nil {
		t.Fatalf("DeleteDir() error = %v", err)
	}
	if err := d.Download(ctx, "a/x.csv", &bytes.Buffer{}); err != nil {
		t.Fatalf("Download() error = %v", err)
	}
	if len(api.requests) != 0 {
		t.Fatalf("dry-run sent %d requests", len(api.requests))
	}
	got := out.String()
	for _, want := range []string{"-X PUT", "/directories/a/b/", "-X DELETE", "sent only if the listing above shows the folder is empty", "/files/a/x.csv"} {
		if !strings.Contains(got, want) {
			t.Errorf("dry-run output is missing %q:\n%s", want, got)
		}
	}
}

package commands

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"wherobots/cli/internal/auth"
	"wherobots/cli/internal/config"
	"wherobots/cli/internal/spec"
)

func TestJobsRunNoWatchPrintsSummary(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/organization":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"fileStore":{"bucketName":"managed-bucket"}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/files/upload-url":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"uploadUrl":"https://example.com/upload"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/files/dir":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"name":"root","path":"s3://managed-bucket/customer/root"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/runs":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"run-123","name":"test-job-001","status":"PENDING","createTime":"2026-01-01T00:00:00Z","payload":{"runtime":"tiny","region":"aws-us-west-2"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	root := buildJobsTestRoot(server.URL)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{
		"job-runs", "create", "s3://bucket/script.py",
		"--name", "test-job-001",
		"--runtime", "tiny",
		"--upload-path", "s3://override-bucket/custom/prefix",
	})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "run-123") || !strings.Contains(got, "test-job-001") || !strings.Contains(got, "PENDING") {
		t.Fatalf("expected summary with ID, name, and status, got %q", got)
	}
}

func TestJobsRunOmitsRegionAndRuntimeWhenUnset(t *testing.T) {
	t.Parallel()

	var hasRegionParam, runtimeInBody bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/organization":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"fileStore":{"bucketName":"managed-bucket"}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/files/upload-url":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"uploadUrl":"https://example.com/upload"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/files/dir":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"name":"root","path":"s3://managed-bucket/customer/root"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/runs":
			_, hasRegionParam = r.URL.Query()["region"]
			bodyBytes, _ := io.ReadAll(r.Body)
			var payload map[string]any
			_ = json.Unmarshal(bodyBytes, &payload)
			_, runtimeInBody = payload["runtime"]
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"run-123","name":"test-job-001","status":"PENDING","createTime":"2026-01-01T00:00:00Z","payload":{}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	root := buildJobsTestRoot(server.URL)
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{
		"job-runs", "create", "s3://bucket/script.py",
		"--name", "test-job-001",
		"--upload-path", "s3://override-bucket/custom/prefix",
	})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if hasRegionParam {
		t.Fatalf("expected no region query param when --run-region is unset")
	}
	if runtimeInBody {
		t.Fatalf("expected runtime to be omitted from the body when --runtime is unset")
	}
}

func TestJobsRunPassesArbitraryRegionAndRuntime(t *testing.T) {
	t.Parallel()

	var gotRegion, gotRuntime string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/organization":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"fileStore":{"bucketName":"managed-bucket"}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/files/upload-url":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"uploadUrl":"https://example.com/upload"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/files/dir":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"name":"root","path":"s3://managed-bucket/customer/root"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/runs":
			gotRegion = r.URL.Query().Get("region")
			bodyBytes, _ := io.ReadAll(r.Body)
			var payload map[string]any
			_ = json.Unmarshal(bodyBytes, &payload)
			if v, ok := payload["runtime"].(string); ok {
				gotRuntime = v
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"run-123","name":"test-job-001","status":"PENDING","createTime":"2026-01-01T00:00:00Z","payload":{}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	root := buildJobsTestRoot(server.URL)
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{
		"job-runs", "create", "s3://bucket/script.py",
		"--name", "test-job-001",
		"--run-region", "byoc-acme-us-east-1",
		"--runtime", "x-large",
		"--upload-path", "s3://override-bucket/custom/prefix",
	})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if gotRegion != "byoc-acme-us-east-1" {
		t.Fatalf("expected region byoc-acme-us-east-1, got %q", gotRegion)
	}
	if gotRuntime != "x-large" {
		t.Fatalf("expected runtime x-large, got %q", gotRuntime)
	}
}

func TestJobsRunWatchReturnsErrorOnFailedStatus(t *testing.T) {
	t.Parallel()

	var statusCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/organization":
			_, _ = io.WriteString(w, `{"fileStore":{"bucketName":"managed-bucket"}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/files/upload-url":
			_, _ = io.WriteString(w, `{"uploadUrl":"https://example.com/upload"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/files/dir":
			_, _ = io.WriteString(w, `{"name":"root","path":"s3://managed-bucket/customer/root"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/runs":
			_, _ = io.WriteString(w, `{"id":"run-xyz","status":"PENDING"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/runs/run-xyz/logs":
			if r.URL.Query().Get("size") == "1000" {
				_, _ = io.WriteString(w, `{"items":[{"raw":"final"}],"current_page":1,"next_page":null}`)
				return
			}
			_, _ = io.WriteString(w, `{"items":[{"raw":"line-1"}],"current_page":0,"next_page":1}`)
		case r.Method == http.MethodGet && r.URL.Path == "/runs/run-xyz":
			statusCalls++
			if statusCalls == 1 {
				_, _ = io.WriteString(w, `{"id":"run-xyz","status":"FAILED"}`)
				return
			}
			_, _ = io.WriteString(w, `{"id":"run-xyz","status":"FAILED"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	root := buildJobsTestRoot(server.URL)
	var out bytes.Buffer
	var errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs([]string{
		"job-runs", "create", "s3://bucket/script.py",
		"--name", "test-job-001",
		"--watch",
	})

	err := root.Execute()
	if err == nil {
		t.Fatalf("expected error for failed run status")
	}
	if !strings.Contains(err.Error(), "run finished with status FAILED") {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out.String(), "line-1") {
		t.Fatalf("expected streamed log line, got %q", out.String())
	}
}

func TestJobsRunAutoUploadLocalScript(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := dir + "/script.py"
	if err := os.WriteFile(script, []byte("print('ok')\n"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	var sawUpload bool
	var sawCreateRun bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/files/upload-url":
			_, _ = io.WriteString(w, fmt.Sprintf(`{"uploadUrl":%q}`, serverURLWithPath(serverURLFromRequest(r), "/upload")))
		case r.Method == http.MethodGet && r.URL.Path == "/organization":
			_, _ = io.WriteString(w, `{"fileStore":{"id":"fs-file-store","bucketName":"managed-bucket"},"storageIntegrations":[{"id":"si-managed","path":"s3://managed-bucket/customer/root"}]}`)
		case r.Method == http.MethodGet && r.URL.Path == "/files/integration-dir":
			if r.URL.Query().Get("integration_id") != "si-managed" {
				t.Fatalf("expected integration_id si-managed, got %q", r.URL.Query().Get("integration_id"))
			}
			_, _ = io.WriteString(w, `{"name":"root","path":"s3://managed-bucket/customer/root"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/files/dir":
			t.Fatalf("did not expect /files/dir fallback when integration-dir works")
		case r.Method == http.MethodPut && r.URL.Path == "/upload":
			sawUpload = true
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && r.URL.Path == "/runs":
			sawCreateRun = true
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), "s3://managed-bucket/customer/root/test-job-001/script.py") {
				t.Fatalf("expected auto-uploaded s3 URI in payload, got %s", string(body))
			}
			_, _ = io.WriteString(w, `{"id":"run-auto","status":"PENDING"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	root := buildJobsTestRoot(server.URL)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"job-runs", "create", script, "--name", "test-job-001"})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !sawUpload || !sawCreateRun {
		t.Fatalf("expected upload and create-run calls; upload=%v create=%v", sawUpload, sawCreateRun)
	}
}

func TestJobsRunAutoUploadFallsBackToFilesDirWhenIntegrationDirFails(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := dir + "/script.py"
	if err := os.WriteFile(script, []byte("print('ok')\n"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	var sawIntegrationDir bool
	var sawFilesDir bool
	var sawUpload bool
	var sawCreateRun bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/organization":
			_, _ = io.WriteString(w, `{"fileStore":{"id":"fs-file-store","bucketName":"managed-bucket"},"storageIntegrations":[{"id":"si-managed","path":"s3://managed-bucket/customer/root"}]}`)
		case r.Method == http.MethodGet && r.URL.Path == "/files/integration-dir":
			sawIntegrationDir = true
			http.Error(w, `{"error":"no integration"}`, http.StatusBadRequest)
		case r.Method == http.MethodGet && r.URL.Path == "/files/dir":
			sawFilesDir = true
			_, _ = io.WriteString(w, `{"name":"root","path":"s3://managed-bucket/customer/root"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/files/upload-url":
			_, _ = io.WriteString(w, fmt.Sprintf(`{"uploadUrl":%q}`, serverURLWithPath(serverURLFromRequest(r), "/upload")))
		case r.Method == http.MethodPut && r.URL.Path == "/upload":
			sawUpload = true
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && r.URL.Path == "/runs":
			sawCreateRun = true
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), "s3://managed-bucket/customer/root/test-job-001/script.py") {
				t.Fatalf("expected auto-uploaded s3 URI in payload, got %s", string(body))
			}
			_, _ = io.WriteString(w, `{"id":"run-auto","status":"PENDING"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	root := buildJobsTestRoot(server.URL)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"job-runs", "create", script, "--name", "test-job-001"})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !sawUpload || !sawCreateRun {
		t.Fatalf("expected upload and create-run calls; upload=%v create=%v", sawUpload, sawCreateRun)
	}
	if !sawIntegrationDir || !sawFilesDir {
		t.Fatalf("expected integration-dir then files/dir fallback; integration=%v filesDir=%v", sawIntegrationDir, sawFilesDir)
	}
}

func TestJobsRunAutoUploadHandlesBucketNameAsS3URI(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := dir + "/script.py"
	if err := os.WriteFile(script, []byte("print('ok')\n"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	var dirQueryParam string
	var sawUpload bool
	var sawCreateRun bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/organization":
			// Return bucketName as a full S3 URI (as some API responses may include the s3:// prefix)
			_, _ = io.WriteString(w, `{"fileStore":{"bucketName":"s3://managed-bucket"}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/files/dir":
			dirQueryParam = r.URL.Query().Get("dir")
			_, _ = io.WriteString(w, `{"name":"root","path":"s3://managed-bucket/customer/root"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/files/upload-url":
			_, _ = io.WriteString(w, fmt.Sprintf(`{"uploadUrl":%q}`, serverURLWithPath(serverURLFromRequest(r), "/upload")))
		case r.Method == http.MethodPut && r.URL.Path == "/upload":
			sawUpload = true
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && r.URL.Path == "/runs":
			sawCreateRun = true
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), "s3://managed-bucket/customer/root/test-job-001/script.py") {
				t.Fatalf("expected auto-uploaded s3 URI in payload, got %s", string(body))
			}
			_, _ = io.WriteString(w, `{"id":"run-auto","status":"PENDING"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	root := buildJobsTestRoot(server.URL)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"job-runs", "create", script, "--name", "test-job-001"})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !sawUpload || !sawCreateRun {
		t.Fatalf("expected upload and create-run calls; upload=%v create=%v", sawUpload, sawCreateRun)
	}
	// The dir query param passed to /files/dir should be the bare bucket name (not s3://managed-bucket/)
	if dirQueryParam != "managed-bucket" {
		t.Fatalf("expected dir param managed-bucket, got %q", dirQueryParam)
	}
}

func TestJobsRunNoUploadWithLocalScriptFails(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := dir + "/script.py"
	if err := os.WriteFile(script, []byte("print('ok')\n"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/files/dir" {
			http.NotFound(w, r)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	root := buildJobsTestRootWithConfig(server.URL, func(cfg *config.Config) {
		cfg.UploadPath = ""
	})
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"job-runs", "create", script, "--name", "test-job-001", "--no-upload"})

	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "remove --no-upload") {
		t.Fatalf("expected no-upload validation error, got %v", err)
	}
}

func TestJobsRunUsesUploadPathFlagOverride(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := dir + "/script.py"
	if err := os.WriteFile(script, []byte("print('ok')\n"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	var sawDirLookup bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/organization":
			sawDirLookup = true
			_, _ = io.WriteString(w, `{"fileStore":{"bucketName":"managed-bucket"}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/files/dir":
			sawDirLookup = true
			_, _ = io.WriteString(w, `{"name":"root","path":"s3://managed-bucket/customer/root"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/files/upload-url":
			if got := r.URL.Query().Get("key"); !strings.HasPrefix(got, "flag-bucket/flag-prefix/") {
				t.Fatalf("expected key from upload-path flag, got %q", got)
			}
			_, _ = io.WriteString(w, fmt.Sprintf(`{"uploadUrl":%q}`, serverURLWithPath(serverURLFromRequest(r), "/upload")))
		case r.Method == http.MethodPut && r.URL.Path == "/upload":
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && r.URL.Path == "/runs":
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), "s3://flag-bucket/flag-prefix/test-job-001/script.py") {
				t.Fatalf("expected run payload to use upload-path flag, got %s", string(body))
			}
			_, _ = io.WriteString(w, `{"id":"run-flag","status":"PENDING"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	root := buildJobsTestRoot(server.URL)
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"job-runs", "create", script, "--name", "test-job-001", "--upload-path", "s3://flag-bucket/flag-prefix"})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if sawDirLookup {
		t.Fatalf("expected upload-path override to skip managed directory APIs")
	}
}

func TestJobsRunUsesUploadPathEnvOverride(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := dir + "/script.py"
	if err := os.WriteFile(script, []byte("print('ok')\n"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	var sawDirLookup bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/organization":
			sawDirLookup = true
			_, _ = io.WriteString(w, `{"fileStore":{"bucketName":"managed-bucket"}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/files/dir":
			sawDirLookup = true
			_, _ = io.WriteString(w, `{"name":"root","path":"s3://managed-bucket/customer/root"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/files/upload-url":
			if got := r.URL.Query().Get("key"); !strings.HasPrefix(got, "env-bucket/env-prefix/") {
				t.Fatalf("expected key from upload-path env, got %q", got)
			}
			_, _ = io.WriteString(w, fmt.Sprintf(`{"uploadUrl":%q}`, serverURLWithPath(serverURLFromRequest(r), "/upload")))
		case r.Method == http.MethodPut && r.URL.Path == "/upload":
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && r.URL.Path == "/runs":
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), "s3://env-bucket/env-prefix/test-job-001/script.py") {
				t.Fatalf("expected run payload to use upload-path env, got %s", string(body))
			}
			_, _ = io.WriteString(w, `{"id":"run-env","status":"PENDING"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	root := buildJobsTestRootWithConfig(server.URL, func(cfg *config.Config) {
		cfg.UploadPath = "s3://env-bucket/env-prefix"
	})
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"job-runs", "create", script, "--name", "test-job-001"})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if sawDirLookup {
		t.Fatalf("expected upload-path env override to skip managed directory APIs")
	}
}

func TestJobsLogsFollowDoesNotReprintWhenNextPageNull(t *testing.T) {
	t.Parallel()

	var logCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/runs/run-777/logs":
			logCalls++
			cursor := r.URL.Query().Get("cursor")
			switch {
			case cursor == "0":
				// First poll: 2 lines, no next_page
				_, _ = io.WriteString(w, `{"items":[{"raw":"line-1"},{"raw":"line-2"}],"current_page":0,"next_page":null}`)
			case cursor == "2":
				// Second poll: 1 new line, no next_page
				_, _ = io.WriteString(w, `{"items":[{"raw":"line-3"}],"current_page":2,"next_page":null}`)
			default:
				// Any further poll: no new lines
				_, _ = io.WriteString(w, `{"items":[],"current_page":0,"next_page":null}`)
			}
		case r.Method == http.MethodGet && r.URL.Path == "/runs/run-777":
			// Terminal on first status check so we don't loop forever
			_, _ = io.WriteString(w, `{"id":"run-777","status":"COMPLETED"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	root := buildJobsTestRoot(server.URL)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"job-runs", "logs", "run-777", "--follow", "--interval", "0.01"})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	got := out.String()
	// Each line should appear exactly once
	if count := strings.Count(got, "line-1"); count != 1 {
		t.Errorf("expected line-1 once, got %d times in %q", count, got)
	}
	if count := strings.Count(got, "line-2"); count != 1 {
		t.Errorf("expected line-2 once, got %d times in %q", count, got)
	}
	if count := strings.Count(got, "line-3"); count != 1 {
		t.Errorf("expected line-3 once, got %d times in %q", count, got)
	}
}

func TestJobsLogsJsonOutput(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/runs/run-555/logs" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"items":[{"raw":"a"},{"raw":"b"}],"current_page":0,"next_page":null}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	root := buildJobsTestRoot(server.URL)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"job-runs", "logs", "run-555", "--output", "json"})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	got := strings.TrimSpace(out.String())
	if !gjsonValid(got) {
		t.Fatalf("expected JSON output, got %q", got)
	}
}

func TestJobsListDefaultsToText(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/runs" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"items":[{"id":"run-1","name":"a","status":"RUNNING","createTime":"2026-01-01T00:00:00Z","payload":{"runtime":"tiny","region":"aws-us-west-2"}}],"total":1,"next_page":null}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	root := buildJobsTestRoot(server.URL)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"job-runs", "list"})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "ID") || !strings.Contains(got, "run-1") {
		t.Fatalf("expected text table output by default, got %q", got)
	}
	if gjsonValid(strings.TrimSpace(got)) {
		t.Fatalf("expected text output by default, got JSON: %q", got)
	}
}

func TestJobsListEmitsCuratedCommandInClientHeader(t *testing.T) {
	t.Parallel()

	var gotClientHeader string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/runs" {
			gotClientHeader = r.Header.Get("X-Wherobots-Client")
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"items":[],"total":0,"next_page":null}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	root := buildJobsTestRoot(server.URL)
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"job-runs", "list"})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	// The curated `job-runs list` command reuses the shared `GET /runs`
	// operation, whose api-subtree CommandPath is `runs.<verb>`. The client
	// header must report the user-facing curated command, not the shared op.
	if !strings.HasSuffix(gotClientHeader, "cmd=job-runs.list") {
		t.Fatalf("X-Wherobots-Client = %q, want cmd=job-runs.list", gotClientHeader)
	}
}

func TestJobsCreateEmitsCuratedCommandInClientHeader(t *testing.T) {
	t.Parallel()

	var runsClientHeader string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/organization":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"fileStore":{"bucketName":"managed-bucket"}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/files/upload-url":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"uploadUrl":"https://example.com/upload"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/files/dir":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"name":"root","path":"s3://managed-bucket/customer/root"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/runs":
			runsClientHeader = r.Header.Get("X-Wherobots-Client")
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"run-123","name":"test-job-001","status":"PENDING","createTime":"2026-01-01T00:00:00Z","payload":{}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	root := buildJobsTestRoot(server.URL)
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{
		"job-runs", "create", "s3://bucket/script.py",
		"--name", "test-job-001",
		"--upload-path", "s3://override-bucket/custom/prefix",
	})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if !strings.HasSuffix(runsClientHeader, "cmd=job-runs.create") {
		t.Fatalf("X-Wherobots-Client on POST /runs = %q, want cmd=job-runs.create", runsClientHeader)
	}
}

func TestApiSubtreeEmitsDottedResourceCommandInClientHeader(t *testing.T) {
	t.Parallel()

	var gotClientHeader string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/runs" {
			gotClientHeader = r.Header.Get("X-Wherobots-Client")
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"items":[],"total":0,"next_page":null}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	root := buildJobsTestRoot(server.URL)
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	// The dynamic api subtree keeps its own operation CommandPath; invoking
	// it directly must still report the api-tree command name.
	root.SetArgs([]string{"api", "runs", "list"})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if !strings.HasSuffix(gotClientHeader, "cmd=runs.list") {
		t.Fatalf("X-Wherobots-Client = %q, want cmd=runs.list", gotClientHeader)
	}
}

func TestJobsRunningAliasFiltersStatus(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/runs" {
			if got := r.URL.Query().Get("status"); got != "RUNNING" {
				t.Fatalf("expected status RUNNING, got %q", got)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"items":[],"total":0,"next_page":null}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	root := buildJobsTestRoot(server.URL)
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"job-runs", "running"})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
}

func TestJobsMetricsTextOutput(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/runs/run-999/metrics" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{
				"series_metrics": {},
				"instant_metrics": {
					"CPU_UTILIZATION_PERCENT": {
						"display_name": "CPU Utilization",
						"metric": {"data": {"value": 45.2, "timestamp": 1710000000}, "format": "PERCENTAGE"}
					},
					"COST_USD": {
						"display_name": "Cost",
						"metric": {"data": {"value": 3.42, "timestamp": 1710000000}, "format": "CURRENCY"}
					},
					"CONSUMED_SPATIAL_UNITS": {
						"display_name": "Consumed Spatial Units",
						"metric": {"data": {"value": 150, "timestamp": 1710000000}, "format": "NUMBER"}
					}
				}
			}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	root := buildJobsTestRoot(server.URL)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"job-runs", "metrics", "run-999"})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "CPU Utilization") || !strings.Contains(got, "45.2%") {
		t.Fatalf("expected CPU Utilization with percentage, got %q", got)
	}
	if !strings.Contains(got, "Cost") || !strings.Contains(got, "$3.42") {
		t.Fatalf("expected Cost with currency, got %q", got)
	}
	if !strings.Contains(got, "Consumed Spatial Units") || !strings.Contains(got, "150") {
		t.Fatalf("expected Consumed Spatial Units as number, got %q", got)
	}
}

func TestJobsMetricsJsonOutput(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/runs/run-999/metrics" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"series_metrics":{},"instant_metrics":{"COST_USD":{"display_name":"Cost","metric":{"data":{"value":3.42,"timestamp":1710000000},"format":"CURRENCY"}}}}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	root := buildJobsTestRoot(server.URL)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"job-runs", "metrics", "run-999", "--output", "json"})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if !gjsonValid(strings.TrimSpace(out.String())) {
		t.Fatalf("expected JSON output, got %q", out.String())
	}
}

func TestJobsMetricsNullValue(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/runs/run-999/metrics" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"series_metrics":{},"instant_metrics":{"GPU_UTILIZATION_PERCENT":{"display_name":"GPU Utilization","metric":{"data":{"value":null,"timestamp":1710000000},"format":"PERCENTAGE"}}}}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	root := buildJobsTestRoot(server.URL)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"job-runs", "metrics", "run-999"})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "GPU Utilization") || !strings.Contains(got, "N/A") {
		t.Fatalf("expected N/A for null metric value, got %q", got)
	}
}

func TestJobsMetricsEmptyMetrics(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/runs/run-999/metrics" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"series_metrics":{},"instant_metrics":{}}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	root := buildJobsTestRoot(server.URL)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"job-runs", "metrics", "run-999"})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "No instant metrics available.") {
		t.Fatalf("expected empty metrics message, got %q", got)
	}
}

func TestFormatMetricValue(t *testing.T) {
	t.Parallel()

	tests := []struct {
		value    float64
		format   string
		expected string
	}{
		{45.2, "PERCENTAGE", "45.2%"},
		{3.42, "CURRENCY", "$3.42"},
		{150, "NUMBER", "150"},
		{1.5, "NUMBER", "1.5"},
		{1073741824, "BYTES", "1 GB"},
		{1536, "BYTES", "1.5 KB"},
		{0, "BYTES", "0 B"},
		{100, "BYTES", "100 B"},
	}

	for _, tt := range tests {
		got := formatMetricValue(tt.value, tt.format)
		if got != tt.expected {
			t.Errorf("formatMetricValue(%v, %q) = %q, want %q", tt.value, tt.format, got, tt.expected)
		}
	}
}

func TestJobsCreateDryRunSkipsRequest(t *testing.T) {
	t.Parallel()

	var serverHits int32
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&serverHits, 1)
	}))
	defer server.Close()

	root := buildJobsTestRoot(server.URL)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{
		"job-runs", "create", "s3://fake-bucket/x.py",
		"--name", "dryrun-test",
		"--dry-run",
	})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if hits := atomic.LoadInt32(&serverHits); hits != 0 {
		t.Fatalf("expected no HTTP requests during --dry-run, got %d", hits)
	}

	got := strings.TrimSpace(out.String())
	if !strings.HasPrefix(got, "curl -X POST '"+server.URL+"/runs") {
		t.Fatalf("expected curl POST %s/runs, got %q", server.URL, got)
	}
	if !strings.Contains(got, "dryrun-test") {
		t.Fatalf("expected curl output to include job name, got %q", got)
	}
}

func TestJobsListDryRunSkipsRequest(t *testing.T) {
	t.Parallel()

	var serverHits int32
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&serverHits, 1)
	}))
	defer server.Close()

	root := buildJobsTestRoot(server.URL)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"job-runs", "list", "--dry-run"})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if hits := atomic.LoadInt32(&serverHits); hits != 0 {
		t.Fatalf("expected no HTTP requests during --dry-run, got %d", hits)
	}

	got := strings.TrimSpace(out.String())
	if !strings.HasPrefix(got, "curl -X GET '"+server.URL+"/runs") {
		t.Fatalf("expected curl GET %s/runs, got %q", server.URL, got)
	}
}

func TestBuildRunPayloadRejectsBadDependency(t *testing.T) {
	t.Parallel()

	_, err := buildRunPayload(
		"s3://bucket/script.py",
		"test-job-001",
		"tiny",
		3600,
		"",
		nil,
		nil,
		[]string{"s3://bucket/data.txt"},
		"",
	)
	if err == nil || !strings.Contains(err.Error(), "supported extensions") {
		t.Fatalf("expected dependency validation error, got %v", err)
	}
}

func TestCompleteRegionsReturnsAllowedRegions(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/organization" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"allowedRegions":["aws-us-west-2","aws-eu-west-1","byoc-acme-us-east-1"]}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	runner := newJobsRunnerForTest(server.URL)
	cmd := &cobra.Command{}
	got, _ := runner.completeRegions(cmd, nil, "")
	want := []string{"aws-us-west-2", "aws-eu-west-1", "byoc-acme-us-east-1"}
	if !equalStrings(got, want) {
		t.Fatalf("completeRegions = %v, want %v", got, want)
	}
}

func TestCompleteRegionsFailsOpenOnError(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	runner := newJobsRunnerForTest(server.URL)
	cmd := &cobra.Command{}
	got, _ := runner.completeRegions(cmd, nil, "")
	if got != nil {
		t.Fatalf("expected nil on error (fail open), got %v", got)
	}
}

func TestCompleteRuntimesSkipsDisabled(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/me/jupyter/lab/config-hint" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"runtimes":{"hint":[{"id":"tiny","enabled":true},{"id":"small"},{"id":"x-large","enabled":false}]}}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	runner := newJobsRunnerForTest(server.URL)
	cmd := &cobra.Command{}
	got, _ := runner.completeRuntimes(cmd, nil, "")
	want := []string{"tiny", "small"}
	if !equalStrings(got, want) {
		t.Fatalf("completeRuntimes = %v, want %v", got, want)
	}
}

func newJobsRunnerForTest(baseURL string) *jobsRunner {
	cfg := config.Config{AppName: "wherobots", APIKey: "test-key", HTTPTimeout: time.Second}
	runner, _ := newJobsRunner(cfg, auth.NewResolver(cfg), jobsTestRuntimeSpec(baseURL), http.DefaultClient, nil)
	return runner
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func buildJobsTestRoot(baseURL string) *cobra.Command {
	return buildJobsTestRootWithConfig(baseURL, nil)
}

func buildJobsTestRootWithConfig(baseURL string, mutate func(*config.Config)) *cobra.Command {
	cfg := config.Config{
		AppName:     "wherobots",
		APIKey:      "test-key",
		HTTPTimeout: time.Second,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	return BuildRootCommand(cfg, auth.NewResolver(cfg), jobsTestRuntimeSpec(baseURL))
}

func jobsTestRuntimeSpec(baseURL string) *spec.RuntimeSpec {
	return &spec.RuntimeSpec{
		BaseURL: baseURL,
		Operations: []*spec.Operation{
			{
				Method: "GET",
				Path:   "/organization",
			},
			{
				Method: "GET",
				Path:   "/files/integration-dir",
				QueryParams: []spec.Parameter{
					{Name: "integration_id", Location: "query", Required: true, Type: "string"},
					{Name: "dir", Location: "query", Required: true, Type: "string"},
				},
			},
			{
				Method: "GET",
				Path:   "/files/dir",
				QueryParams: []spec.Parameter{
					{Name: "dir", Location: "query", Required: true, Type: "string"},
				},
			},
			{
				Method: "POST",
				Path:   "/files/upload-url",
				QueryParams: []spec.Parameter{
					{Name: "key", Location: "query", Required: true, Type: "string"},
				},
			},
			{
				Method: "POST",
				Path:   "/runs",
				QueryParams: []spec.Parameter{
					// Optional: omitting it makes the API use the org default region.
					{Name: "region", Location: "query", Required: false, Type: "string"},
				},
				RequestBody: &spec.RequestBodyInfo{Required: true, ContentType: "application/json", SchemaType: "object"},
			},
			{
				Method:         "GET",
				Path:           "/runs/{run_id}",
				PathParamOrder: []string{"run_id"},
				PathParams:     []spec.Parameter{{Name: "run_id", Location: "path", Required: true, Type: "string"}},
			},
			{
				Method:         "GET",
				Path:           "/runs/{run_id}/logs",
				PathParamOrder: []string{"run_id"},
				PathParams:     []spec.Parameter{{Name: "run_id", Location: "path", Required: true, Type: "string"}},
			},
			{
				Method:         "GET",
				Path:           "/runs/{run_id}/metrics",
				PathParamOrder: []string{"run_id"},
				PathParams:     []spec.Parameter{{Name: "run_id", Location: "path", Required: true, Type: "string"}},
			},
			{
				Method: "GET",
				Path:   "/runs",
			},
			{
				Method: "GET",
				Path:   "/me/jupyter/lab/config-hint",
			},
		},
	}
}

func serverURLFromRequest(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func serverURLWithPath(base, path string) string {
	return strings.TrimRight(base, "/") + path
}

func gjsonValid(raw string) bool {
	var payload any
	return json.Unmarshal([]byte(raw), &payload) == nil
}

func TestJobsRunUploadURLStorageSourceErrorIsActionable(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := dir + "/script.py"
	if err := os.WriteFile(script, []byte("print('ok')\n"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	envelope := `{"errors":[{"code":"BAD_REQUEST_ERROR","message":"Bad Request","details":"InvalidInputException (No storage source found for bucket: flag-prefix)","path":"/files/upload-url","suggestion":"Update your request and try again."}],"requestId":"req-bug-2046"}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && r.URL.Path == "/files/upload-url" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, envelope)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	root := buildJobsTestRoot(server.URL)
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"job-runs", "create", script, "--name", "test-job-001", "--upload-path", "s3://flag-bucket/flag-prefix"})

	err := root.Execute()
	if err == nil {
		t.Fatal("expected error from 400 upload-url response")
	}
	got := err.Error()
	for _, want := range []string{
		"requesting upload URL for s3://flag-bucket/flag-prefix/test-job-001/script.py",
		"No storage source found for bucket: flag-prefix",
		"Update your request and try again.",
		"req-bug-2046",
		"is not a registered storage source",
		"omit --upload-path",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected error to contain %q, got:\n%s", want, got)
		}
	}
}

func TestJobsRunUploadURLStorageSourceErrorEnvOverrideIsActionable(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := dir + "/script.py"
	if err := os.WriteFile(script, []byte("print('ok')\n"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	envelope := `{"errors":[{"code":"BAD_REQUEST_ERROR","message":"Bad Request","details":"InvalidInputException (No storage source found for bucket: env-prefix)","path":"/files/upload-url","suggestion":"Update your request and try again."}],"requestId":"req-bug-2046-env"}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && r.URL.Path == "/files/upload-url" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, envelope)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	root := buildJobsTestRootWithConfig(server.URL, func(cfg *config.Config) {
		cfg.UploadPath = "s3://env-bucket/env-prefix"
	})
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"job-runs", "create", script, "--name", "test-job-001"})

	err := root.Execute()
	if err == nil {
		t.Fatal("expected error from 400 upload-url response")
	}
	got := err.Error()
	for _, want := range []string{
		"requesting upload URL for s3://env-bucket/env-prefix/test-job-001/script.py",
		"No storage source found for bucket: env-prefix",
		"Update your request and try again.",
		"req-bug-2046-env",
		"is not a registered storage source",
		"unset WHEROBOTS_UPLOAD_PATH",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected error to contain %q, got:\n%s", want, got)
		}
	}
	// The override came from the env var, so the misleading flag guidance must
	// not appear.
	if strings.Contains(got, "omit --upload-path") {
		t.Fatalf("did not expect --upload-path guidance for env-var override, got:\n%s", got)
	}
}

func TestJobsRunUploadURLNonEnvelopeErrorKeepsContextWithoutGuidance(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := dir + "/script.py"
	if err := os.WriteFile(script, []byte("print('ok')\n"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/files/upload-url" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, "upstream rejected request")
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	root := buildJobsTestRoot(server.URL)
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"job-runs", "create", script, "--name", "test-job-001", "--upload-path", "s3://flag-bucket/flag-prefix"})

	err := root.Execute()
	if err == nil {
		t.Fatal("expected error from 400 upload-url response")
	}
	got := err.Error()
	if !strings.Contains(got, "requesting upload URL for s3://flag-bucket/flag-prefix/test-job-001/script.py") {
		t.Fatalf("expected upload context prefix, got:\n%s", got)
	}
	if !strings.Contains(got, "upstream rejected request") {
		t.Fatalf("expected raw body fallback, got:\n%s", got)
	}
	if strings.Contains(got, "omit --upload-path") {
		t.Fatalf("did not expect storage-source guidance for non-envelope error, got:\n%s", got)
	}
}

func TestJobsRunUploadURLStorageSourceErrorWithoutOverrideOmitsGuidance(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := dir + "/script.py"
	if err := os.WriteFile(script, []byte("print('ok')\n"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	envelope := `{"errors":[{"code":"BAD_REQUEST_ERROR","message":"Bad Request","details":"InvalidInputException (No storage source found for bucket: customer)","path":"/files/upload-url","suggestion":"Update your request and try again."}],"requestId":"req-managed"}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/organization":
			_, _ = io.WriteString(w, `{"fileStore":{"id":"fs-file-store","bucketName":"managed-bucket"},"storageIntegrations":[{"id":"si-managed","path":"s3://managed-bucket/customer/root"}]}`)
		case r.Method == http.MethodGet && r.URL.Path == "/files/integration-dir":
			_, _ = io.WriteString(w, `{"name":"root","path":"s3://managed-bucket/customer/root"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/files/upload-url":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, envelope)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	root := buildJobsTestRoot(server.URL)
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"job-runs", "create", script, "--name", "test-job-001"})

	err := root.Execute()
	if err == nil {
		t.Fatal("expected error from 400 upload-url response")
	}
	got := err.Error()
	if !strings.Contains(got, "requesting upload URL for s3://managed-bucket/customer/root/test-job-001/script.py") {
		t.Fatalf("expected upload context prefix, got:\n%s", got)
	}
	if strings.Contains(got, "omit --upload-path") {
		t.Fatalf("did not expect override guidance when no --upload-path was given, got:\n%s", got)
	}
}

func TestJobsArgumentErrors(t *testing.T) {
	t.Parallel()
	for _, command := range []struct {
		name     string
		argument string
		example  string
	}{
		{"create", "script", "s3://my-bucket/job.py"},
		{"logs", "run-id", "run-123"},
		{"metrics", "run-id", "run-123"},
	} {
		for _, input := range []struct {
			name    string
			args    []string
			message string
		}{
			{"missing", nil, "missing required argument <" + command.argument + ">"},
			{"blank", []string{"  "}, "missing required argument <" + command.argument + ">"},
			{"extra", []string{"first", "second"}, "expected one <" + command.argument + "> argument, received 2"},
		} {
			t.Run(command.name+"/"+input.name, func(t *testing.T) {
				root := buildJobsTestRoot("http://127.0.0.1:1")
				root.SetOut(io.Discard)
				root.SetErr(io.Discard)
				root.SetArgs(append([]string{"job-runs", command.name}, input.args...))
				err := root.Execute()
				if err == nil {
					t.Fatal("expected argument error")
				}
				path := "wherobots job-runs " + command.name
				for _, want := range []string{
					input.message,
					"Usage: " + path + " <" + command.argument + "> [flags]",
					"Example: " + path + " " + command.example,
					"Run '" + path + " --help' for options.",
				} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error = %q, want %q", err, want)
					}
				}
			})
		}
	}
}

package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestUploadFileToPresignedURL(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Fatalf("expected PUT, got %s", r.Method)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "script.py")
	if err := os.WriteFile(path, []byte("print('hi')\n"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	client := &http.Client{Timeout: time.Second}
	if err := UploadFileToPresignedURL(context.Background(), client, server.URL, path); err != nil {
		t.Fatalf("UploadFileToPresignedURL() error = %v", err)
	}
}

func TestUploadFileToPresignedURLServerError(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("<Error><Code>InvalidRequest</Code><Message>bad upload to org-42/user-7</Message></Error>"))
	}))
	defer server.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "script.py")
	if err := os.WriteFile(path, []byte("print('hi')\n"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	client := &http.Client{Timeout: time.Second}
	err := UploadFileToPresignedURL(context.Background(), client, server.URL, path)
	if err == nil || err.Error() != "upload failed with HTTP 400 (InvalidRequest)" {
		t.Fatalf("expected status and code only, got %v", err)
	}
	if strings.Contains(err.Error(), "org-42") {
		t.Fatalf("error must not include the storage body's message: %v", err)
	}
}

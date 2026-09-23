//go:build unix

package commands

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A real SIGINT during a download cancels only the transfer, which then
// removes its .partial file. Not parallel: it signals the test process.
func TestFilesDownloadInterruptLeavesNoFile(t *testing.T) {
	started := make(chan struct{})
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("first bytes"))
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	}))
	defer storage.Close()
	m := newFilesMock(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, storage.URL+"/get", http.StatusTemporaryRedirect)
	})

	go func() {
		<-started
		_ = syscall.Kill(os.Getpid(), syscall.SIGINT)
	}()
	// The deadline only stops a hang; the signal must cancel the transfer first.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	dest := filepath.Join(t.TempDir(), "slow.csv")
	_, err := m.runContext(t, ctx, "files", "my-files", "download", "slow.csv", dest)
	if err == nil || ctx.Err() != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("want the signal to cancel the download, got err=%v ctx=%v", err, ctx.Err())
	}
	entries, _ := os.ReadDir(filepath.Dir(dest))
	if len(entries) != 0 {
		t.Fatalf("files left behind after an interrupted download: %v", entries)
	}
}

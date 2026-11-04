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

	"wherobots/cli/internal/auth"
	"wherobots/cli/internal/config"
	"wherobots/cli/internal/spec"
)

const refusedRedirectMessage = "redirect not followed"

type seenRequest struct {
	Method string
	Body   string
}

// redirectStub answers every request with a redirect to /target, which
// returns 200 with JSON, and records what reaches each side.
type redirectStub struct {
	server *httptest.Server

	mu     sync.Mutex
	origin []seenRequest
	target []seenRequest
}

func newRedirectStub(t *testing.T, status int, redirectBody string) *redirectStub {
	t.Helper()
	s := &redirectStub{}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seen := seenRequest{Method: r.Method, Body: string(body)}

		s.mu.Lock()
		defer s.mu.Unlock()
		if r.URL.Path == "/target" {
			s.target = append(s.target, seen)
			_, _ = io.WriteString(w, `{"ok":true}`)
			return
		}
		s.origin = append(s.origin, seen)
		w.Header().Set("Location", "/target")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, redirectBody)
	}))
	t.Cleanup(s.server.Close)
	return s
}

func (s *redirectStub) run(args ...string) (string, error) {
	cfg := config.Config{AppName: "wherobots", APIKey: "test-key", HTTPTimeout: time.Second}
	runtimeSpec := &spec.RuntimeSpec{
		BaseURL: s.server.URL,
		Operations: []*spec.Operation{
			{Method: "GET", Path: "/things"},
			{
				Method: "POST",
				Path:   "/things",
				RequestBody: &spec.RequestBodyInfo{
					Required:   true,
					SchemaType: "object",
					Fields:     []spec.BodyField{{Name: "name", Type: "string", Required: true}},
				},
			},
			{
				Method:         "DELETE",
				Path:           "/things/{id}",
				PathParams:     []spec.Parameter{{Name: "id", Location: "path", Required: true, Type: "string"}},
				PathParamOrder: []string{"id"},
			},
		},
	}

	root := BuildRootCommand(cfg, auth.NewResolver(cfg), runtimeSpec)
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs(args)
	err := root.Execute()
	return stdout.String(), err
}

var apiWrites = []struct {
	name string
	args []string
	want seenRequest
}{
	{
		name: "POST",
		args: []string{"api", "things", "create", "--name", "n1", "--yes"},
		want: seenRequest{Method: http.MethodPost, Body: `{"name":"n1"}`},
	},
	{
		name: "DELETE",
		args: []string{"api", "things", "delete", "--id", "t-1", "--yes"},
		want: seenRequest{Method: http.MethodDelete},
	},
}

func TestAPIWriteRefusesRedirectThatDropsMethod(t *testing.T) {
	t.Parallel()

	for _, write := range apiWrites {
		for _, status := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther} {
			t.Run(fmt.Sprintf("%s %d", write.name, status), func(t *testing.T) {
				t.Parallel()

				stub := newRedirectStub(t, status, "")
				stdout, err := stub.run(write.args...)

				if err == nil {
					t.Fatalf("Execute() succeeded with stdout %q; target saw %+v", stdout, stub.target)
				}
				if !strings.Contains(err.Error(), fmt.Sprintf("HTTP %d", status)) || !strings.Contains(err.Error(), refusedRedirectMessage) {
					t.Fatalf("Execute() error = %q, want the status and %q", err, refusedRedirectMessage)
				}
				if len(stub.target) != 0 {
					t.Fatalf("redirect target was hit: %+v", stub.target)
				}
				if len(stub.origin) != 1 || stub.origin[0] != write.want {
					t.Fatalf("origin saw %+v, want one %+v", stub.origin, write.want)
				}
				if stdout != "" {
					t.Fatalf("stdout = %q, want empty", stdout)
				}
			})
		}
	}
}

func TestAPIWriteRefusedRedirectIgnoresErrorEnvelope(t *testing.T) {
	t.Parallel()

	stub := newRedirectStub(t, http.StatusFound, `{"errors":[{"code":"MOVED","message":"moved"}]}`)
	_, err := stub.run(apiWrites[0].args...)

	if err == nil || !strings.Contains(err.Error(), refusedRedirectMessage) {
		t.Fatalf("Execute() error = %v, want %q", err, refusedRedirectMessage)
	}
}

func TestAPIWriteRedirectStatusWithoutLocationKeepsErrorEnvelope(t *testing.T) {
	t.Parallel()

	envelope := `{"errors":[{"code":"MOVED","message":"moved"}]}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusFound)
		_, _ = io.WriteString(w, envelope)
	}))
	defer server.Close()

	stub := &redirectStub{server: server}
	_, err := stub.run(apiWrites[0].args...)

	if err == nil || err.Error() != envelope {
		t.Fatalf("Execute() error = %v, want the raw envelope", err)
	}
}

func TestAPIWriteFollowsRedirectThatKeepsMethod(t *testing.T) {
	t.Parallel()

	for _, write := range apiWrites {
		for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
			t.Run(fmt.Sprintf("%s %d", write.name, status), func(t *testing.T) {
				t.Parallel()

				stub := newRedirectStub(t, status, "")
				stdout, err := stub.run(write.args...)

				if err != nil {
					t.Fatalf("Execute() error = %v", err)
				}
				if len(stub.target) != 1 || stub.target[0] != write.want {
					t.Fatalf("target saw %+v, want one %+v", stub.target, write.want)
				}
				if !strings.Contains(stdout, `"ok"`) {
					t.Fatalf("stdout = %q, want the target's response", stdout)
				}
			})
		}
	}
}

func TestAPIReadFollowsMovedPermanently(t *testing.T) {
	t.Parallel()

	stub := newRedirectStub(t, http.StatusMovedPermanently, "")
	stdout, err := stub.run("api", "things", "list")

	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if want := (seenRequest{Method: http.MethodGet}); len(stub.target) != 1 || stub.target[0] != want {
		t.Fatalf("target saw %+v, want one %+v", stub.target, want)
	}
	if !strings.Contains(stdout, `"ok"`) {
		t.Fatalf("stdout = %q, want the target's response", stdout)
	}
}

func TestJobsCreateRefusedRedirectIsNotRetried(t *testing.T) {
	t.Parallel()

	stub := newRedirectStub(t, http.StatusFound, "")
	root := buildJobsTestRoot(stub.server.URL)
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"job-runs", "create", "s3://bucket/script.py", "--name", "test-job-001"})

	err := root.Execute()

	if err == nil || !strings.Contains(err.Error(), refusedRedirectMessage) {
		t.Fatalf("Execute() error = %v, want %q; target saw %+v", err, refusedRedirectMessage, stub.target)
	}
	var posts int
	for _, r := range stub.origin {
		if r.Method == http.MethodPost {
			posts++
		}
	}
	if posts != 1 {
		t.Fatalf("origin saw %d POSTs, want 1: %+v", posts, stub.origin)
	}
}

func TestFilesUploadToStorageRedirects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		status     int
		wantErr    string
		wantTarget []seenRequest
	}{
		{name: "301 fails", status: http.StatusMovedPermanently, wantErr: refusedRedirectMessage},
		{name: "302 fails", status: http.StatusFound, wantErr: refusedRedirectMessage},
		{name: "303 fails", status: http.StatusSeeOther, wantErr: refusedRedirectMessage},
		{
			name:       "307 keeps the PUT and its body",
			status:     http.StatusTemporaryRedirect,
			wantTarget: []seenRequest{{Method: http.MethodPut, Body: "a,b\n"}},
		},
		{
			name:       "308 keeps the PUT and its body",
			status:     http.StatusPermanentRedirect,
			wantTarget: []seenRequest{{Method: http.MethodPut, Body: "a,b\n"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			storage := newRedirectStub(t, tt.status, "")
			m := newFilesMock(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = fmt.Fprintf(w, `{"uploadUrl":%q}`, storage.server.URL+"/put?sig=1")
			})
			local := filepath.Join(t.TempDir(), "q3.csv")
			if err := os.WriteFile(local, []byte("a,b\n"), 0o600); err != nil {
				t.Fatal(err)
			}

			_, err := m.run(t, "files", "my-files", "upload", local, "q3.csv")

			if tt.wantErr == "" && err != nil {
				t.Fatalf("Execute() error = %v", err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("Execute() error = %v, want %q; target saw %+v", err, tt.wantErr, storage.target)
			}
			if err != nil && strings.Contains(err.Error(), "/target") {
				t.Fatalf("error names the redirect location: %v", err)
			}
			if len(storage.target) != len(tt.wantTarget) || (len(tt.wantTarget) == 1 && storage.target[0] != tt.wantTarget[0]) {
				t.Fatalf("target saw %+v, want %+v", storage.target, tt.wantTarget)
			}
		})
	}
}

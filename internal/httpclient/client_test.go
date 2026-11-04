package httpclient

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestReadsFollowRedirects(t *testing.T) {
	t.Parallel()

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		t.Run(method, func(t *testing.T) {
			t.Parallel()

			var gotMethod string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/target" {
					gotMethod = r.Method
					return
				}
				http.Redirect(w, r, "/target", http.StatusMovedPermanently)
			}))
			defer server.Close()

			req, err := http.NewRequest(method, server.URL+"/start", nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := New(time.Second).Do(req)
			if err != nil {
				t.Fatalf("Do() error = %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK || gotMethod != method {
				t.Fatalf("status = %d, target saw %q", resp.StatusCode, gotMethod)
			}
		})
	}
}

func TestChainedRedirectStopsWhereMethodWouldChange(t *testing.T) {
	t.Parallel()

	var hits []string
	mux := http.NewServeMux()
	mux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, r.Method+" /start")
		http.Redirect(w, r, "/next", http.StatusTemporaryRedirect)
	})
	mux.HandleFunc("/next", func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, r.Method+" /next")
		http.Redirect(w, r, "/target", http.StatusFound)
	})
	mux.HandleFunc("/target", func(_ http.ResponseWriter, r *http.Request) {
		hits = append(hits, r.Method+" /target")
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	_, err := New(time.Second).Post(server.URL+"/start", "text/plain", strings.NewReader("body"))

	var refused *RefusedRedirectError
	if !errors.As(err, &refused) || refused.StatusCode != http.StatusFound {
		t.Fatalf("Post() error = %v, want a refused 302", err)
	}
	if got := strings.Join(hits, ", "); got != "POST /start, POST /next" {
		t.Fatalf("server saw %q", got)
	}
}

func TestRedirectLoopStopsAtLimit(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, r.URL.Path, http.StatusMovedPermanently)
	}))
	defer server.Close()

	resp, err := New(5 * time.Second).Get(server.URL + "/loop")
	if err == nil {
		resp.Body.Close()
		t.Fatalf("Get() status = %d, want an error", resp.StatusCode)
	}
	if !strings.Contains(err.Error(), "stopped after 10 redirects") {
		t.Fatalf("Get() error = %v", err)
	}
}

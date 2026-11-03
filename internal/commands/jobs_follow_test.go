package commands

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"wherobots/cli/internal/api"
)

const fakeRunID = "run-777"

// fakeRunServer pages logs the way the API does: the cursor is an inclusive
// nanosecond timestamp, a page is capped at pageCap, and next_page is the
// timestamp of the first line that did not fit, or null when all of them did.
type fakeRunServer struct {
	*httptest.Server

	mu      sync.Mutex
	lines   []api.LogItem
	pageCap int
	// onStatus answers each status check and may append lines.
	onStatus func(check int) api.RunStatus
	checks   int
}

func newFakeRunServer(t *testing.T, onStatus func(check int) api.RunStatus) *fakeRunServer {
	t.Helper()
	f := &fakeRunServer{pageCap: 500, onStatus: onStatus}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		var response any
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/runs":
			w.WriteHeader(http.StatusCreated)
			response = api.RunView{ID: fakeRunID, Status: api.RunStatusPENDING}
		case r.URL.Path == "/runs/"+fakeRunID+"/logs":
			response = f.logs(t, r)
		case r.URL.Path == "/runs/"+fakeRunID:
			f.checks++
			response = api.RunView{ID: fakeRunID, Status: f.onStatus(f.checks)}
		default:
			http.NotFound(w, r)
			return
		}
		if err := json.NewEncoder(w).Encode(response); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

// add inserts one log line in timestamp order; onStatus can use it because the
// mutex is held.
func (f *fakeRunServer) add(timestamp int, raw string) {
	f.lines = append(f.lines, api.LogItem{Timestamp: stamp(timestamp), Raw: raw})
	slices.SortStableFunc(f.lines, func(a, b api.LogItem) int { return cmp.Compare(a.Timestamp, b.Timestamp) })
}

func stamp(timestamp int) string {
	return strconv.FormatInt(1_700_000_000_000_000_000+int64(timestamp), 10)
}

func (f *fakeRunServer) logs(t *testing.T, r *http.Request) api.LogsResponse {
	t.Helper()
	cursor, err := strconv.ParseInt(r.URL.Query().Get("cursor"), 10, 64)
	if err != nil {
		t.Errorf("cursor must be a nanosecond timestamp: %v", err)
	}
	size, err := strconv.Atoi(r.URL.Query().Get("size"))
	if err != nil {
		t.Errorf("size: %v", err)
	}
	size = min(size, f.pageCap)

	items := []api.LogItem{}
	for _, item := range f.lines {
		if timestamp, _ := strconv.ParseInt(item.Timestamp, 10, 64); timestamp >= cursor {
			items = append(items, item)
		}
	}
	page := api.LogsResponse{CurrentPage: strconv.FormatInt(cursor, 10), Items: items}
	if len(items) > size {
		page.Items = items[:size]
		page.NextPage = &items[size].Timestamp
	}
	return page
}

// run executes the command with a deadline, so a follow that never ends fails
// the test instead of hanging it.
func (f *fakeRunServer) run(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	root := buildJobsTestRoot(f.URL)
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs(args)
	err = root.ExecuteContext(ctx)
	return out.String(), errOut.String(), err
}

func followArgs() []string {
	return []string{"job-runs", "logs", fakeRunID, "--follow", "--interval", "0.01"}
}

func assertLines(t *testing.T, got string, want ...string) {
	t.Helper()
	if lines := strings.Fields(got); !slices.Equal(lines, want) {
		t.Fatalf("printed %d lines %v, want %d lines %v", len(lines), lines, len(want), want)
	}
}

func TestJobsLogsFollowEndsOnTimedOutRun(t *testing.T) {
	t.Parallel()

	server := newFakeRunServer(t, func(int) api.RunStatus { return api.RunStatusTIMEDOUT })
	server.add(1, "line-1")

	stdout, _, err := server.run(t, followArgs()...)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	assertLines(t, stdout, "line-1")
}

func TestJobsCreateWatchFailsOnTimedOutRun(t *testing.T) {
	t.Parallel()

	server := newFakeRunServer(t, func(int) api.RunStatus { return api.RunStatusTIMEDOUT })

	_, _, err := server.run(t, "job-runs", "create", "s3://bucket/job.py", "--name", "test-job-001", "--watch")
	if err == nil || !strings.Contains(err.Error(), "TIMED_OUT") {
		t.Fatalf("expected a TIMED_OUT failure, got %v", err)
	}
}

func TestJobsCreateWatchSucceedsOnCompletedRun(t *testing.T) {
	t.Parallel()

	server := newFakeRunServer(t, func(int) api.RunStatus { return api.RunStatusCOMPLETED })

	if _, _, err := server.run(t, "job-runs", "create", "s3://bucket/job.py", "--name", "test-job-001", "--watch"); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
}

func TestJobsLogsFollowStopsOnUnknownStatus(t *testing.T) {
	t.Parallel()

	server := newFakeRunServer(t, func(int) api.RunStatus { return "BRAND_NEW" })
	server.add(1, "line-1")

	stdout, stderr, err := server.run(t, followArgs()...)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	assertLines(t, stdout, "line-1")
	if !strings.Contains(stderr, "BRAND_NEW") {
		t.Fatalf("expected a warning naming the status, got %q", stderr)
	}
}

func TestJobsCreateWatchFailsOnUnknownStatus(t *testing.T) {
	t.Parallel()

	server := newFakeRunServer(t, func(int) api.RunStatus { return "BRAND_NEW" })

	_, stderr, err := server.run(t, "job-runs", "create", "s3://bucket/job.py", "--name", "test-job-001", "--watch")
	if err == nil || !strings.Contains(err.Error(), "BRAND_NEW") {
		t.Fatalf("expected a failure naming the status, got %v", err)
	}
	if strings.Contains(stderr, "Run finished") {
		t.Fatalf("an unknown status may still be running, stderr = %q", stderr)
	}
}

func TestJobsLogsFollowDoesNotRepeatTailPages(t *testing.T) {
	t.Parallel()

	var server *fakeRunServer
	server = newFakeRunServer(t, func(check int) api.RunStatus {
		switch check {
		case 1:
			server.add(3, "line-3")
			return api.RunStatusRUNNING
		case 2:
			return api.RunStatusRUNNING
		}
		server.add(4, "line-4")
		return api.RunStatusCOMPLETED
	})
	server.add(1, "line-1")
	server.add(2, "line-2")

	stdout, _, err := server.run(t, followArgs()...)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	assertLines(t, stdout, "line-1", "line-2", "line-3", "line-4")
}

func TestJobsLogsFollowReadsEveryPageAfterTheRunEnds(t *testing.T) {
	t.Parallel()

	want := make([]string, 0, 1250)
	for i := range 1250 {
		want = append(want, fmt.Sprintf("line-%d", i))
	}
	var server *fakeRunServer
	server = newFakeRunServer(t, func(int) api.RunStatus {
		for i, line := range want {
			server.add(i, line)
		}
		return api.RunStatusCOMPLETED
	})

	stdout, _, err := server.run(t, followArgs()...)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	assertLines(t, stdout, want...)
}

func TestJobsLogsFollowPrintsALineThatArrivesLate(t *testing.T) {
	t.Parallel()

	var server *fakeRunServer
	server = newFakeRunServer(t, func(check int) api.RunStatus {
		if check == 1 {
			server.add(2, "late")
			return api.RunStatusRUNNING
		}
		return api.RunStatusCOMPLETED
	})
	server.add(1, "first")
	server.add(3, "third")

	stdout, _, err := server.run(t, followArgs()...)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	assertLines(t, stdout, "first", "third", "late")
}

func TestJobsLogsFollowKeepsLinesSharingATimestampAcrossPages(t *testing.T) {
	t.Parallel()

	server := newFakeRunServer(t, func(int) api.RunStatus { return api.RunStatusCOMPLETED })
	server.pageCap = 3
	server.add(1, "a")
	server.add(2, "b")
	server.add(2, "c")
	server.add(2, "d")
	server.add(3, "e")

	stdout, _, err := server.run(t, followArgs()...)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	assertLines(t, stdout, "a", "b", "c", "d", "e")
}

func TestJobsLogsFollowMovesPastAPageOfOneTimestamp(t *testing.T) {
	t.Parallel()

	server := newFakeRunServer(t, func(int) api.RunStatus { return api.RunStatusCOMPLETED })
	server.pageCap = 2
	server.add(1, "a")
	server.add(1, "b")
	server.add(1, "dropped")
	server.add(2, "c")

	stdout, stderr, err := server.run(t, followArgs()...)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	assertLines(t, stdout, "a", "b", "c")
	if !strings.Contains(stderr, stamp(1)) {
		t.Fatalf("expected a warning naming the timestamp, got %q", stderr)
	}
}

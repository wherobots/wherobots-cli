package commands

import (
	"io"
	"slices"
	"strconv"
	"testing"
	"time"

	"wherobots/cli/internal/api"
)

func logPage(next string, items ...api.LogItem) api.LogsResponse {
	page := api.LogsResponse{Items: items}
	if next != "" {
		page.NextPage = &next
	}
	return page
}

func logItem(timestamp int, raw string) api.LogItem {
	return api.LogItem{Timestamp: stamp(timestamp), Raw: raw}
}

func TestLogStreamPrintsOnlyTheNewCopiesOfARepeatedLine(t *testing.T) {
	t.Parallel()

	stream := newLogStream(io.Discard)
	first, more, err := stream.next(logPage("", logItem(5, "same"), logItem(5, "same")))
	if err != nil || !slices.Equal(first, []string{"same", "same"}) || more {
		t.Fatalf("first page = %v, more = %v, err = %v", first, more, err)
	}

	if lines, _, _ := stream.next(logPage("", logItem(5, "same"), logItem(5, "same"))); len(lines) != 0 {
		t.Fatalf("repeated tail printed %v", lines)
	}
	lines, _, _ := stream.next(logPage("", logItem(5, "same"), logItem(5, "same"), logItem(5, "same")))
	if !slices.Equal(lines, []string{"same"}) {
		t.Fatalf("expected the one new line, got %v", lines)
	}
}

func TestLogStreamRestartsBehindTheNewestLine(t *testing.T) {
	t.Parallel()

	stream := newLogStream(io.Discard)
	if _, _, err := stream.next(logPage("", logItem(5, "a"))); err != nil {
		t.Fatal(err)
	}
	newest, err := strconv.ParseInt(stamp(5), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	if want := newest - int64(5*time.Second); stream.cursor != want {
		t.Fatalf("cursor = %d, want %d", stream.cursor, want)
	}

	cursor := stream.cursor
	if _, _, err := stream.next(logPage("")); err != nil {
		t.Fatal(err)
	}
	if stream.cursor != cursor {
		t.Fatalf("an empty page moved the cursor to %d", stream.cursor)
	}
}

func TestLogStreamDoesNotPrintBlankLines(t *testing.T) {
	t.Parallel()

	stream := newLogStream(io.Discard)
	lines, _, _ := stream.next(logPage("", logItem(5, ""), logItem(6, "a")))
	if !slices.Equal(lines, []string{"a"}) {
		t.Fatalf("lines = %v", lines)
	}
}

func TestLogStreamRejectsATimestampThatIsNotANumber(t *testing.T) {
	t.Parallel()

	stream := newLogStream(io.Discard)
	if _, _, err := stream.next(logPage("", api.LogItem{Timestamp: "soon", Raw: "a"})); err == nil {
		t.Fatal("expected an error")
	}
}

package commands

import (
	"fmt"
	"io"
	"maps"
	"strconv"
	"time"

	"wherobots/cli/internal/api"
)

// logGrace is how far behind the newest line a poll restarts. Log streams are
// ingested independently, so a line older than one already read can still land.
const logGrace = 5 * time.Second

type logLine struct {
	timestamp int64
	raw       string
}

// logStream turns log pages into lines, each printed once. The API's cursor is
// an inclusive timestamp and next_page is null on any page that reaches the
// current end of the logs, so polls restart behind that end and read lines
// again that are already printed.
type logStream struct {
	cursor int64
	// printed counts the lines printed within logGrace of the newest one.
	printed map[logLine]int
	warn    io.Writer
}

func newLogStream(warn io.Writer) *logStream {
	return &logStream{printed: map[logLine]int{}, warn: warn}
}

// next returns the page's unseen lines and moves the cursor. more reports
// whether another page can be read right away.
func (s *logStream) next(page api.LogsResponse) (lines []string, more bool, err error) {
	var newest int64
	fresh := 0
	read := map[logLine]int{}
	for _, item := range page.Items {
		timestamp, err := strconv.ParseInt(item.Timestamp, 10, 64)
		if err != nil {
			return nil, false, fmt.Errorf("parse log timestamp: %w", err)
		}
		newest = max(newest, timestamp)
		line := logLine{timestamp, item.Raw}
		read[line]++
		if read[line] <= s.printed[line] {
			continue
		}
		s.printed[line]++
		fresh++
		if item.Raw != "" {
			lines = append(lines, item.Raw)
		}
	}

	floor := newest - int64(logGrace)
	maps.DeleteFunc(s.printed, func(line logLine, _ int) bool { return line.timestamp < floor })

	if page.NextPage == nil {
		if len(page.Items) > 0 {
			s.cursor = floor
		}
		return lines, false, nil
	}
	next, err := strconv.ParseInt(*page.NextPage, 10, 64)
	if err != nil {
		return nil, false, fmt.Errorf("parse log cursor: %w", err)
	}
	if next == s.cursor && fresh == 0 {
		// A full page shares one timestamp, so the cursor cannot reach the rest.
		_, _ = fmt.Fprintf(s.warn, "Skipped log lines: more share timestamp %d than fit in one page.\n", next)
		next++
	}
	s.cursor = next
	return lines, true, nil
}

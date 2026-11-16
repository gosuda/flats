package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"
	"unicode/utf8"

	"github.com/gosuda/flats/internal/store"
)

// `flats audit export` (docs/spec/event-webhook.md, rules 22 to 25): the audit log and the approval records as JSON
// lines, oldest first.

// exportLag is how far behind the clock an export stops (rule 25). The host takes the time of a record before it
// writes it, and a write waits at most 5 seconds for the lock, so a record that is this old has been committed.
const exportLag = 30 * time.Second

// Limits of the text of an approval line (rule 23), in bytes.
const (
	exportTextMax      = 1024 // reason and result
	exportDecidedByMax = 512
)

// exportNow is the clock of the export. A test replaces it.
var exportNow = time.Now

// exportStderr gets the summary line of the export. The hook of package cli hands a command one writer, its standard
// output, so the command writes to the standard error itself. A test replaces it.
var exportStderr io.Writer = os.Stderr

// export runs `flats audit export`.
func (c *auditCmd) export(args []string) error {
	// The cutoff is taken first, so that nothing the command does later moves it. It is in whole milliseconds, as the
	// times of the rows are.
	now := exportNow()
	rng := store.ExportRange{Cutoff: time.UnixMilli(now.Add(-exportLag).UnixMilli())}
	f := c.flags("audit export")
	since := f.String("since", "", "only lines from this RFC 3339 time, or from this long ago (a duration such as 24h)")
	if err := c.parseFlags(args); err != nil {
		return err
	}
	if *since != "" {
		t, err := parseSince(*since, now)
		if err != nil {
			return usagef("--since: %v", err)
		}
		rng.Since = t
	}
	rd, err := c.openReader()
	if err != nil {
		return err
	}
	defer rd.Close()
	w := newExportWriter(c.out)
	if err := w.merge(exportStreams(c.ctx, rd, rng)); err != nil {
		return err
	}
	// The line names the cutoff and not the time of the last line, so that a later export with --since continues from
	// it and misses nothing that was written meanwhile with an earlier time.
	_, err = fmt.Fprintf(exportStderr, "exported %d lines through %s\n", w.lines, auditTime(rng.Cutoff))
	return err
}

// exportLine is one line of the export before it is printed.
type exportLine struct {
	at    int64 // the time of the line in milliseconds, which the merge orders by
	value any   // what is encoded as the line
}

// exportStream is one of the three streams of rule 24. Its lines rise by (at, id), and it is read a page at a time.
type exportStream struct {
	page []exportLine                 // the lines of the last page that are not printed yet
	more bool                         // another page can follow
	read func() ([]exportLine, error) // reads the page after the one it read before
}

// pagedStream makes a stream from a page read of the store and from the line of a row. The read gets the last row of
// the page before, which is the place that the next page starts after.
func pagedStream[R any](read func(after *R) ([]R, error), line func(R) exportLine) *exportStream {
	var last *R
	return &exportStream{more: true, read: func() ([]exportLine, error) {
		rows, err := read(last)
		if err != nil {
			return nil, err
		}
		lines := make([]exportLine, len(rows))
		for i, r := range rows {
			lines[i] = line(r)
		}
		if n := len(rows); n > 0 {
			// A copy, so that the page of rows is not kept until the next read.
			r := rows[n-1]
			last = &r
		}
		return lines, nil
	}}
}

// exportStreams returns the streams in the order that decides between lines of one time (rule 24): the audit rows,
// the requests of the approvals, the decisions of the approvals.
func exportStreams(ctx context.Context, rd *store.AuditReader, rng store.ExportRange) []*exportStream {
	return []*exportStream{
		pagedStream(func(after *store.AuditEvent) ([]store.AuditEvent, error) {
			return rd.ExportAudit(ctx, rng, after, auditPage)
		}, exportAuditLine),
		pagedStream(func(after *store.ExportApproval) ([]store.ExportApproval, error) {
			return rd.ExportCreated(ctx, rng, after, auditPage)
		}, exportCreatedLine),
		pagedStream(func(after *store.ExportApproval) ([]store.ExportApproval, error) {
			return rd.ExportDecided(ctx, rng, after, auditPage)
		}, exportDecidedLine),
	}
}

// The three lines of rule 23. A string field is always present, also when it is empty.

type auditRecordLine struct {
	Record string `json:"record"`
	auditLine
}

type approvalCreatedLine struct {
	Record string `json:"record"`
	Event  string `json:"event"`
	At     string `json:"at"`
	ID     string `json:"id"`
	Flat   string `json:"flat"`
	FlatID string `json:"flat_id"`
	Action string `json:"action"`
	Via    string `json:"via"`
	Reason string `json:"reason"`
}

type approvalDecidedLine struct {
	Record    string `json:"record"`
	Event     string `json:"event"`
	At        string `json:"at"`
	ID        string `json:"id"`
	Flat      string `json:"flat"`
	FlatID    string `json:"flat_id"`
	Action    string `json:"action"`
	State     string `json:"state"`
	DecidedBy string `json:"decided_by"`
	Result    string `json:"result"`
}

func exportAuditLine(e store.AuditEvent) exportLine {
	return exportLine{at: e.Time.UnixMilli(), value: auditRecordLine{Record: "audit", auditLine: auditLineOf(e)}}
}

func exportCreatedLine(a store.ExportApproval) exportLine {
	return exportLine{at: a.At.UnixMilli(), value: approvalCreatedLine{Record: "approval", Event: "created", At: auditTime(a.At),
		ID: a.ID, Flat: a.Flat, FlatID: a.FlatID, Action: a.Action, Via: a.Via, Reason: cutText(a.Reason, exportTextMax)}}
}

func exportDecidedLine(a store.ExportApproval) exportLine {
	return exportLine{at: a.At.UnixMilli(), value: approvalDecidedLine{Record: "approval", Event: "decided", At: auditTime(a.At),
		ID: a.ID, Flat: a.Flat, FlatID: a.FlatID, Action: a.Action, State: a.Status,
		DecidedBy: cutText(a.DecidedBy, exportDecidedByMax), Result: cutText(a.Result, exportTextMax)}}
}

// cutText shortens s to at most limit bytes without splitting a character.
func cutText(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// exportWriter prints the lines of an export and counts them.
type exportWriter struct {
	w     *bufio.Writer
	buf   bytes.Buffer  // the JSON of the line that is being printed, used again for every line
	enc   *json.Encoder // encodes into buf
	lines int
}

func newExportWriter(out io.Writer) *exportWriter {
	w := &exportWriter{w: bufio.NewWriter(out)}
	w.enc = json.NewEncoder(&w.buf)
	return w
}

// merge prints the lines of the streams by their time. Lines of one time come in the order of the streams, and each
// stream gives its own lines by id, which is the order of rule 24. At most one page of each stream is in memory.
func (w *exportWriter) merge(streams []*exportStream) error {
	for {
		var next *exportStream
		for _, s := range streams {
			if len(s.page) == 0 && s.more {
				// What is printed so far goes out before the next read, so a long export reaches its reader page by
				// page, and a failed write stops the command before it reads more. No read is open here: a page
				// read is closed when it returns.
				if err := w.w.Flush(); err != nil {
					return err
				}
				page, err := s.read()
				if err != nil {
					return err
				}
				s.page, s.more = page, len(page) == auditPage
			}
			// The comparison is strict, so an earlier stream keeps its turn among lines of one time.
			if len(s.page) > 0 && (next == nil || s.page[0].at < next.page[0].at) {
				next = s
			}
		}
		if next == nil {
			return w.w.Flush()
		}
		line := next.page[0]
		next.page = next.page[1:]
		if err := w.print(line.value); err != nil {
			return err
		}
	}
}

// print writes v as one JSON line.
func (w *exportWriter) print(v any) error {
	w.buf.Reset()
	if err := w.enc.Encode(v); err != nil { // Encode ends the value with the newline of the line
		return err
	}
	if _, err := w.w.Write(jsonForTerminal(w.buf.Bytes())); err != nil {
		return err
	}
	w.lines++
	return nil
}

// jsonForTerminal returns the JSON text b in a form that the output filter of package cli (internal/cli/safe.go)
// leaves as it is. The filter rewrites the characters that a terminal acts on. JSON escapes the C0 controls itself and
// writes the others as they are, and the filter would turn DEL and a C1 control into \x.., which is not JSON. An agent
// writes the reason of an approval, so one such character would spoil its line for the reader. They become \u escapes
// here: in a JSON text they can only be inside a string, where the escape is the same character. A text without such a
// character is returned as it is, with no copy. `flats audit ls --json` and `flats audit export` both print through it.
func jsonForTerminal(b []byte) []byte {
	var out []byte
	start := 0
	for i := 0; i < len(b); {
		if b[i] < 0x7f {
			i++
			continue
		}
		r, size := utf8.DecodeRune(b[i:])
		if filtered(r) || r == utf8.RuneError && size == 1 {
			out = append(out, b[start:i]...)
			out = fmt.Appendf(out, `\u%04x`, r)
			start = i + size
		}
		i += size
	}
	if out == nil {
		return b
	}
	return append(out, b[start:]...)
}

// filtered reports whether the output filter of package cli rewrites r, for the characters that JSON does not escape
// itself: DEL, the C1 controls and the bidi overrides.
func filtered(r rune) bool {
	return r == 0x7f || r >= 0x80 && r <= 0x9f || r >= 0x202a && r <= 0x202e || r >= 0x2066 && r <= 0x2069
}

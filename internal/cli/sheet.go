package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/gosuda/flats/internal/api"
	"github.com/gosuda/flats/internal/sheet"
	"golang.org/x/term"
)

type sheetClient interface {
	call(context.Context, string, string, url.Values, any, any) (response, error)
}

// csvByteOrderMark starts a CSV file so that spreadsheet programs read it
// as UTF-8.
const csvByteOrderMark = "\xef\xbb\xbf"

func (a *app) sheet(args []string) error {
	if len(args) == 0 {
		return usagef("missing subcommand (get or apply)")
	}
	sub, args := args[0], args[1:]
	fs := a.flags("sheet")
	switch sub {
	case "get":
		q := url.Values{}
		for _, name := range []string{"tab", "range", "format", "include", "revision", "limit"} {
			fs.Func(name, "sheet read selector (include is a comma-separated list of shown, formats, widths)", func(v string) error {
				if q.Has(name) {
					return fmt.Errorf("repeated --%s", name)
				}
				q.Set(name, v)
				return nil
			})
		}
		output := fs.String("output", "", "with --format csv: write the CSV file here instead of standard output")
		pos, err := a.parse(fs, args, 1, 1)
		if err != nil {
			return err
		}
		if _, _, err := api.SheetQuery(q); err != nil {
			return usagef("%s", err)
		}
		if *output != "" && q.Get("format") != "csv" {
			return usagef("--output needs --format csv")
		}
		return a.sheetGet(a.client(), pos[0], q, *output)
	case "apply":
		state := fs.String("state", "", "state of the read the call is based on; overrides the file's state")
		opID := fs.String("op-id", "", "stable operation id for replay after a timeout; otherwise use the file's id or generate one")
		message := fs.String("message", "", "note shown in sheet history")
		pos, err := a.parse(fs, args, 2, 2)
		if err != nil {
			return err
		}
		return a.sheetApply(a.client(), pos[0], pos[1], *state, *opID, *message)
	}
	return usagef("unknown subcommand %q (get or apply)", sub)
}

// sheetColumn is the column letters of a cell address such as "AB12".
func sheetColumn(address string) string {
	return strings.TrimRight(address, "0123456789")
}

// sheetGet reads every page of a sheet read and prints them as one result.
func (a *app) sheetGet(c sheetClient, slug string, q url.Values, output string) error {
	path := "/api/flats/" + pathEscape(slug) + "/sheet"
	revision := q.Get("revision")
	var merged map[string]json.RawMessage
	var rows, shown [][]json.RawMessage
	var formats []json.RawMessage
	var csv strings.Builder
	seen := map[string]bool{}
	for {
		var page map[string]json.RawMessage
		if _, err := c.call(a.ctx, http.MethodGet, path, q, nil, &page); err != nil {
			return err
		}
		var part struct {
			Range  *string             `json:"range"`
			Start  string              `json:"start"`
			Rows   [][]json.RawMessage `json:"rows"`
			Shown  [][]json.RawMessage `json:"shown"`
			Format []json.RawMessage   `json:"formats"`
			CSV    string              `json:"csv"`
			Next   *struct {
				Cursor string `json:"cursor"`
			} `json:"next"`
		}
		for name, dst := range map[string]any{"range": &part.Range, "start": &part.Start, "rows": &part.Rows, "shown": &part.Shown, "formats": &part.Format, "csv": &part.CSV, "next": &part.Next} {
			if raw, ok := page[name]; ok {
				if err := json.Unmarshal(raw, dst); err != nil {
					return fmt.Errorf("unexpected sheet reply: %s: %w", name, err)
				}
			}
		}
		// A page that starts inside a row continues the last row of the
		// page before.
		continues := merged != nil && part.Range != nil && sheetColumn(part.Start) != sheetColumn(strings.SplitN(*part.Range, ":", 2)[0])
		join := func(all, more [][]json.RawMessage) [][]json.RawMessage {
			if continues && len(all) > 0 && len(more) > 0 {
				all[len(all)-1] = append(all[len(all)-1], more[0]...)
				more = more[1:]
			}
			return append(all, more...)
		}
		rows, shown = join(rows, part.Rows), join(shown, part.Shown)
		formats = append(formats, part.Format...)
		csv.WriteString(part.CSV)
		if merged == nil {
			merged = page
		}
		if part.Next == nil {
			break
		}
		if part.Next.Cursor == "" || seen[part.Next.Cursor] {
			return fmt.Errorf("sheet returned a repeated continuation cursor")
		}
		seen[part.Next.Cursor] = true
		// A cursor names the capture of the first page: its selectors,
		// state and page size.
		q = url.Values{"cursor": {part.Next.Cursor}}
		if revision != "" {
			q.Set("revision", revision)
		}
	}
	delete(merged, "next")
	if _, ok := merged["csv"]; ok {
		if !a.jsonOut {
			return a.sheetWriteCSV(csv.String(), output)
		}
		merged["csv"], _ = json.Marshal(csv.String())
	}
	if _, ok := merged["rows"]; ok {
		merged["rows"], _ = json.Marshal(rows)
	}
	if _, ok := merged["shown"]; ok {
		merged["shown"], _ = json.Marshal(shown)
	}
	if _, ok := merged["formats"]; ok {
		merged["formats"], _ = json.Marshal(formats)
	}
	if a.jsonOut {
		return json.NewEncoder(a.out).Encode(merged)
	}
	a.writeJSON(merged)
	return nil
}

// sheetWriteCSV writes the CSV file. A file and a pipe get its exact bytes;
// a terminal gets them through the escaping writer, because cell text comes
// from other people and agents.
func (a *app) sheetWriteCSV(text, output string) error {
	if output == "" {
		w := a.out
		if f, ok := a.env.Stdout.(*os.File); a.env.Stdout != nil && (!ok || !term.IsTerminal(int(f.Fd()))) {
			w = a.env.Stdout
		}
		_, err := io.WriteString(w, csvByteOrderMark+text)
		return err
	}
	if err := os.WriteFile(output, []byte(csvByteOrderMark+text), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(a.out, "Wrote %s\n", output)
	return nil
}

func (a *app) sheetApply(c sheetClient, slug, path, state, opID, message string) error {
	limit, room := sheet.MaxUpdateBodyBytes(), sheet.MaxUpdateReadBytes()
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	// Bound the read: a device or a growing file has no reliable size.
	// White space around the call is no part of it, as for the API.
	raw, err := io.ReadAll(io.LimitReader(f, room+1))
	if err != nil {
		return err
	}
	tooLarge := usagef("%s exceeds the update_sheet limit of %d bytes", path, limit)
	if int64(len(raw)) > room {
		return tooLarge
	}
	raw = bytes.TrimSpace(raw)
	// The file is read as the sheet reads a call, before the fields of the
	// command line go into it: a field twice or in another case is an
	// error of the file, and is not settled by which of the two a map
	// keeps.
	written, err := sheet.DecodeUpdate(raw)
	var refusal *sheet.WriteError
	if errors.As(err, &refusal) && refusal.Reason == "json" {
		return usagef("%s must be an update_sheet JSON object without slug", path)
	}
	if errors.As(err, &refusal) && refusal.Limit == "call_bytes" {
		return tooLarge
	}
	if err != nil {
		return usagef("invalid update_sheet: %s", err)
	}
	if written.HasSlug() {
		return usagef("%s must not contain slug; name the sheet on the command line", path)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return usagef("%s must be an update_sheet JSON object without slug", path)
	}
	if state != "" {
		fields["state"], _ = json.Marshal(state)
	}
	if opID != "" {
		fields["op_id"], _ = json.Marshal(opID)
	} else if written.OpID == "" {
		// An empty id in the file protects no more than none.
		fields["op_id"], _ = json.Marshal("cli-" + rand.Text())
	}
	if message != "" {
		fields["message"], _ = json.Marshal(message)
	}
	// The body holds the values of the file byte for byte. Marshalling
	// them again would spell "<", ">" and "&" as six bytes each, and a
	// file within the bytes of one call could pass them. The names are
	// those of a call, which the first reading checked: none needs an
	// escape.
	body := []byte{'{'}
	for i, name := range slices.Sorted(maps.Keys(fields)) {
		if i > 0 {
			body = append(body, ',')
		}
		body = append(strconv.AppendQuote(body, name), ':')
		body = append(body, fields[name]...)
	}
	body = append(body, '}')
	update, err := sheet.DecodeUpdate(body)
	if errors.As(err, &refusal) && refusal.Limit == "call_bytes" {
		// The bound is that of the request, so what the command adds counts.
		return usagef("%s with the fields the command adds exceeds the update_sheet limit of %d bytes; a generated op_id and the values of --state, --op-id and --message count, so give the file its own op_id or make it smaller", path, limit)
	}
	if err != nil {
		return usagef("invalid update_sheet: %s", err)
	}
	// Print before sending so a lost response still leaves a usable retry id.
	fmt.Fprintf(a.errw, "op_id: %s (after a timeout, send the same file again with --op-id %s)\n", update.OpID, update.OpID)
	var out struct {
		Result   sheet.WriteResult `json:"result"`
		Revision int               `json:"revision"`
		Pending  bool              `json:"pending"`
		Created  bool              `json:"created"`
		Flat     *flatView         `json:"flat"`
	}
	resp, err := c.call(a.ctx, http.MethodPost, "/api/flats/"+pathEscape(slug)+"/sheet", nil, sentAsWritten(body), &out)
	if err != nil {
		return err
	}
	if a.jsonOut {
		a.emitRaw(resp)
		return nil
	}
	revision := "its revision was already deleted by retention"
	if out.Pending {
		revision = "revision pending"
	} else if out.Revision > 0 {
		revision = fmt.Sprintf("revision %d", out.Revision)
	}
	what := "Applied"
	if out.Created {
		what = "Created"
	}
	fmt.Fprintf(a.out, "%s sheet %s: %s, %d cells written, %d formulas, %d formula errors\n", what, slug, revision, out.Result.CellsWritten, out.Result.FormulasWritten, out.Result.FormulaErrors)
	if out.Pending {
		fmt.Fprintln(a.errw, "warning: the write is committed; its revision is not recorded yet. Do not send it again with a new op_id")
	}
	if out.Result.Replay {
		fmt.Fprintln(a.out, "  this op_id was already applied; nothing changed again")
	}
	for _, e := range out.Result.Errors {
		fmt.Fprintf(a.out, "  %s shows %s\n", e.At, e.Error)
	}
	fmt.Fprintf(a.out, "  state: %s\n", out.Result.State)
	if out.Flat != nil {
		printURLs(a.out, *out.Flat)
	}
	return nil
}

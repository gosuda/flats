package cli

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
)

// boardWrite is the reply of a board write or import.
type boardWrite struct {
	Revision *int            `json:"revision"`
	Seq      int64           `json:"seq"`
	IDs      json.RawMessage `json:"ids"`
	Created  bool            `json:"created"`
	Replayed bool            `json:"replayed"`
	Warning  string          `json:"warning"`
}

func (a *app) board(args []string) error {
	if len(args) == 0 {
		return usagef("missing subcommand (get, apply or import)")
	}
	sub, args := args[0], args[1:]
	fs := a.flags("board")
	switch sub {
	case "get":
		outline := fs.Bool("outline", false, "element types, text and connections per frame instead of every field")
		revision := fs.Int("revision", 0, "read this earlier revision")
		pos, err := a.parse(fs, args, 1, 1)
		if err != nil {
			return err
		}
		return a.boardGet(pos[0], *outline, *revision)
	case "apply":
		message := fs.String("message", "", "note shown in the board's history")
		pos, err := a.parse(fs, args, 2, 2)
		if err != nil {
			return err
		}
		return a.boardApply(pos[0], pos[1], *message)
	case "import":
		replace := fs.Bool("replace", false, "replace the whole board instead of adding the file's elements")
		opID := fs.String("op-id", "", "operation id (8 to 120 characters); resending the same file with the same id after a timeout imports it once. Default: a random id, printed with the result")
		pos, err := a.parse(fs, args, 2, 2)
		if err != nil {
			return err
		}
		return a.boardImport(pos[0], pos[1], *replace, *opID)
	}
	return usagef("unknown subcommand %q (get, apply or import)", sub)
}

// boardGet prints every page of a board read.
func (a *app) boardGet(slug string, outline bool, revision int) error {
	q := url.Values{}
	if outline {
		q.Set("format", "outline")
	}
	if revision > 0 {
		q.Set("revision", strconv.Itoa(revision))
	}
	var elements []json.RawMessage
	var first struct {
		Seq      int64           `json:"seq"`
		Revision int             `json:"revision"`
		Pending  bool            `json:"pending"`
		Meta     json.RawMessage `json:"meta"`
	}
	for page := 0; ; page++ {
		var p struct {
			Elements []json.RawMessage `json:"elements"`
			Next     string            `json:"next"`
		}
		resp, err := a.client().call(a.ctx, http.MethodGet, "/api/flats/"+pathEscape(slug)+"/board", q, nil, &p)
		if err != nil {
			return err
		}
		if page == 0 {
			_ = json.Unmarshal(resp.Body, &first)
		}
		elements = append(elements, p.Elements...)
		if p.Next == "" {
			break
		}
		q.Set("cursor", p.Next)
	}
	if a.jsonOut {
		a.writeJSON(map[string]any{"seq": first.Seq, "revision": first.Revision, "pending": first.Pending, "meta": first.Meta, "elements": elements})
		return nil
	}
	if revision > 0 {
		fmt.Fprintf(a.out, "Board %s as of revision %d: %d elements\n", slug, revision, len(elements))
	} else {
		pending := ""
		if first.Pending {
			pending = "; people's edits are not a revision yet"
		}
		fmt.Fprintf(a.out, "Board %s at seq %d (revision %d): %d elements%s\n", slug, first.Seq, first.Revision, len(elements), pending)
	}
	for _, e := range elements {
		var line bytes.Buffer
		if json.Compact(&line, e) != nil {
			line.Write(e)
		}
		fmt.Fprintf(a.out, "%s\n", line.Bytes())
	}
	return nil
}

// boardApply sends an update_board body from a file. A body without op_id
// gets a random one.
func (a *app) boardApply(slug, path, message string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil {
		return fmt.Errorf("%s is not an update_board JSON object: %w", path, err)
	}
	if _, ok := body["op_id"]; !ok {
		body["op_id"], _ = json.Marshal("cli-" + rand.Text())
	}
	if message != "" {
		body["message"], _ = json.Marshal(message)
	}
	var out boardWrite
	resp, err := a.client().call(a.ctx, http.MethodPost, "/api/flats/"+pathEscape(slug)+"/board", nil, body, &out)
	if err != nil {
		return err
	}
	if a.jsonOut {
		a.emitRaw(resp)
		return nil
	}
	a.printBoardWrite(slug, "Applied", out)
	return nil
}

// boardImport sends a board data file under opID, or a random operation id
// when opID is empty. Resending with the same opID does not import twice.
func (a *app) boardImport(slug, path string, replace bool, opID string) error {
	if opID == "" {
		opID = "cli-" + rand.Text()
	} else if len(opID) < 8 || len(opID) > 120 {
		return usagef("--op-id must be 8 to 120 characters")
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	q := url.Values{"mode": {"add"}, "op_id": {opID}}
	if replace {
		q.Set("mode", "replace")
	}
	resp, err := a.client().do(a.ctx, http.MethodPost, "/api/flats/"+pathEscape(slug)+"/board/import", q, f, "application/zip")
	if err != nil {
		return err
	}
	if resp.Status >= 300 {
		if a.jsonOut {
			a.emitRaw(resp)
			return exitCode(ExitError)
		}
		return newAPIError(resp)
	}
	if a.jsonOut {
		a.emitRaw(resp)
		return nil
	}
	var out boardWrite
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		return fmt.Errorf("unexpected reply: %w", err)
	}
	a.printBoardWrite(slug, "Imported "+path+" into", out)
	fmt.Fprintf(a.out, "  op_id: %s (resend with --op-id %s to import this file at most once)\n", opID, opID)
	return nil
}

func (a *app) printBoardWrite(slug, verb string, out boardWrite) {
	what := "board " + slug
	if out.Created {
		what = "new board " + slug
	}
	rev := "revision pending"
	if out.Revision != nil {
		rev = "revision " + strconv.Itoa(*out.Revision)
	}
	fmt.Fprintf(a.out, "%s %s: %s, seq %d\n", verb, what, rev, out.Seq)
	if len(out.IDs) > 0 && string(out.IDs) != "{}" && string(out.IDs) != "[]" && string(out.IDs) != "null" {
		fmt.Fprintf(a.out, "  ids: %s\n", out.IDs)
	}
	if out.Replayed {
		fmt.Fprintln(a.out, "  this op_id was already applied; nothing changed again")
	}
	if out.Warning != "" {
		fmt.Fprintf(a.out, "  warning: %s\n", out.Warning)
	}
}

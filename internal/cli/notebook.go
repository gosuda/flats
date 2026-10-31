package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"strconv"

	"github.com/gosuda/flats/internal/api"
	"github.com/gosuda/flats/internal/notebook"
)

const defaultNotebookInputBytes = notebook.MaxAgentOperationBytes + 4*(((20<<20)+2)/3)

type notebookClient interface {
	call(context.Context, string, string, url.Values, any, any) (response, error)
}

func (a *app) notebook(args []string) error {
	if len(args) == 0 {
		return usagef("missing subcommand (get or apply)")
	}
	sub, args := args[0], args[1:]
	fs := a.flags("notebook")
	switch sub {
	case "get":
		q := url.Values{}
		for _, name := range []string{"page", "database", "view", "sort", "filter", "format", "limit", "revision", "cursor", "op-id", "retry-hint"} {
			key := name
			switch name {
			case "op-id":
				key = "op_id"
			case "retry-hint":
				key = "retry_hint"
			}
			help := "notebook read selector (sort and filter are JSON arrays)"
			if name == "retry-hint" {
				help = "admission ticket from an unavailable read; repeat the original selectors before expiry"
			}
			fs.Func(name, help, func(v string) error {
				if q.Has(key) {
					return fmt.Errorf("repeated --%s", name)
				}
				q.Set(key, v)
				return nil
			})
		}
		pos, err := a.parse(fs, args, 1, 1)
		if err != nil {
			return err
		}
		if _, _, err := api.NotebookQuery(q); err != nil {
			return usagef("%s", err)
		}
		return a.notebookGet(a.client(), pos[0], q)
	case "apply":
		message := fs.String("message", "", "note shown in notebook history")
		opID := fs.String("op-id", "", "stable operation id for replay after a timeout; otherwise use the file's id or generate one")
		maxInputBytes := fs.Int64("max-input-bytes", defaultNotebookInputBytes, "local input byte limit before decoding; default admits 20 MiB of uploads plus 8 MiB of operation JSON; raise for larger host upload limits")
		pos, err := a.parse(fs, args, 2, 2)
		if err != nil {
			return err
		}
		return a.notebookApplyWithLimit(a.client(), pos[0], pos[1], *message, *opID, *maxInputBytes)
	}
	return usagef("unknown subcommand %q (get or apply)", sub)
}

func (a *app) notebookGet(c notebookClient, slug string, q url.Values) error {
	path := "/api/flats/" + pathEscape(slug) + "/notebook"
	opID := q.Get("op_id")
	seen := map[string]bool{}
	if cursor := q.Get("cursor"); cursor != "" {
		seen[cursor] = true
	}
	for page := 0; ; page++ {
		var part struct {
			Next     string `json:"next"`
			Complete *bool  `json:"complete"`
			Pending  bool   `json:"pending"`
		}
		resp, err := c.call(a.ctx, http.MethodGet, path, q, nil, &part)
		if err != nil {
			return err
		}
		if part.Complete == nil || *part.Complete != (part.Next == "") {
			return fmt.Errorf("incomplete notebook reply: expected complete and a continuation cursor")
		}
		if a.jsonOut {
			if page == 0 {
				fmt.Fprint(a.out, "[")
			} else {
				fmt.Fprint(a.out, ",")
			}
			a.out.Write(resp.Body)
		} else {
			if page == 0 {
				fmt.Fprintf(a.out, "Notebook %s\n", slug)
				if part.Pending {
					fmt.Fprintln(a.errw, "warning: current edits are not a revision yet")
				}
			}
			a.emitRaw(resp)
		}
		if part.Next == "" {
			if a.jsonOut {
				fmt.Fprintln(a.out, "]")
			}
			return nil
		}
		if seen[part.Next] {
			return fmt.Errorf("notebook returned a repeated continuation cursor")
		}
		seen[part.Next] = true
		// Cursors bind the original selectors, revision and page size.
		q = url.Values{"cursor": {part.Next}}
		if opID != "" {
			q.Set("op_id", opID)
		}
	}
}

func (a *app) notebookApply(c notebookClient, slug, path, message, opID string) error {
	return a.notebookApplyWithLimit(c, slug, path, message, opID, defaultNotebookInputBytes)
}

func (a *app) notebookApplyWithLimit(c notebookClient, slug, path, message, opID string, maxInputBytes int64) error {
	if maxInputBytes <= 0 || maxInputBytes == math.MaxInt64 {
		return usagef("--max-input-bytes must be positive and less than %d", int64(math.MaxInt64))
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.Size() > maxInputBytes {
		return usagef("%s exceeds --max-input-bytes (%d)", path, maxInputBytes)
	}
	// Bound reads too: devices and files growing after Stat have no reliable size.
	raw, err := io.ReadAll(io.LimitReader(f, maxInputBytes+1))
	if err != nil {
		return err
	}
	if int64(len(raw)) > maxInputBytes {
		return usagef("%s exceeds --max-input-bytes (%d)", path, maxInputBytes)
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) < 2 || raw[0] != '{' || raw[len(raw)-1] != '}' {
		return usagef("%s must be an update_notebook JSON object without slug", path)
	}
	// Validate the original bytes before map decoding can discard duplicate keys.
	prefix := struct {
		Slug string `json:"slug"`
	}{slug}
	head, _ := json.Marshal(prefix)
	full := append([]byte{}, head[:len(head)-1]...)
	if len(bytes.TrimSpace(raw[1:len(raw)-1])) > 0 {
		full = append(full, ',')
	}
	full = append(full, raw[1:]...)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return usagef("invalid update_notebook: %s", err)
	}
	// A missing op id is the only field the CLI supplies before admission.
	if _, present := fields["op_id"]; !present {
		id := opID
		if id == "" {
			id = "cli-" + rand.Text()
		}
		encoded, _ := json.Marshal(id)
		full = append(append(append([]byte(`{"op_id":`), encoded...), ','), full[1:]...)
	}
	update, err := notebook.DecodeUpdate(full)
	if err != nil {
		return usagef("invalid update_notebook: %s", err)
	}
	if opID != "" && update.OpID != opID {
		return usagef("--op-id differs from the file's op_id; preserve the original id to replay")
	}
	fields["op_id"], _ = json.Marshal(update.OpID)
	if message != "" {
		fields["message"], _ = json.Marshal(message)
	}
	// Print before sending so a lost response still leaves a usable retry id.
	fmt.Fprintf(a.errw, "op_id: %s (retry the same body with --op-id %s)\n", update.OpID, update.OpID)
	var out struct {
		Result   notebook.WriteResult `json:"result"`
		Revision int                  `json:"revision"`
		Pending  bool                 `json:"pending"`
		Created  bool                 `json:"created"`
	}
	resp, err := c.call(a.ctx, http.MethodPost, "/api/flats/"+pathEscape(slug)+"/notebook", nil, fields, &out)
	if err != nil {
		return err
	}
	if a.jsonOut {
		a.emitRaw(resp)
		return nil
	}
	revision := "revision unavailable"
	if out.Pending || out.Result.RevisionPending {
		revision = "revision pending"
	} else if out.Revision > 0 {
		revision = "revision " + strconv.Itoa(out.Revision)
	} else {
		for _, warning := range out.Result.Warnings {
			if warning.Code == "revision_pruned" {
				revision = "revision pruned"
				break
			}
		}
	}
	fmt.Fprintf(a.out, "Applied notebook %s: %s, %d operations\n", slug, revision, out.Result.Applied)
	if out.Pending || out.Result.RevisionPending {
		fmt.Fprintln(a.errw, "warning: edit committed; revision import is pending. Do not submit a new op_id to retry it")
	}
	if out.Result.Replay {
		fmt.Fprintln(a.out, "  this op_id was already applied; nothing changed again")
	}
	for _, warning := range out.Result.Warnings {
		fmt.Fprintf(a.errw, "warning: %s: %s\n", warning.Path, warning.Message)
	}
	// Keep mappings, guard state and sample completeness available in text mode.
	a.writeJSON(out.Result)
	return nil
}

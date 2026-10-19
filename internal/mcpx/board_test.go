package mcpx

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/wsconn"
)

// withOrigin adds an Origin header to the handshake written through it.
type withOrigin struct {
	net.Conn
	origin string
	done   bool
}

func (c *withOrigin) Write(p []byte) (int, error) {
	if !c.done && bytes.HasSuffix(p, []byte("\r\n\r\n")) {
		c.done = true
		q := append(slices.Clone(p[:len(p)-2]), []byte("Origin: "+c.origin+"\r\n\r\n")...)
		if _, err := c.Conn.Write(q); err != nil {
			return 0, err
		}
		return len(p), nil
	}
	return c.Conn.Write(p)
}

// shareFromCanvas sends ids from the board's canvas with Send to agent and
// returns the share number.
func shareFromCanvas(t *testing.T, privateURL string, ids []string) int {
	t.Helper()
	u, err := url.Parse(privateURL)
	if err != nil {
		t.Fatal(err)
	}
	ws, err := wsconn.Dial(context.Background(), func(ctx context.Context) (net.Conn, error) {
		c, err := net.Dial("tcp", "127.0.0.1:"+u.Port())
		if err != nil {
			return nil, err
		}
		return &withOrigin{Conn: c, origin: strings.TrimSuffix(privateURL, "/")}, nil
	}, u.Host, "/_board/ws")
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close(1000, "")
	send := func(v any) {
		b, _ := json.Marshal(v)
		if err := ws.WriteFrame(wsconn.OpText, b); err != nil {
			t.Fatal(err)
		}
	}
	send(map[string]any{"t": "hello", "v": 1, "pending": map[string]any{"count": 0}, "editor": "ed1", "name": "Pat"})
	welcomed := false
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		raw, err := ws.ReadMessage(64 << 20)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		_ = json.Unmarshal([]byte(raw), &m)
		switch m["t"] {
		case "welcome":
			welcomed = true
			send(map[string]any{"t": "share", "ids": ids})
		case "shared":
			if welcomed {
				return int(m["n"].(float64))
			}
		}
	}
	t.Fatal("no shared answer")
	return 0
}

func TestBoardTools(t *testing.T) {
	e := newEnv(t)
	var fi FlatInfo
	text, failed := call(t, e.local, "create_flat", map[string]any{"slug": "plan", "name": "Plan", "type": "board"}, &fi)
	if failed || fi.Type != "board" || !fi.Running || !strings.Contains(text, "Created empty board plan") {
		t.Fatalf("create_flat board: %s", text)
	}
	text, _ = call(t, e.local, "get_flat", map[string]any{"slug": "plan"}, nil)
	if !strings.Contains(text, "board, running") {
		t.Fatalf("get_flat: %s", text)
	}

	write := map[string]any{"slug": "plan", "op_id": "sort-plan-1", "message": "first notes",
		"ops": []any{
			map[string]any{"op": "add", "ref": "$f", "el": map[string]any{"type": "frame", "text": "Ideas", "x": 0, "y": 0, "w": 900, "h": 600}},
			map[string]any{"op": "add", "ref": "$a", "el": map[string]any{"type": "sticky", "text": "alpha", "frame": "$f"}},
			map[string]any{"op": "add", "ref": "$b", "el": map[string]any{"type": "sticky", "text": "beta", "frame": "$f"}},
			map[string]any{"op": "add", "el": map[string]any{"type": "connector", "ends": []any{map[string]any{"el": "$a"}, map[string]any{"el": "$b"}}}},
		},
		"layout": []any{map[string]any{"kind": "row", "ids": []any{"$a", "$b"}, "in": "$f"}},
	}
	var out UpdateBoardOut
	text, failed = call(t, e.local, "update_board", write, &out)
	if failed || out.Revision == nil || *out.Revision != 2 || len(out.Layout) != 2 || out.Flat == nil {
		t.Fatalf("update_board: %s", text)
	}
	ids := out.IDs.(map[string]any)
	// The same op_id and request returns the first result.
	var again UpdateBoardOut
	text, failed = call(t, e.local, "update_board", write, &again)
	if failed || again.Revision == nil || *again.Revision != 2 || again.Seq != out.Seq || !strings.Contains(text, "already applied") {
		t.Fatalf("resend: %s", text)
	}
	write["message"] = "another request"
	if text, failed = call(t, e.local, "update_board", write, nil); !failed || !strings.Contains(text, `"category":"conflict"`) {
		t.Fatalf("op_id reused for another request: %s", text)
	}
	// A guard against a changed board is a conflict.
	stale := map[string]any{"slug": "plan", "op_id": "guarded-1", "if_seq": out.Seq - 1, "ops": []any{map[string]any{"op": "delete", "ids": []any{ids["$a"]}}}}
	if text, failed = call(t, e.local, "update_board", stale, nil); !failed || !strings.Contains(text, `"category":"conflict"`) {
		t.Fatalf("stale guard: %s", text)
	}

	var page GetBoardOut
	text, failed = call(t, e.local, "get_board", map[string]any{"slug": "plan"}, &page)
	if failed || len(page.Elements) != 4 || page.Seq != out.Seq || page.Revision != 2 || page.Meta["title"] != "Plan" {
		t.Fatalf("get_board: %s", text)
	}
	var outline GetBoardOut
	call(t, e.local, "get_board", map[string]any{"slug": "plan", "format": "outline", "frame": ids["$f"]}, &outline)
	if len(outline.Elements) != 3 || !slices.ContainsFunc(outline.Elements, func(el map[string]any) bool { return el["text"] == "alpha" && el["x"] == nil }) {
		t.Fatalf("outline: %+v", outline.Elements)
	}
	var first GetBoardOut
	call(t, e.local, "get_board", map[string]any{"slug": "plan", "limit": 3}, &first)
	if len(first.Elements) != 3 || first.Next == "" {
		t.Fatalf("first page: %+v", first)
	}
	var second GetBoardOut
	call(t, e.local, "get_board", map[string]any{"slug": "plan", "cursor": first.Next}, &second)
	if len(second.Elements) != 1 || second.Next != "" {
		t.Fatalf("second page: %+v", second)
	}
	if text, failed = call(t, e.local, "get_board", map[string]any{"slug": "plan", "cursor": first.Next}, nil); !failed || !strings.Contains(text, `"category":"conflict"`) || !strings.Contains(text, "cursor expired") {
		t.Fatalf("expired cursor: %s", text)
	}
	if text, failed = call(t, e.local, "get_board", map[string]any{"slug": "plan", "rect": []any{1, 2, 3}}, nil); !failed || !strings.Contains(text, `"category":"invalid"`) {
		t.Fatalf("bad rect: %s", text)
	}
	var old GetBoardOut
	call(t, e.local, "get_board", map[string]any{"slug": "plan", "revision": 1}, &old)
	if old.Revision != 1 || len(old.Elements) != 0 {
		t.Fatalf("revision 1: %+v", old)
	}
	text, _ = call(t, e.local, "get_revision", map[string]any{"slug": "plan", "revision": 2}, nil)
	if !strings.Contains(text, "get_board") {
		t.Fatalf("get_revision of a board must point to get_board: %s", text)
	}

	var shares GetBoardShareOut
	call(t, e.local, "get_board_share", map[string]any{"slug": "plan"}, &shares)
	if len(shares.Shares) != 0 {
		t.Fatalf("shares before any: %+v", shares)
	}
	if text, failed = call(t, e.local, "get_board_share", map[string]any{"slug": "plan", "n": 7}, nil); !failed || !strings.Contains(text, `"category":"not_found"`) {
		t.Fatalf("unknown share: %s", text)
	}
	n := shareFromCanvas(t, fi.PrivateURL, []string{ids["$a"].(string), "e_gone000000"})
	call(t, e.local, "get_board_share", map[string]any{"slug": "plan", "n": n}, &shares)
	if len(shares.Shares) != 1 || len(shares.Shares[0].Elements) != 1 || !slices.Equal(shares.Shares[0].Missing, []string{"e_gone000000"}) || shares.Shares[0].Label != "Pat" {
		t.Fatalf("share: %+v", shares)
	}

	// import_board copies an exported data file with new ids.
	_, zipped := get(t, fi.PrivateURL+"/_board/export.zip")
	imp := map[string]any{"slug": "copy", "op_id": "import-copy-1", "archive": base64.StdEncoding.EncodeToString(zipped), "mode": "add"}
	var imported UpdateBoardOut
	text, failed = call(t, e.local, "import_board", imp, &imported)
	if failed || !imported.Created || imported.Revision == nil || len(imported.IDs.([]any)) != 4 {
		t.Fatalf("import_board: %s", text)
	}
	text, failed = call(t, e.local, "import_board", imp, &imported)
	if failed || !strings.Contains(text, "already imported") {
		t.Fatalf("import resend: %s", text)
	}

	// The revision tools work on boards.
	var revs struct {
		Revisions []struct {
			Kind     string         `json:"kind"`
			Elements map[string]int `json:"elements"`
		} `json:"revisions"`
	}
	call(t, e.local, "list_revisions", map[string]any{"slug": "plan"}, &revs)
	if len(revs.Revisions) != 2 || revs.Revisions[0].Kind != "agent" || revs.Revisions[0].Elements["added"] != 4 {
		t.Fatalf("list_revisions: %+v", revs)
	}
	var done ActionDone
	if text, failed = call(t, e.local, "rollback_revision", map[string]any{"slug": "plan", "revision": 1}, &done); failed || done.Revision != 3 {
		t.Fatalf("rollback_revision: %s", text)
	}
	call(t, e.local, "get_board", map[string]any{"slug": "plan"}, &page)
	if len(page.Elements) != 0 {
		t.Fatalf("after rollback: %+v", page.Elements)
	}
	if text, failed = call(t, e.local, "pin_revision", map[string]any{"slug": "plan", "revision": 2, "pinned": true}, nil); failed {
		t.Fatalf("pin_revision: %s", text)
	}
	// Version tools point to the revision tools.
	if text, failed = call(t, e.local, "list_versions", map[string]any{"slug": "plan"}, nil); !failed || !strings.Contains(text, "rollback_revision") {
		t.Fatalf("list_versions on a board: %s", text)
	}
}

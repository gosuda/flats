package docs

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

// revEntry is one outbox entry of the docs worker.
type revEntry struct {
	Seq        int64    `json:"seq"`
	Kind       string   `json:"kind"`
	Authors    []string `json:"authors"`
	Message    string   `json:"message"`
	RollbackOf int      `json:"rollback_of"`
	Op         string   `json:"op"`
	Source     string   `json:"source"`
	Documents  []struct {
		Path string `json:"path"`
		Hash string `json:"hash"`
		Size int64  `json:"size"`
	} `json:"documents"`
}

func (e revEntry) hash(path string) string {
	for _, d := range e.Documents {
		if d.Path == path {
			return d.Hash
		}
	}
	return ""
}

func digestOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// hostOp calls a revision host operation with the trusted headers.
func (a *app) hostOp(t *testing.T, method, op string, body any) (int, map[string]any) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, a.srv.URL+"/_docs/host/revisions/"+op, r)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Flats-Access", "private")
	req.Header.Set("X-Flats-Host-Op", "revisions")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	raw, _ := io.ReadAll(res.Body)
	_ = json.Unmarshal(raw, &out)
	return res.StatusCode, out
}

// outbox returns every unacknowledged entry.
func (a *app) outbox(t *testing.T) []revEntry {
	t.Helper()
	var all []revEntry
	after := int64(0)
	for {
		status, out := a.hostOp(t, "GET", fmt.Sprintf("outbox?after=%d", after), nil)
		if status != 200 {
			t.Fatal("outbox", status, out)
		}
		b, _ := json.Marshal(out["entries"])
		var page []revEntry
		if err := json.Unmarshal(b, &page); err != nil {
			t.Fatal(err)
		}
		all = append(all, page...)
		if len(page) == 0 || out["more"] != true {
			return all
		}
		after = page[len(page)-1].Seq
	}
}

func (a *app) revisionText(t *testing.T, hash string) string {
	t.Helper()
	status, out := a.hostOp(t, "GET", "text?hash="+hash, nil)
	if status != 200 {
		t.Fatal("text", hash, status, out)
	}
	return out["text"].(string)
}

func TestRevisionHostOperationsAreHostOnly(t *testing.T) {
	t.Parallel()
	a := startApp(t, t.TempDir(), initialText, "v1")
	for _, h := range []http.Header{{}, {"X-Flats-Access": {"private"}}, {"X-Flats-Host-Op": {"revisions"}}, {"X-Flats-Access": {"public"}, "X-Flats-Host-Op": {"revisions"}}} {
		if status, _, _ := a.getHeaders(t, "/_docs/host/revisions/outbox", h); status != 403 {
			t.Fatal("revision outbox reachable without the host channel", h, status)
		}
	}
}

func TestRevisionsCreatedAndAgentEdit(t *testing.T) {
	t.Parallel()
	a := startApp(t, t.TempDir(), initialText, "v1")
	if status, out := a.hostOp(t, "POST", "sync", map[string]any{}); status != 200 || out["store"] == nil {
		t.Fatal("sync", status, out)
	}
	entries := a.outbox(t)
	if len(entries) != 1 || entries[0].Kind != "created" || entries[0].Source != "v1" || entries[0].hash("index.md") != digestOf(initialText) || entries[0].hash("nested/other.md") != digestOf("# Other\n") {
		t.Fatalf("first entry %+v", entries)
	}
	if got := a.revisionText(t, entries[0].hash("index.md")); got != initialText {
		t.Fatal("recorded text", got)
	}
	// A person's edit opens a group; the agent edit records it first.
	x := connect(t, a.srv.URL, "private")
	x.hello(t, nil)
	edit(t, x, "human", fixture(t).Independent)
	live := a.document(t)["markdown"].(string)
	status, res := a.postEdit(t, hostEdit, `{"ops":[{"op":"insert","text":"Agent paragraph","at":"end"}],"label":"Agent (MCP)"}`)
	if status != 200 {
		t.Fatal("edit", status, res)
	}
	entries = a.outbox(t)
	if len(entries) != 3 {
		t.Fatalf("entries %+v", entries)
	}
	person, agent := entries[1], entries[2]
	if person.Kind != "person" || len(person.Authors) != 1 || person.hash("index.md") != digestOf(live) {
		t.Fatalf("person entry %+v", person)
	}
	if agent.Kind != "agent" || len(agent.Authors) != 1 || agent.Authors[0] != "Agent (MCP)" || agent.hash("index.md") != res["hash"] {
		t.Fatalf("agent entry %+v", agent)
	}
	// Acknowledged entries go; recorded texts stay for later entries.
	status, out := a.hostOp(t, "POST", "sync", map[string]any{})
	if status != 200 {
		t.Fatal(status, out)
	}
	if status, out := a.hostOp(t, "POST", "ack", map[string]any{"store": out["store"], "through": agent.Seq}); status != 200 {
		t.Fatal("ack", status, out)
	}
	if left := a.outbox(t); len(left) != 0 {
		t.Fatal("acknowledged entries remain", left)
	}
	if got := a.revisionText(t, agent.hash("index.md")); got == "" {
		t.Fatal("recorded text released after acknowledgement")
	}
}

func TestRevisionStageTakesEscapedText(t *testing.T) {
	t.Parallel()
	a := startApp(t, t.TempDir(), initialText, "v1")
	// Go's JSON escapes < as six bytes: a valid 1 MiB text grows to 6 MiB.
	text := strings.Repeat("<", 1024*1024)
	status, out := a.hostOp(t, "POST", "stage", map[string]any{"text": text})
	if status != 200 || out["hash"] != digestOf(text) {
		t.Fatal("stage", status, out)
	}
	if status, _ := a.hostOp(t, "POST", "stage", map[string]any{"text": text + "x"}); status != 400 {
		t.Fatal("over 1 MiB staged", status)
	}
}

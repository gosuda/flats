package runtime

import (
	"encoding/json"
	"strings"
	"testing"

	runtimeref "github.com/gosuda/flats/docs"
)

// These run the actual published snippets, so doc edits cannot silently break
// the example or substitute Promise/byte-store semantics for the real API.
func TestRuntimeReferenceExamples(t *testing.T) {
	parts := strings.Split(runtimeref.Markdown, "```js\n")
	if len(parts) != 3 {
		t.Fatal("expected the server and FILES binary examples")
	}
	server := strings.Split(parts[1], "```")[0]
	f := mustStart(t, newManager(t), "ref-server", map[string]string{"index.js": server}, "index.js", nil)
	r := f.do(t, "GET", "/healthz", "")
	if r.status != 200 || r.body != "ok" {
		t.Fatalf("documented health: %+v", r)
	}
	r = f.do(t, "GET", "/", "")
	var out struct {
		Hits int    `json:"hits"`
		Last string `json:"last"`
	}
	if err := json.Unmarshal([]byte(r.body), &out); err != nil || out.Hits != 1 || out.Last != "1" {
		t.Fatalf("documented DB/FILES example: %s, %v", r.body, err)
	}
	binary := strings.Split(parts[2], "```")[0]
	code := `export default { fetch(request, env) {` + binary + `
const missing = env.FILES.get("attachments/demo.b64");
const absentDelete = env.FILES.delete("attachments/demo.b64");
const put = env.FILES.put("unicode", "한글");
let invalid = false;
try { env.FILES.put("../escape", "x"); } catch (_) { invalid = true; }
return Response.json({decoded: Array.from(decoded), keys, removed, missing,
absentDelete, synchronous: put === undefined, unicode: env.FILES.get("unicode"), invalid});
} };`
	b := mustStart(t, newManager(t), "ref-binary", map[string]string{"index.js": code}, "index.js", nil)
	r = b.do(t, "GET", "/", "")
	want := `{"decoded":[0,128,255],"keys":["attachments/demo.b64"],"removed":true,"missing":null,"absentDelete":false,"synchronous":true,"unicode":"한글","invalid":true}`
	if r.status != 200 || r.body != want {
		t.Fatalf("documented binary/missing/error contract: %d %s", r.status, r.body)
	}
}

// Verify the documented boundary cases through the published JS API, including
// host I/O exceptions rather than merely matching words in the reference.
func TestRuntimeReferenceFilesystemAndJSON(t *testing.T) {
	code := `export default { fetch(request, env) {
 env.FILES.put("parent", "x");
 const throws = f => { try { f(); return false; } catch (_) { return true; } };
 const object = {z:1, a:[1,2], m:"<>&"};
 const [{value}] = env.DB.query("SELECT ? AS value", [object]);
 return Response.json({get: throws(() => env.FILES.get("parent/child")),
 put: throws(() => env.FILES.put("parent/child", "x")),
 delete: throws(() => env.FILES.delete("parent/child")), value,
 stringify: JSON.stringify(object)});
} };`
	f := mustStart(t, newManager(t), "ref-errors", map[string]string{"index.js": code}, "index.js", nil)
	r := f.do(t, "GET", "/", "")
	var out struct {
		Get, Put, Delete bool
		Value, Stringify string
	}
	if err := json.Unmarshal([]byte(r.body), &out); err != nil {
		t.Fatal(err)
	}
	want := `{"a":[1,2],"m":"\u003c\u003e\u0026","z":1}`
	if r.status != 200 || !out.Get || !out.Put || !out.Delete || out.Value != want || out.Value == out.Stringify {
		t.Fatalf("documented I/O and host JSON contract: %d %+v", r.status, out)
	}
}

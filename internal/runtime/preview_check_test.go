package runtime

import (
	"testing"

	runtimeref "github.com/gosuda/flats/docs"
)

// The published browser check script must parse in every form agents run it
// in: as a script, and as one expression, which tools that take a function
// string evaluate as `(() => <script>)`. It needs a DOM, so each form is
// compiled inside a function that is never called.
func TestPreviewCheckScriptParses(t *testing.T) {
	t.Parallel()
	forms := map[string]string{
		"function body":       "function previewCheck() {\n" + runtimeref.PreviewCheck + "\n}\n",
		"arrow function body": "const previewCheck = (() => " + runtimeref.PreviewCheck + "\n);\n",
		"expression":          "function previewCheck() {\n  return (" + runtimeref.PreviewCheck + "\n  );\n}\n",
	}
	for name, form := range forms {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			code := form + `export default { fetch() { return new Response(typeof previewCheck); } };`
			f := mustStart(t, newManager(t), "preview-check", map[string]string{"index.js": code}, "index.js", nil)
			if r := f.do(t, "GET", "/", ""); r.status != 200 || r.body != "function" {
				t.Fatalf("preview check script did not compile as %s: %+v", name, r)
			}
		})
	}
}

package runtime

import (
	"testing"

	runtimeref "github.com/gosuda/flats/docs"
)

// The published browser check script must at least parse as JavaScript. It
// needs a DOM, so it is compiled inside a function that is never called.
func TestPreviewCheckScriptParses(t *testing.T) {
	t.Parallel()
	code := "function previewCheck() {\n" + runtimeref.PreviewCheck + "\n}\n" +
		`export default { fetch() { return new Response(typeof previewCheck); } };`
	f := mustStart(t, newManager(t), "preview-check", map[string]string{"index.js": code}, "index.js", nil)
	if r := f.do(t, "GET", "/", ""); r.status != 200 || r.body != "function" {
		t.Fatalf("preview check script did not compile: %+v", r)
	}
}
func TestPreviewCheckWrappedExecution(t *testing.T) {
	t.Parallel()
	domPrelude := `
function setupDOM(opts = {}) {
	globalThis.location = { href: "http://localhost/draft" };
	globalThis.matchMedia = (q) => ({ matches: opts.dark ?? false });
	globalThis.getComputedStyle = (el) => el.style || {
		backgroundImage: "none",
		backgroundColor: "rgb(255, 255, 255)",
		opacity: "1",
		display: "block",
		overflowX: "visible",
		color: "rgb(0, 0, 0)",
		fontSize: "16px",
		fontWeight: "400",
		colorScheme: opts.dark ? "dark" : "light",
	};
	let canvasFill = "";
	const dummyCanvas = {
		clearRect() {},
		fillRect() {},
		getImageData() {
			if (canvasFill === "rgba(0,0,0,0)") return { data: [0, 0, 0, 0] };
			return { data: [255, 255, 255, 255] };
		},
		get fillStyle() { return canvasFill; },
		set fillStyle(v) { canvasFill = v; },
	};
	const root = {
		tagName: "HTML",
		id: "",
		classList: [],
		clientWidth: 1280,
		clientHeight: 800,
		scrollWidth: opts.scrollWidth ?? 1280,
		style: { backgroundImage: "none", backgroundColor: opts.unpainted ? "rgba(0,0,0,0)" : "rgb(255, 255, 255)", opacity: "1", display: "block", colorScheme: opts.dark ? "dark" : "light" },
		querySelectorAll: () => [],
		parentElement: null,
		getRootNode: () => ({ host: null }),
	};
	const body = {
		tagName: "BODY",
		id: "",
		classList: [],
		style: { backgroundImage: "none", backgroundColor: opts.unpainted ? "rgba(0,0,0,0)" : "rgb(255, 255, 255)", opacity: "1", display: "block" },
		querySelectorAll: () => [],
		parentElement: root,
		getRootNode: () => ({ host: null }),
	};
	globalThis.document = {
		title: opts.title ?? "Draft Preview",
		documentElement: root,
		body: body,
		querySelector: (sel) => {
			if (sel.includes("viewport") && !opts.noViewport) return { content: "width=device-width, initial-scale=1" };
			if (sel.includes("icon") && !opts.noIcon) return { href: "/favicon.ico" };
			return null;
		},
		querySelectorAll: () => [],
		createElement: (tag) => {
			if (tag === "canvas") return { getContext: () => dummyCanvas };
			return {};
		},
		createTreeWalker: () => ({ nextNode: () => null }),
		createRange: () => ({ selectNodeContents() {}, getBoundingClientRect: () => ({ width: 0, height: 0 }) }),
	};
	globalThis.performance = {
		getEntriesByType: () => [],
	};
	globalThis.NodeFilter = {
		SHOW_TEXT: 4,
		FILTER_ACCEPT: 1,
		FILTER_REJECT: 2,
	};
	globalThis.SVGElement = class {};
}
`
	code := domPrelude + "\n" +
		"const valueWrap = (" + runtimeref.PreviewCheck + ");\n" +
		"function bodyWrap(opts) {\n" +
		"	return (" + runtimeref.PreviewCheck + ")(opts);\n" +
		"}\n" +
		`export default {
	fetch(req) {
		const url = new URL(req.url);
		const shape = url.searchParams.get("shape");
		const defective = url.searchParams.get("defective") === "true";
		setupDOM(defective ? { title: "", noViewport: true, unpainted: true } : {});
		let verdict;
		if (shape === "value") {
			verdict = valueWrap();
		} else if (shape === "body") {
			verdict = bodyWrap();
		} else {
			return new Response("bad shape", { status: 400 });
		}
		return Response.json(verdict);
	}
};`
	f := mustStart(t, newManager(t), "preview-check-wrap", map[string]string{"index.js": code}, "index.js", nil)

	type problem struct {
		Check string `json:"check"`
	}
	type verdict struct {
		Title        string    `json:"title"`
		ViewportMeta bool      `json:"viewportMeta"`
		Problems     []problem `json:"problems"`
	}

	for _, shape := range []string{"value", "body"} {
		var v verdict
		f.json(t, "/?shape="+shape, &v)
		if v.Title != "Draft Preview" || !v.ViewportMeta || len(v.Problems) != 0 {
			t.Fatalf("shape %s unexpected clean verdict: %+v", shape, v)
		}

		var def verdict
		f.json(t, "/?shape="+shape+"&defective=true", &def)
		if def.Title != "" || def.ViewportMeta || len(def.Problems) != 3 {
			t.Fatalf("shape %s unexpected defective verdict: %+v", shape, def)
		}
		checks := map[string]bool{}
		for _, p := range def.Problems {
			checks[p.Check] = true
		}
		for _, expected := range []string{"title", "viewport", "background"} {
			if !checks[expected] {
				t.Fatalf("shape %s missing expected check %s: %+v", shape, expected, def)
			}
		}
	}
}

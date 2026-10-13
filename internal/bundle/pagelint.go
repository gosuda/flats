package bundle

import (
	"bytes"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strings"

	"golang.org/x/net/html"
)

// dataLoad finds calls that load data from an absolute URL literal while a
// page renders: the first argument of fetch, EventSource, WebSocket, the
// d3-fetch style loaders (d3.csv, d3.json, ...) and Papa.parse, and the
// second argument of XMLHttpRequest.open. The URL is the first or second
// capture group. Dynamic URLs (variables, template expressions) and code
// loading (import) are not detected.
var dataLoad = regexp.MustCompile(
	`(?:\bfetch|\bnew\s+(?:EventSource|WebSocket)|\.(?:json|csv|tsv|text|xml)|\bPapa\.parse)\s*\(\s*` + dataURL +
		`|\.open\s*\(\s*["'][A-Za-z]+["']\s*,\s*` + dataURL)

// dataURL is a quoted absolute http(s)/ws(s) or protocol-relative URL.
const dataURL = "[\"'`]((?:(?:https?|wss?):)?//[^\"'`\\s]+)"

// dataScriptTypes are <script> types that hold data or code maps rather than
// code the page runs.
var dataScriptTypes = map[string]bool{"importmap": true, "application/json": true, "application/ld+json": true, "text/plain": true, "text/template": true}

// externalData warns, for a page, about data loaded from another origin
// while it renders: a published version then no longer fixes what readers
// see, and the page breaks offline. It scans the entry's inline scripts and
// the bundled JavaScript files, one warning per file and origin.
func (l *linter) externalData(files []File, entry string) {
	for _, f := range files {
		switch {
		case f.Path == entry:
			seen := map[string]bool{}
			for _, script := range inlineScripts(f.Data) {
				l.dataLoads(f.Path, script, seen)
			}
		case isScript(f.Path):
			l.dataLoads(f.Path, f.Data, map[string]bool{})
		}
	}
}

func (l *linter) dataLoads(file string, src []byte, seen map[string]bool) {
	for _, m := range dataLoad.FindAllSubmatch(src, -1) {
		raw := string(m[1])
		if raw == "" {
			raw = string(m[2])
		}
		if strings.HasPrefix(raw, "//") {
			raw = "https:" + raw
		}
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			continue
		}
		origin := strings.ToLower(u.Scheme + "://" + u.Host)
		if seen[origin] {
			continue
		}
		seen[origin] = true
		l.add(file, fmt.Sprintf("page loads data from %s while it renders, so a published version no longer fixes what readers see, and the page breaks offline", origin),
			"bundle the data file (e.g. data/sales.csv) and load it by a relative path, or embed it in the HTML")
	}
}

// inlineScripts returns the text of the scripts without src that a browser
// runs; data blocks and scripts inside <template> are skipped.
func inlineScripts(data []byte) [][]byte {
	var out [][]byte
	z := html.NewTokenizer(bytes.NewReader(data))
	inScript, template := false, 0
	for {
		switch z.Next() {
		case html.ErrorToken:
			return out
		case html.TextToken:
			if inScript {
				out = append(out, bytes.Clone(z.Text()))
			}
		case html.EndTagToken:
			inScript = false
			if name, _ := z.TagName(); string(name) == "template" && template > 0 {
				template--
			}
		case html.StartTagToken:
			inScript = false
			name, hasAttr := z.TagName()
			switch string(name) {
			case "template":
				template++
			case "script":
				src, typ := false, ""
				for hasAttr {
					var k, v []byte
					k, v, hasAttr = z.TagAttr()
					switch string(k) {
					case "src":
						src = true
					case "type":
						typ = strings.ToLower(strings.TrimSpace(string(v)))
					}
				}
				inScript = template == 0 && !src && !dataScriptTypes[typ]
			}
		}
	}
}

func isScript(p string) bool {
	switch strings.ToLower(path.Ext(p)) {
	case ".js", ".mjs":
		return true
	}
	return false
}

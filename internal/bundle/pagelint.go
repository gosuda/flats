package bundle

import (
	"bytes"
	"fmt"
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

// dataLoads matches one call at a time, so a file with many calls never
// holds all matches at once. A repeated origin costs no further allocation,
// and the scan stops at maxSeen distinct origins.
func (l *linter) dataLoads(file string, src []byte, seen map[string]bool) {
	var key []byte
	for pos := 0; pos < len(src) && len(seen) < maxSeen; {
		m := dataLoad.FindSubmatchIndex(src[pos:])
		if m == nil {
			return
		}
		group := 2
		if m[2] < 0 {
			group = 4
		}
		raw := src[pos+m[group] : pos+m[group+1]]
		pos += m[1]
		if key = originKey(key[:0], raw); key == nil || seen[string(key)] {
			continue
		}
		origin := string(key)
		seen[origin] = true
		l.add(file, fmt.Sprintf("page loads data from %s while it renders, so a published version no longer fixes what readers see, and the page breaks offline", origin),
			"bundle the data file (e.g. data/sales.csv) and load it by a relative path, or embed it in the HTML")
	}
}

// originKey appends the lowercase "scheme://host" of a URL that dataLoad
// matched (absolute, or protocol-relative and read as https) to dst. It
// returns nil when the URL has no host.
func originKey(dst, raw []byte) []byte {
	i := bytes.Index(raw, []byte("//"))
	host := raw[i+2:]
	if j := bytes.IndexAny(host, "/?#"); j >= 0 {
		host = host[:j]
	}
	// Never repeat credentials ("user:pass@") in a warning; the origin is
	// what follows them.
	if j := bytes.LastIndexByte(host, '@'); j >= 0 {
		host = host[j+1:]
	}
	if len(host) == 0 {
		return nil
	}
	if i == 0 {
		dst = append(dst, "https:"...)
	} else {
		dst = appendLower(dst, raw[:i])
	}
	dst = append(dst, "//"...)
	return appendLower(dst, host)
}

func appendLower(dst, b []byte) []byte {
	for _, c := range b {
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		dst = append(dst, c)
	}
	return dst
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

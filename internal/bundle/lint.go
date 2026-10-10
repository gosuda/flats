package bundle

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/html"

	"github.com/gosuda/flats/internal/contenttype"
)

// MaxImageBytes is the image size above which Lint suggests compressing.
const MaxImageBytes = 1 << 20

// maxWarnings bounds the warnings of one save so a large site with one
// systematic mistake does not flood the agent's context.
const maxWarnings = 50

// Lint reports non-blocking page-quality warnings for a valid flat bundle:
// HTML documents without a title or viewport, external scripts and styles
// without an exact version, references to files missing from the bundle,
// large images, and a missing favicon or console thumbnail. It never fails:
// docs bundles and bundles that do not validate return nil. The checks are
// heuristics over tokens, not a browser render (guide topic.design).
func Lint(files []File) []Problem {
	m, err := ParseManifest(files)
	if err != nil || m.Type == contenttype.Docs {
		return nil
	}
	// Manifest paths may be written as "./404.html"; files never are.
	canonical := func(p string) string {
		c, _ := cleanPath(p)
		return c
	}
	m.Entry = canonical(m.Entry)
	l := linter{index: make(map[string]bool, len(files)), static: m.Kind == "static", spa: m.SPA, notFound: canonical(m.NotFound)}
	for _, f := range files {
		l.index[f.Path] = true
	}
	htmlEntry := l.static && isHTML(m.Entry)
	if htmlEntry && m.Screenshot == "" {
		l.add(ManifestName, "flats.json has no screenshot, so the console shows no thumbnail for this flat",
			`capture the page at desktop width, add it as e.g. screenshot.png and set "screenshot": "screenshot.png" in flats.json`)
	}
	for _, f := range files {
		switch {
		case isHTML(f.Path):
			entry := l.static && f.Path == m.Entry
			if entry || looksLikeDocument(f.Data) {
				l.document(f, entry)
			}
		case isImage(f.Path) && len(f.Data) > MaxImageBytes:
			l.add(f.Path, fmt.Sprintf("image is %.1f MiB; pages load it on every visit", float64(len(f.Data))/(1<<20)),
				"resize it to the largest size it is displayed at and re-encode it (WebP or AVIF for photos), ideally below 500 KiB")
		}
	}
	if more := l.total - len(l.out); more > 0 {
		l.out = append(l.out, Problem{Message: fmt.Sprintf("%d more warnings not shown", more),
			Fix: "fix the warnings above and save again to see the rest"})
	}
	return l.out
}

type linter struct {
	index    map[string]bool
	static   bool
	spa      bool
	notFound string
	out      []Problem // at most maxWarnings
	total    int       // every warning found, including those not kept
}

func (l *linter) add(p, msg, fix string) {
	l.keep(Problem{Path: p, Message: msg, Fix: fix})
}

func (l *linter) keep(w Problem) {
	l.total++
	if len(l.out) < maxWarnings {
		l.out = append(l.out, w)
	}
}

// maxSeen bounds the per-document set of references already checked; past
// it, a repeated reference may be counted twice.
const maxSeen = 4096

// document checks one HTML document. Only static flats get reference
// checks: a server handler owns its routes, so a path need not be a file.
func (l *linter) document(f File, entry bool) {
	var (
		title, viewport, icon bool
		titleText             strings.Builder
		inTitle, inImportMap  bool
		// inert is the depth inside <svg> or <math>, whose <title> is not
		// the page's, and <template>, whose content the browser ignores.
		inert int
		// Reference warnings are checked while scanning, so memory stays
		// bounded however many references a page has; they are reported
		// after the page-level warnings.
		refWarn []Problem
		refMore int
		seen    = map[ref]bool{}
	)
	// A <base href> changes how every reference resolves: skip the file
	// checks, and resolve code URLs against an external base.
	base, hasBase := baseHref(f.Data)
	checkFiles := l.static && !hasBase
	check := func(r ref) {
		r.url = strings.TrimSpace(r.url)
		if r.url == "" || seen[r] {
			return
		}
		if len(seen) < maxSeen {
			seen[r] = true
		}
		code := r
		if base != nil {
			if u, err := url.Parse(r.url); err == nil {
				code.url = base.ResolveReference(u).String()
			}
		}
		for _, w := range []Problem{l.external(f.Path, code), l.missing(f.Path, r, entry, checkFiles)} {
			switch {
			case w.Message == "":
			case len(refWarn) < maxWarnings:
				refWarn = append(refWarn, w)
			default:
				refMore++
			}
		}
	}
	z := html.NewTokenizer(bytes.NewReader(f.Data))
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			break
		}
		switch tt {
		case html.TextToken:
			if inTitle {
				titleText.Write(z.Text())
			}
			if inImportMap {
				for _, r := range importMapRefs(z.Text()) {
					check(r)
				}
			}
			continue
		case html.EndTagToken:
			inTitle, inImportMap = false, false
			if name, _ := z.TagName(); inert > 0 && inertTags[string(name)] {
				inert--
			}
			continue
		case html.StartTagToken, html.SelfClosingTagToken:
		default:
			continue
		}
		inTitle, inImportMap = false, false
		name, hasAttr := z.TagName()
		attrs := map[string]string{}
		for hasAttr {
			var k, v []byte
			k, v, hasAttr = z.TagAttr()
			if _, dup := attrs[string(k)]; !dup {
				attrs[string(k)] = string(v)
			}
		}
		rel := relTokens(attrs["rel"])
		switch tag := string(name); tag {
		case "svg", "math", "template":
			if tt == html.StartTagToken {
				inert++
			}
		case "title":
			// The browser shows the first title only.
			if inert == 0 && !title {
				title, inTitle = true, tt == html.StartTagToken
			}
		case "meta":
			viewport = viewport || (inert == 0 && strings.EqualFold(strings.TrimSpace(attrs["name"]), "viewport"))
		case "script":
			if strings.EqualFold(strings.TrimSpace(attrs["type"]), "importmap") {
				inImportMap = tt == html.StartTagToken
			}
			check(ref{url: attrs["src"], kind: codeAsset})
		case "link":
			icon = icon || (inert == 0 && rel["icon"] && strings.TrimSpace(attrs["href"]) != "")
			kind := plainAsset
			as := strings.ToLower(strings.TrimSpace(attrs["as"]))
			if rel["stylesheet"] || rel["modulepreload"] || (rel["preload"] && (as == "script" || as == "style")) {
				kind = codeAsset
			}
			if rel["stylesheet"] || rel["icon"] || rel["apple-touch-icon"] || rel["manifest"] || rel["preload"] || rel["modulepreload"] || rel["mask-icon"] {
				check(ref{url: attrs["href"], kind: kind})
			}
		case "iframe":
			check(ref{url: attrs["src"], kind: pageRef})
		case "img", "source", "video", "audio", "track", "embed", "input":
			if tag != "input" || strings.EqualFold(attrs["type"], "image") {
				check(ref{url: attrs["src"], kind: plainAsset})
			}
			for _, u := range srcset(attrs["srcset"]) {
				check(ref{url: u, kind: plainAsset})
			}
			if tag == "video" {
				check(ref{url: attrs["poster"], kind: plainAsset})
			}
		case "object":
			check(ref{url: attrs["data"], kind: plainAsset})
		}
	}
	switch t := strings.Join(strings.Fields(titleText.String()), " "); {
	case !title || t == "":
		l.add(f.Path, "page has no <title>, so browser tabs, history and shared links show the raw address",
			"add <title> with a short noun phrase that names the page, e.g. <title>Team Lunch Poll</title>")
	case placeholderTitles[strings.ToLower(t)]:
		l.add(f.Path, fmt.Sprintf("<title> %q is a template placeholder", t),
			"replace it with a short noun phrase that names this page")
	}
	if !viewport {
		l.add(f.Path, "page has no viewport meta tag, so phones render it as a zoomed-out desktop page",
			`add <meta name="viewport" content="width=device-width, initial-scale=1"> to <head>`)
	}
	if entry && !icon && !l.index["favicon.ico"] {
		l.add(f.Path, "page has no favicon, so browsers request /favicon.ico and get 404",
			`add <link rel="icon" href="favicon.svg"> with the icon file in the bundle, or add favicon.ico at the root`)
	}
	for _, w := range refWarn {
		l.keep(w)
	}
	l.total += refMore
}

var baseTag = regexp.MustCompile(`(?i)<base[\s/>]`)

// baseHref finds the first <base> element with an href. found reports
// whether there is one; ext is its URL when it points at another origin.
func baseHref(data []byte) (ext *url.URL, found bool) {
	if !baseTag.Match(data) {
		return nil, false
	}
	z := html.NewTokenizer(bytes.NewReader(data))
	for {
		switch z.Next() {
		case html.ErrorToken:
			return nil, false
		case html.StartTagToken, html.SelfClosingTagToken:
			name, hasAttr := z.TagName()
			for string(name) == "base" && hasAttr {
				var k, v []byte
				k, v, hasAttr = z.TagAttr()
				if href := strings.TrimSpace(string(v)); string(k) == "href" && href != "" {
					u, _ := externalURL(href)
					return u, true
				}
			}
		}
	}
}

var inertTags = map[string]bool{"svg": true, "math": true, "template": true}

type assetKind int

const (
	plainAsset assetKind = iota // image, icon, media: only checked for existence
	codeAsset                   // script or stylesheet: also checked for a pinned version
	pageRef                     // a frame showing a page: an SPA serves its entry there
)

type ref struct {
	url    string
	kind   assetKind
	prefix bool // an import-map prefix ("lib/"), not a fetched file
}

// placeholderTitles are scaffold defaults that do not name a page.
var placeholderTitles = map[string]bool{
	"document": true, "untitled": true, "untitled document": true, "index": true, "home page": true,
	"vite app": true, "vite + react": true, "vite + react + ts": true, "vite + vue": true, "vite + vue + ts": true,
	"vite + svelte": true, "vite + svelte + ts": true, "react app": true, "my app": true, "app": true,
	"svelte app": true, "vue app": true, "next.js app": true, "create next app": true,
}

// fontCSSHosts serve generated font stylesheets that have no version to pin.
var fontCSSHosts = map[string]bool{"fonts.googleapis.com": true, "fonts.bunny.net": true, "use.typekit.net": true}

// external warns when a script or stylesheet loads from another origin
// without an exact version (bundle by default; CDN only when pinned).
func (l *linter) external(page string, r ref) Problem {
	raw := r.url
	u, ok := externalURL(raw)
	if r.kind != codeAsset || !ok {
		return Problem{}
	}
	if fontCSSHosts[strings.ToLower(u.Hostname())] {
		return warn(page, fmt.Sprintf("font stylesheet loads from %s on every visit and cannot be pinned to a version", u.Hostname()),
			"bundle the font files (.woff2) and declare them with @font-face, so the page also works offline and without third-party requests")
	}
	pinned, tag := exactVersion(strings.ToLower(u.Hostname()), u.EscapedPath())
	if pinned {
		return Problem{}
	}
	what := "has no version"
	if tag != "" {
		what = fmt.Sprintf("uses version %q, which is not exact and can change under you", short(tag))
	}
	return warn(page, fmt.Sprintf("external script or stylesheet %s %s", short(raw), what),
		"bundle the file into the upload (preferred), or pin an exact version such as @18.3.1 and add an integrity hash")
}

func warn(p, msg, fix string) Problem { return Problem{Path: p, Message: msg, Fix: fix} }

// externalURL parses an absolute http(s) or protocol-relative URL.
func externalURL(raw string) (*url.URL, bool) {
	if strings.HasPrefix(raw, "//") {
		raw = "https:" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return nil, false
	}
	if s := strings.ToLower(u.Scheme); s != "http" && s != "https" {
		return nil, false
	}
	return u, true
}

var (
	semver      = regexp.MustCompile(`^v?\d+\.\d+\.\d+(?:[-+][0-9A-Za-z.+-]+)?$`)
	semverInSeg = regexp.MustCompile(`(?:^|[-_.@])v?\d+\.\d+\.\d+(?:$|[-_.+])`)
)

// exactVersion reports whether a CDN URL names an exact version. On npm
// package CDNs (jsDelivr /npm/ and /gh/, unpkg, esm.sh, Skypack, JSPM) the
// package segment itself needs "@version": a versioned file name inside an
// unversioned package still follows the latest release. Elsewhere an
// npm-style "pkg@version" marker decides on its own; without one, a path
// segment such as /18.3.1/ or a file name such as jquery-3.7.1.min.js
// counts as pinned. @18.3.1 passes, while @latest, @18, @^18 or @~18.3.1
// fail and are returned as tag.
func exactVersion(host, escapedPath string) (pinned bool, tag string) {
	p, err := url.PathUnescape(escapedPath)
	if err != nil {
		p = escapedPath
	}
	if pkg, ok := npmPackage(host, p); ok {
		i := strings.LastIndex(pkg, "@")
		if i <= 0 {
			return false, ""
		}
		v := pkg[i+1:]
		if semver.MatchString(v) {
			return true, ""
		}
		return false, v
	}
	marker := false
	for _, seg := range strings.Split(p, "/") {
		i := strings.LastIndex(seg, "@")
		if i <= 0 { // no marker, or the "@scope" of a scoped package
			continue
		}
		marker = true
		if v := seg[i+1:]; !semver.MatchString(v) {
			return false, v
		}
	}
	if marker {
		return true, ""
	}
	for _, seg := range strings.Split(p, "/") {
		if semver.MatchString(seg) || semverInSeg.MatchString(seg) {
			return true, ""
		}
	}
	return false, ""
}

var esmBuild = regexp.MustCompile(`^v\d+$`)

// npmPackage returns the package segment ("react@18.3.1", "repo@v1") of
// a URL path on an npm package CDN.
func npmPackage(host, p string) (string, bool) {
	segs := strings.Split(strings.TrimPrefix(p, "/"), "/")
	switch host {
	case "cdn.jsdelivr.net", "fastly.jsdelivr.net", "gcore.jsdelivr.net":
		if len(segs) >= 3 && segs[0] == "gh" {
			return segs[2], true // gh/<user>/<repo>@<version>
		}
		if segs[0] != "npm" {
			return "", false
		}
		segs = segs[1:]
	case "ga.jspm.io":
		return segs[0], strings.HasPrefix(segs[0], "npm:") // npm:react@18.3.1
	case "esm.sh":
		for len(segs) > 1 && (segs[0] == "stable" || esmBuild.MatchString(segs[0])) {
			segs = segs[1:] // build prefixes such as /v135/
		}
	case "unpkg.com", "cdn.skypack.dev":
	default:
		return "", false
	}
	if len(segs) >= 2 && strings.HasPrefix(segs[0], "@") {
		return segs[1], true // @scope/pkg@version
	}
	return segs[0], len(segs) > 0 && segs[0] != ""
}

// missing warns when a relative reference names no file in the bundle,
// resolving it the way the static server does. Visitors open the entry at
// the flat root, so its relative references resolve from there.
func (l *linter) missing(page string, r ref, entry, enabled bool) Problem {
	raw := r.url
	lower := strings.ToLower(raw)
	if !enabled || r.prefix || strings.HasPrefix(raw, "#") || strings.HasPrefix(raw, "//") {
		return Problem{}
	}
	if u, err := url.Parse(raw); err != nil || u.Scheme != "" || strings.HasPrefix(lower, "data:") {
		return Problem{}
	}
	target := raw
	if i := strings.IndexAny(target, "?#"); i >= 0 {
		target = target[:i]
	}
	if target == "" {
		return Problem{}
	}
	if dec, err := url.PathUnescape(target); err == nil {
		target = dec
	}
	dir := strings.HasSuffix(target, "/")
	resolve := func(from string) string {
		if strings.HasPrefix(target, "/") {
			from = "/"
		}
		return strings.TrimPrefix(path.Clean("/"+path.Join(from, target)), "/")
	}
	from := path.Dir(page)
	if entry {
		from = "/"
	}
	rel := resolve(from)
	// A single-page app serves its entry HTML for unknown extensionless
	// paths: fine for a frame, but not as a script, stylesheet or image.
	spaFrame := l.spa && r.kind == pageRef && !strings.Contains(path.Base(rel), ".")
	if !l.resolves(rel, dir, r.kind == pageRef) && !spaFrame {
		return warn(page, fmt.Sprintf("references %s, but the bundle has no file %s", short(raw), short(displayPath(rel))),
			"add the file to the upload, or fix the path (paths are relative to the page; a leading / starts at the flat root)")
	}
	// A single-page app's entry and the 404 page are also served at nested
	// URLs such as /a/b, where a relative path resolves under /a/.
	if (entry && l.spa || page == l.notFound) && !strings.HasPrefix(target, "/") {
		return warn(page, fmt.Sprintf("relative reference %s breaks where this page is served at a nested URL (an app route or a 404 such as /a/b)", short(raw)),
			fmt.Sprintf("use a root-relative path such as /%s", short(rel)))
	}
	return Problem{}
}

// maxShown bounds how much of a reference a warning repeats.
const maxShown = 200

// short truncates s for display, keeping UTF-8 intact.
func short(s string) string {
	if len(s) <= maxShown {
		return s
	}
	cut := maxShown
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// resolves reports whether the static server answers rel with a file. Its
// HTML fallbacks (the entry at the root, dir/index.html, pretty .html URLs)
// count only for page references: served as a script, stylesheet or image,
// HTML is refused by the browser.
func (l *linter) resolves(rel string, dir, page bool) bool {
	if rel == ManifestName {
		return false // flats.json is never served
	}
	if !dir && rel != "" && rel != "." && l.index[rel] {
		return true
	}
	if !page {
		return false
	}
	if rel == "" || rel == "." || l.index[path.Join(rel, "index.html")] {
		return true
	}
	return !dir && !strings.Contains(path.Base(rel), ".") && l.index[rel+".html"]
}

func displayPath(rel string) string {
	if rel == "" {
		return "/"
	}
	return rel
}

// importMapRefs returns the URLs of an import map's imports and scopes.
func importMapRefs(data []byte) []ref {
	var m struct {
		Imports map[string]string            `json:"imports"`
		Scopes  map[string]map[string]string `json:"scopes"`
	}
	if json.Unmarshal(data, &m) != nil {
		return nil
	}
	var out []ref
	add := func(specifier, v string) {
		out = append(out, ref{url: v, kind: codeAsset, prefix: strings.HasSuffix(specifier, "/")})
	}
	for k, v := range m.Imports {
		add(k, v)
	}
	for _, s := range m.Scopes {
		for k, v := range s {
			add(k, v)
		}
	}
	return out
}

// srcset returns the URLs of a srcset attribute ("a.png 1x, b.png 2x"),
// following the HTML candidate parsing rules: a URL runs to whitespace, so a
// data: URL keeps its commas, and descriptors end at a comma outside
// parentheses.
func srcset(v string) []string {
	var out []string
	space := func(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' }
	for i := 0; i < len(v); {
		for i < len(v) && (space(v[i]) || v[i] == ',') {
			i++
		}
		start := i
		for i < len(v) && !space(v[i]) {
			i++
		}
		u := v[start:i]
		if u == "" {
			break
		}
		if trimmed := strings.TrimRight(u, ","); trimmed != u {
			out = append(out, trimmed) // "a.png," ends the candidate
			continue
		}
		out = append(out, u)
		for depth := 0; i < len(v); i++ {
			switch v[i] {
			case '(':
				depth++
			case ')':
				depth--
			}
			if v[i] == ',' && depth <= 0 {
				i++
				break
			}
		}
	}
	return out
}

func relTokens(v string) map[string]bool {
	out := map[string]bool{}
	for _, t := range strings.Fields(strings.ToLower(v)) {
		out[t] = true
	}
	return out
}

// looksLikeDocument tells full pages from HTML fragments (templates,
// partials), which have no head to put a title or viewport in.
func looksLikeDocument(data []byte) bool {
	head := data
	if len(head) > 4096 {
		head = head[:4096]
	}
	lower := bytes.ToLower(head)
	return bytes.Contains(lower, []byte("<!doctype html")) || bytes.Contains(lower, []byte("<html")) || bytes.Contains(lower, []byte("<head"))
}

func isHTML(p string) bool {
	ext := strings.ToLower(path.Ext(p))
	return ext == ".html" || ext == ".htm"
}

func isImage(p string) bool {
	switch strings.ToLower(path.Ext(p)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".avif", ".bmp", ".tif", ".tiff", ".svg", ".ico", ".heic":
		return true
	}
	return false
}

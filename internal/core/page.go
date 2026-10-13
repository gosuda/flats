package core

import (
	"context"
	"encoding/json"
	"html"
	"strings"

	"github.com/gosuda/flats/internal/bundle"
	"github.com/gosuda/flats/internal/contenttype"
)

// pageStarter is the first Draft of a page created by CreatePage: a complete
// small page that meets the page contract (title, viewport, both color
// schemes), for the agent to replace with its own.
const pageStarter = `<!doctype html>
<html>
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{title}}</title>
<style>
:root { --bg: #f7f7f4; --fg: #1d1f1c; --muted: #5d625a; color-scheme: light }
@media (prefers-color-scheme: dark) {
  :root { --bg: #151714; --fg: #e8eae4; --muted: #a2a89c; color-scheme: dark }
}
body { margin: 0; padding: 48px max(16px, calc((100% - 720px) / 2)); background: var(--bg); color: var(--fg); font: 17px/1.6 system-ui, sans-serif }
p { color: var(--muted) }
</style>
</head>
<body>
<h1>{{title}}</h1>
<p>This page has no content yet.</p>
</body>
</html>
`

// CreatePage creates a page flat with a starter Draft (revision 1). Nothing
// is published: a page follows the website lifecycle. An existing slug is
// refused.
func (s *Service) CreatePage(ctx context.Context, slugName, name string, via Via) (FlatView, error) {
	if _, err := s.CreateFlat(ctx, slugName, name, via); err != nil {
		return FlatView{}, err
	}
	if err := s.savePageStarter(ctx, slugName, name, via); err != nil {
		return FlatView{}, err
	}
	return s.GetFlat(ctx, slugName)
}

// savePageStarter saves the starter Draft only while the flat has no Draft:
// a save that reached the new flat first is kept, and creation reports a
// conflict instead of replacing it.
func (s *Service) savePageStarter(ctx context.Context, slugName, name string, via Via) error {
	title := strings.Join(strings.Fields(name), " ")
	if title == "" {
		title = slugName
	}
	manifest, err := json.Marshal(map[string]string{"type": contenttype.Page, "name": title})
	if err != nil {
		return err
	}
	page := strings.ReplaceAll(pageStarter, "{{title}}", html.EscapeString(title))
	files := []bundle.File{{Path: "index.html", Data: []byte(page)}, {Path: bundle.ManifestName, Data: manifest}}
	_, err = s.SaveVersion(ctx, slugName, files, SaveMeta{Message: "created", CheckRevision: true, ExpectedRevision: 0}, via)
	return err
}

package notebook

import (
	"context"
	"fmt"
	"slices"
)

// PageChanges compares per-page file identities, not only page headers: row,
// schema, and block changes belong to the page that owns their content file.
func PageChanges(entry OutboxEntry, prev []File, read func(context.Context, string) ([]byte, error)) ([]PageChange, error) {
	type pageFiles struct {
		files  map[string]File
		header File
	}
	index := func(files []File) (map[ID]*pageFiles, error) {
		pages := map[ID]*pageFiles{}
		for _, f := range files {
			p, err := parseRevisionPath(f.Path)
			if err != nil {
				return nil, err
			}
			if p.page == "" {
				continue
			}
			if pages[p.page] == nil {
				pages[p.page] = &pageFiles{files: map[string]File{}}
			}
			if _, exists := pages[p.page].files[f.Path]; exists {
				return nil, fmt.Errorf("duplicate page file")
			}
			pages[p.page].files[f.Path] = f
			if p.kind == "page" {
				pages[p.page].header = f
			}
		}
		for _, p := range pages {
			if p.header.Path == "" {
				return nil, fmt.Errorf("page content without header")
			}
		}
		return pages, nil
	}
	before, err := index(prev)
	if err != nil {
		return nil, err
	}
	after, err := index(entry.Files)
	if err != nil {
		return nil, err
	}
	ids := map[ID]bool{}
	for id := range before {
		ids[id] = true
	}
	for id := range after {
		ids[id] = true
	}
	sorted := make([]ID, 0, len(ids))
	for id := range ids {
		sorted = append(sorted, id)
	}
	slices.Sort(sorted)
	result := []PageChange{}
	for _, id := range sorted {
		old, new := before[id], after[id]
		change := "changed"
		if old == nil {
			change = "added"
		} else if new == nil {
			change = "removed"
		} else {
			same := len(old.files) == len(new.files)
			for path, f := range old.files {
				if new.files[path] != f {
					same = false
					break
				}
			}
			if same {
				continue
			}
		}
		header := old
		if new != nil {
			header = new
		}
		data, err := read(context.Background(), header.header.Hash)
		if err != nil {
			return nil, err
		}
		if int64(len(data)) != header.header.Size || contentHash(data) != header.header.Hash {
			return nil, fmt.Errorf("page summary integrity failure")
		}
		var value struct {
			ID       ID     `json:"id"`
			Title    string `json:"title"`
			Parent   *ID    `json:"parent"`
			Position string `json:"position"`
		}
		if err := strictDecode(data, &value); err != nil {
			return nil, err
		}
		if value.ID != id || value.ID.ValidateKind(PageKind) != nil {
			return nil, fmt.Errorf("page summary identity mismatch")
		}
		result = append(result, PageChange{Page: id, Title: value.Title, Change: change})
	}
	// Preserve canonical title text; no normalization or inferred authorship.
	return result, nil
}

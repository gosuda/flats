// Package contenttype names the content adapters supported by the host.
package contenttype

import "encoding/json"

const (
	Flat   = "flat"
	Docs   = "docs"
	Page   = "page"
	Design = "design"
	Board  = "board"
)

type Description struct {
	Type        string `json:"type"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

func Types() []Description {
	return []Description{{Flat, "Website", "Static or server website"}, {Docs, "Document", "Markdown documents and assets in the embedded collaborative editor"},
		{Page, "Page", "Static HTML page: a document with charts, graphs and visualizations"},
		{Design, "Design", "Canvas of HTML artboards that people and agents edit live"},
		{Board, "Board", "Infinite-canvas whiteboard of stickies, shapes and connectors that people and agents edit live"}}
}

// RevisionModel reports whether content of type t follows the Revision
// model: no Draft, versions or publish; changes are live at once.
func RevisionModel(t string) bool { return t == Docs || t == Design || t == Board }

// FromManifest derives the type without changing the metadata schema.
func FromManifest(raw json.RawMessage) string {
	var m struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(raw, &m)
	if m.Type == "" {
		return Flat
	}
	return m.Type
}

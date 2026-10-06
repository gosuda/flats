// Package contenttype names the content adapters supported by the host.
package contenttype

import "encoding/json"

const (
	Flat = "flat"
	Docs = "docs"
)

type Description struct {
	Type        string `json:"type"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

func Types() []Description {
	return []Description{{Flat, "Website", "Static or server website"}, {Docs, "Document", "Markdown documents and assets in the embedded collaborative editor"}}
}

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

package store

import (
	"encoding/json"
	"github.com/gosuda/flats/internal/contenttype"
)

// MarshalJSON derives type from immutable manifest metadata, including legacy rows.
func (v Version) MarshalJSON() ([]byte, error) {
	type version Version
	return json.Marshal(struct {
		version
		Type string `json:"type"`
	}{version(v), contenttype.FromManifest(v.Manifest)})
}

func (d Draft) MarshalJSON() ([]byte, error) {
	type draft Draft
	return json.Marshal(struct {
		draft
		Type string `json:"type"`
	}{draft(d), contenttype.FromManifest(d.Manifest)})
}

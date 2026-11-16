package webhook

import (
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"strings"
	"time"
)

// maxBody is the largest body of an event in bytes. A body above it is
// dropped, which valid input never reaches.
const maxBody = 4096

// atLayout is RFC 3339 in UTC with exactly three fraction digits. A layout
// with ".000" keeps the digits when the milliseconds are zero.
const atLayout = "2006-01-02T15:04:05.000Z"

// Payload is the body of an event. The sender adds no field to it, so a
// receiver sees exactly the fields that the event specification lists.
type Payload struct {
	// ID is filled by Notify when empty.
	ID string `json:"id"`
	// Type is approval.created, approval.decided, visibility.changed or
	// network.changed.
	Type string `json:"type"`
	// At is filled by Notify from Config.Now when empty.
	At         string  `json:"at"`
	Flat       FlatRef `json:"flat"`
	Operation  string  `json:"operation"`
	State      string  `json:"state"`
	ApprovalID string  `json:"approval_id,omitempty"`
	Via        string  `json:"via,omitempty"`
	// DecidedBy is "operator" or "autopilot".
	DecidedBy  string  `json:"decided_by,omitempty"`
	Change     *Change `json:"change,omitempty"`
	ConsoleURL string  `json:"console_url"`
}

// FlatRef names the flat of an event. ID is always present and is empty when
// the flat is not known.
type FlatRef struct {
	Slug string `json:"slug"`
	ID   string `json:"id"`
}

// Access is a visibility together with its public access level.
type Access struct {
	Visibility string `json:"visibility"`
	Access     string `json:"access"`
}

// Change describes what a visibility.changed or network.changed event
// changed. Only the fields of its kind are written: From and To for a
// visibility change, Grant "origins" with Origins, or Grant "provider" with
// Provider and Permitted.
type Change struct {
	// From and To are the access before and after a visibility change.
	From, To *Access
	// Grant is "origins" or "provider" for a network change and empty for a
	// visibility change.
	Grant string
	// Origins is the list after the change. It is written as [] when empty.
	Origins []string
	// Provider is the provider of a "provider" grant.
	Provider string
	// Permitted is whether the provider is permitted. It is always written
	// for a "provider" grant.
	Permitted bool
}

// MarshalJSON writes only the fields that belong to the kind of change, so a
// receiver never sees a field that is empty only because of the other kind.
func (c Change) MarshalJSON() ([]byte, error) {
	switch c.Grant {
	case "origins":
		origins := c.Origins
		if origins == nil {
			origins = []string{}
		}
		return json.Marshal(struct {
			Grant   string   `json:"grant"`
			Origins []string `json:"origins"`
		}{c.Grant, origins})
	case "provider":
		return json.Marshal(struct {
			Grant     string `json:"grant"`
			Provider  string `json:"provider"`
			Permitted bool   `json:"permitted"`
		}{c.Grant, c.Provider, c.Permitted})
	}
	return json.Marshal(struct {
		From *Access `json:"from,omitempty"`
		To   *Access `json:"to,omitempty"`
	}{c.From, c.To})
}

// idEncoding is built once because WithPadding copies the alphabet tables.
var idEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// newID returns "evt_" and 26 random lower-case base32 characters, which is
// the base32 text of 16 random bytes without padding.
func newID() string {
	var raw [16]byte
	// Since Go 1.24 Read never fails: it ends the program when the system
	// source of randomness is unusable.
	_, _ = rand.Read(raw[:])
	return "evt_" + strings.ToLower(idEncoding.EncodeToString(raw[:]))
}

// formatAt writes a time in the form of the At field.
func formatAt(t time.Time) string {
	return t.UTC().Format(atLayout)
}

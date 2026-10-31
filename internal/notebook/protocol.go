package notebook

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

const (
	liveClientFrame     = 256 << 10
	liveServerFrame     = 512 << 10
	liveFragment        = 64 << 10
	livePrivateQueue    = 4 << 20
	livePublicQueue     = 1 << 20
	liveAssemblyMax     = 8 << 20
	liveHostAssemblyMax = 64 << 20
)

var liveAssemblyBytes atomic.Int64

// liveScope is the closed wire union; Scope is the snapshot reader's selection.
type liveScope struct {
	Kind string `json:"kind"`
	ID   ID     `json:"id,omitempty"`
}

func (s liveScope) selection(public bool) (Scope, error) {
	scope := Scope{Public: public}
	switch s.Kind {
	case "tree":
		if s.ID != "" {
			return scope, invalidRead("scope")
		}
		scope.Tree = true
	case "page":
		if err := s.ID.ValidateKind(PageKind); err != nil {
			return scope, err
		}
		scope.Page = s.ID
	case "database":
		if err := s.ID.ValidateKind(DatabaseKind); err != nil {
			return scope, err
		}
		scope.Database = s.ID
	default:
		return scope, invalidRead("scope")
	}
	return scope, nil
}

type liveHello struct {
	Type      string    `json:"type"`
	Version   int       `json:"version"`
	Session   string    `json:"session,omitempty"`
	Watermark *Sequence `json:"watermark,omitempty"`
	Pending   struct {
		Count int       `json:"count"`
		First *Sequence `json:"first_n,omitempty"`
	} `json:"pending"`
	Name          string      `json:"name,omitempty"`
	Page          ID          `json:"page,omitempty"`
	PendingScopes []liveScope `json:"pending_scopes,omitempty"`
	Spill         string      `json:"spill,omitempty"`
	NextWindow    int         `json:"next_window,omitempty"`
}

type liveWatch struct {
	Type       string      `json:"type"`
	Generation int         `json:"generation"`
	Page       ID          `json:"page"`
	Databases  []ID        `json:"visible_databases"`
	Pending    []liveScope `json:"pending_scopes"`
}

func (w liveWatch) scopes(public bool) ([]Scope, error) {
	if w.Generation < 1 || len(w.Databases)+len(w.Pending) > 128 {
		return nil, invalidRead("watch")
	}
	scopes := []Scope{{Tree: true, Public: public}}
	if w.Page != "" {
		if err := w.Page.ValidateKind(PageKind); err != nil {
			return nil, err
		}
		scopes = append(scopes, Scope{Page: w.Page, Public: public})
	}
	for _, id := range w.Databases {
		if err := id.ValidateKind(DatabaseKind); err != nil {
			return nil, err
		}
		scopes = append(scopes, Scope{Database: id, Public: public})
	}
	for _, s := range w.Pending {
		scope, err := s.selection(public)
		if err != nil {
			return nil, err
		}
		scopes = append(scopes, scope)
	}
	seen := map[Scope]bool{}
	result := scopes[:0]
	for _, s := range scopes {
		if !seen[s] {
			seen[s] = true
			result = append(result, s)
		}
	}
	return result, nil
}

func liveObject(data []byte) (string, map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := strictDecode(data, &fields); err != nil || fields == nil {
		return "", nil, invalidRead("frame")
	}
	var kind string
	if err := json.Unmarshal(fields["type"], &kind); err != nil {
		return "", nil, invalidRead("frame")
	}
	return kind, fields, nil
}

func liveEnvelope(kind string, value any) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	fields["type"], _ = json.Marshal(kind)
	return json.Marshal(fields)
}

func liveParts(data []byte, kind string, generation int, seq Sequence, send func([]byte) error) error {
	if len(data) <= liveServerFrame {
		return send(data)
	}
	// Even a fragment consisting entirely of escapable bytes fits its envelope.
	count := 0
	for at := 0; at < len(data); count++ {
		end := min(at+liveFragment, len(data))
		for end < len(data) && !utf8.RuneStart(data[end]) {
			end--
		}
		at = end
	}
	for part, at := 0, 0; at < len(data); part++ {
		end := min(at+liveFragment, len(data))
		for end < len(data) && !utf8.RuneStart(data[end]) {
			end--
		}
		raw, err := liveEnvelope(kind, map[string]any{"generation": generation, "seq": seq, "part": part, "parts": count, "data": string(data[at:end])})
		if err != nil {
			return err
		}
		if len(raw) > liveServerFrame {
			return fmt.Errorf("notebook frame exceeds limit")
		}
		if err := send(raw); err != nil {
			return err
		}
		at = end
	}
	return nil
}

type liveAssembly struct {
	n        Sequence
	parts    int
	next     int
	data     bytes.Buffer
	deadline time.Time
	charged  int64
}

func (a *liveAssembly) reset() { liveAssemblyBytes.Add(-a.charged); *a = liveAssembly{} }
func (a *liveAssembly) add(data []byte, now time.Time) ([]byte, error) {
	var p struct {
		Type  string   `json:"type"`
		N     Sequence `json:"n"`
		Part  int      `json:"part"`
		Parts int      `json:"parts"`
		Data  string   `json:"data"`
	}
	if err := strictDecode(data, &p); err != nil {
		return nil, err
	}
	if p.N == 0 || p.Parts < 1 || p.Parts > 32 || p.Part < 0 || p.Part >= p.Parts {
		return nil, invalidRead("action_part")
	}
	if a.parts == 0 {
		if p.Part != 0 {
			return nil, invalidRead("action_part")
		}
		a.n = p.N
		a.parts = p.Parts
		a.deadline = now.Add(30 * time.Second)
	}
	if !now.Before(a.deadline) || p.N != a.n || p.Parts != a.parts || p.Part != a.next || a.data.Len()+len(p.Data) > liveAssemblyMax {
		a.reset()
		return nil, invalidRead("action_part")
	}
	n := int64(len(p.Data))
	if liveAssemblyBytes.Add(n) > liveHostAssemblyMax {
		liveAssemblyBytes.Add(-n)
		a.reset()
		return nil, unavailable("assembly")
	}
	a.charged += n
	a.data.WriteString(p.Data)
	a.next++
	if a.next != a.parts {
		return nil, nil
	}
	raw := bytes.Clone(a.data.Bytes())
	number := a.n
	a.reset()
	_, fields, err := liveObject(raw)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(fields["n"], &p.N); err != nil || p.N != number {
		return nil, errors.New("action number mismatch")
	}
	return raw, nil
}

type liveRate struct {
	tokens float64
	last   time.Time
}

func (b *liveRate) take(now time.Time, rate, burst float64) bool {
	if b.last.IsZero() {
		b.tokens = burst
	} else {
		b.tokens = min(burst, b.tokens+now.Sub(b.last).Seconds()*rate)
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

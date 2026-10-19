// Package model holds the board's element model: content fields, property
// bundles, validation, canonical JSON, the public projection, connector
// geometry and fractional stacking keys. It has no state and no I/O.
package model

// Limits of section 12 of the board technical design. The element, text,
// alternative text, pen point and asset limits are judged per item by the
// "excess grew" rule (Limits), so a lowered limit never blocks a board that
// is already over it.
const (
	MaxElements      = 10000
	MaxText          = 4000 // characters per element
	MaxLabel         = 200  // frame name or connector label
	MaxAlt           = 1000
	MaxTitle         = 200
	MaxPenPoints     = 2000
	MaxAssets        = 512
	MaxPersonOps     = 500
	MaxAgentOps      = 2000
	MaxLayoutTargets = 1000
	MaxZLen          = 32
	MaxResultsBytes  = 64 << 10

	MaxCoord = 10_000_000
	MinSize  = 1
	MaxSize  = 100_000
	MaxRot   = 360
	PenScale = 10000 // pen points are normalized to 0..PenScale inside the box

	// maxZInput bounds a stacking key a client sends; longer stored keys are
	// rekeyed after the batch (MaxZLen).
	maxZInput = 64
)

// Limits are the per-item limits a batch is checked against.
type Limits struct {
	Elements  int
	Text      int
	Label     int
	Alt       int // image alternative text, in characters
	PenPoints int
	Assets    int
}

// DefaultLimits are the B1 limits.
var DefaultLimits = Limits{
	Elements:  MaxElements,
	Text:      MaxText,
	Label:     MaxLabel,
	Alt:       MaxAlt,
	PenPoints: MaxPenPoints,
	Assets:    MaxAssets,
}

// TextLimit is the text limit of an element type.
func (l Limits) TextLimit(typ string) int {
	if typ == TypeFrame || typ == TypeConnector {
		return l.Label
	}
	return l.Text
}

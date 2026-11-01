package agentsetup

import "strings"

// DiffLine is one line of a change preview. Op is '+', '-', ' ' for context,
// or '@' for a gap between hunks.
type DiffLine struct {
	Op   byte
	Text string
}

// maxDiffCells bounds the work of the line diff. Beyond it the middle of the
// change is shown as one removal and one addition.
const maxDiffCells = 4_000_000

func textLines(b []byte) []string {
	var out []string
	for _, l := range splitLines(b) {
		out = append(out, string(b[l.start:l.textEnd]))
	}
	return out
}

// LineDiff lists the lines that differ between before and after, each hunk
// with ctx lines of context.
func LineDiff(before, after []byte, ctx int) []DiffLine {
	a, b := textLines(before), textLines(after)
	pre := 0
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		pre++
	}
	suf := 0
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}
	midA, midB := a[pre:len(a)-suf], b[pre:len(b)-suf]

	var ops []DiffLine
	for _, l := range a[:pre] {
		ops = append(ops, DiffLine{' ', l})
	}
	ops = append(ops, diffMiddle(midA, midB)...)
	for _, l := range a[len(a)-suf:] {
		ops = append(ops, DiffLine{' ', l})
	}
	return hunks(ops, ctx)
}

func diffMiddle(a, b []string) []DiffLine {
	var out []DiffLine
	if len(a)*len(b) > maxDiffCells {
		for _, l := range a {
			out = append(out, DiffLine{'-', l})
		}
		for _, l := range b {
			out = append(out, DiffLine{'+', l})
		}
		return out
	}
	// lcs[i][j] is the length of the longest common subsequence of a[i:] and b[j:].
	lcs := make([][]int32, len(a)+1)
	for i := range lcs {
		lcs[i] = make([]int32, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			out = append(out, DiffLine{' ', a[i]})
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			out = append(out, DiffLine{'-', a[i]})
			i++
		default:
			out = append(out, DiffLine{'+', b[j]})
			j++
		}
	}
	for ; i < len(a); i++ {
		out = append(out, DiffLine{'-', a[i]})
	}
	for ; j < len(b); j++ {
		out = append(out, DiffLine{'+', b[j]})
	}
	return out
}

// hunks keeps the changed lines of ops and ctx lines around them.
func hunks(ops []DiffLine, ctx int) []DiffLine {
	keep := make([]bool, len(ops))
	for i, op := range ops {
		if op.Op == ' ' {
			continue
		}
		for k := max(0, i-ctx); k <= min(len(ops)-1, i+ctx); k++ {
			keep[k] = true
		}
	}
	var out []DiffLine
	gap := false
	for i, op := range ops {
		if !keep[i] {
			gap = len(out) > 0
			continue
		}
		if gap {
			out = append(out, DiffLine{'@', ""})
			gap = false
		}
		out = append(out, op)
	}
	return out
}

// Render formats d for a terminal, one line each.
func (d DiffLine) Render() string {
	switch d.Op {
	case '@':
		return "  ..."
	case ' ':
		return "    " + d.Text
	}
	return "  " + string(d.Op) + " " + strings.TrimRight(d.Text, "\r")
}

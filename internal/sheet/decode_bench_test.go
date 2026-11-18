package sheet

import (
	"fmt"
	"strings"
	"testing"
)

// pasteFrame is the action of a paste of rows by cols typed strings, as a
// browser sends it once its parts are joined.
func pasteFrame(rows, cols int) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, `{"type":"action","n":"1","base":"7","ops":[{"op":"write","tab":1,"rows":[[1,%d]],"cols":[[2001,%d]],"values":[`, rows, cols)
	for r := 0; r < rows; r++ {
		if r > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('[')
		for c := 0; c < cols; c++ {
			if c > 0 {
				b.WriteByte(',')
			}
			switch c % 4 {
			case 0:
				fmt.Fprintf(&b, `"item %d"`, r)
			case 1:
				fmt.Fprintf(&b, `"%d.%02d"`, r*37%10000, c)
			case 2:
				fmt.Fprintf(&b, `"=B%d*C%d"`, r+1, r+1)
			default:
				b.WriteString(`"a \"quoted\" note, with a comma"`)
			}
		}
		b.WriteByte(']')
	}
	b.WriteString(`]}]}`)
	return []byte(b.String())
}

// writeCall is an update_sheet call that writes rows by cols cells.
func writeCall(rows, cols int) []byte {
	var b strings.Builder
	b.WriteString(`{"state":"s_000000000000.1","op_id":"bench-1","ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[`)
	for r := 0; r < rows; r++ {
		if r > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('[')
		for c := 0; c < cols; c++ {
			if c > 0 {
				b.WriteByte(',')
			}
			switch c % 5 {
			case 0:
				fmt.Fprintf(&b, `"item %d"`, r)
			case 1:
				fmt.Fprintf(&b, `%d.%02d`, r*37%10000, c)
			case 2:
				fmt.Fprintf(&b, `"=B%d*C%d"`, r+1, r+1)
			case 3:
				b.WriteString(`{"date":"2026-03-15"}`)
			default:
				b.WriteString(`null`)
			}
		}
		b.WriteByte(']')
	}
	b.WriteString(`]}]}`)
	return []byte(b.String())
}

// BenchmarkDecodePaste decodes the action of a large paste: 5,000 rows of
// 20 typed strings, about 1.3 MiB.
func BenchmarkDecodePaste(b *testing.B) {
	data := pasteFrame(5000, 20)
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	for b.Loop() {
		var f actionFrame
		if err := strictJSON(data, &f); err != nil || len(f.Ops) != 1 {
			b.Fatal(err)
		}
	}
}

// BenchmarkDecodeSet decodes the action of one typed cell, the frame a
// person sends most.
func BenchmarkDecodeSet(b *testing.B) {
	data := []byte(`{"type":"action","n":"12","base":"340","ops":[{"op":"set","tab":1,"row":20,"col":2003,"text":"=SUM(B2:B8)","at":[19,2]}]}`)
	b.ReportAllocs()
	for b.Loop() {
		var f actionFrame
		if err := strictJSON(data, &f); err != nil || len(f.Ops) != 1 {
			b.Fatal(err)
		}
	}
}

// BenchmarkDecodeCall decodes an update_sheet call of 50,000 cells.
func BenchmarkDecodeCall(b *testing.B) {
	data := writeCall(500, 100)
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := DecodeUpdate(data); err != nil {
			b.Fatal(err)
		}
	}
}

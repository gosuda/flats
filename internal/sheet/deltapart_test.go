package sheet

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// joinDeltaParts joins the delta_part frames of one delta as the browser
// client does (net.js): the fields of the parts in order, a list field
// carried by several parts as one list, a row that goes on under the same
// row id as one row, and an edit with more as one edit with the next.
func joinDeltaParts(parts []map[string]any) (map[string]any, error) {
	d, _, err := joinDeltaPartsCounted(parts)
	return d, err
}

// joinDeltaPartsCounted also counts the items that went on in a later part.
func joinDeltaPartsCounted(parts []map[string]any) (map[string]any, int, error) {
	d := map[string]any{"type": "delta"}
	continued := 0
	for i, p := range parts {
		if p["part"] != float64(i) || p["parts"] != float64(len(parts)) || p["seq"] != parts[0]["seq"] {
			return nil, 0, fmt.Errorf("part %d is %v of %v", i, p["part"], p["parts"])
		}
		for k, v := range p {
			switch k {
			case "type", "part", "parts":
				continue
			case "seq":
				d[k] = v
				continue
			}
			have, ok := d[k]
			if !ok {
				d[k] = v
				continue
			}
			list, ok1 := have.([]any)
			next, ok2 := v.([]any)
			if !ok1 || !ok2 {
				return nil, 0, fmt.Errorf("part %d repeats the field %s", i, k)
			}
			if len(list) > 0 && len(next) > 0 {
				switch k {
				case "cells":
					last, first := list[len(list)-1].([]any), next[0].([]any)
					if last[0] == first[0] {
						last[1] = append(last[1].([]any), first[1].([]any)...)
						next = next[1:]
						continued++
					}
				case "struct":
					last, first := list[len(list)-1].(map[string]any), next[0].(map[string]any)
					if last["more"] == true {
						if first["op"] != last["op"] || first["tab"] != last["tab"] {
							return nil, 0, fmt.Errorf("part %d goes on with another edit", i)
						}
						delete(last, "more")
						for fk, fv := range first {
							if fk == "op" || fk == "tab" {
								continue
							}
							if lv, ok := last[fk]; ok {
								last[fk] = append(lv.([]any), fv.([]any)...)
							} else {
								last[fk] = fv
							}
						}
						next = next[1:]
						continued++
					}
				}
			}
			d[k] = append(list, next...)
		}
	}
	for _, e := range asList(d["struct"]) {
		if e.(map[string]any)["more"] != nil {
			return nil, 0, fmt.Errorf("an edit goes on after the last part")
		}
	}
	return d, continued, nil
}

func asList(v any) []any {
	list, _ := v.([]any)
	return list
}

// ndjsonFrames reads the body of a History tab: one frame a line.
func ndjsonFrames(data []byte) ([]any, error) {
	var out []any
	lines := bufio.NewScanner(bytes.NewReader(data))
	lines.Buffer(nil, serverFrameMax+1)
	for lines.Scan() {
		var f map[string]any
		if err := json.Unmarshal(lines.Bytes(), &f); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, lines.Err()
}

// cutFrame cuts the text of a delta frame into its delta_part frames, as
// a connection sends them: the parts counted first, then built one at a
// time.
func cutFrame(frame string) ([]string, error) {
	members, ok := jsonMembers([]byte(frame), 0)
	if !ok || len(members) < 2 || members[0].key != "type" || members[1].key != "seq" {
		return nil, fmt.Errorf("no delta frame")
	}
	var fields []deltaField
	for _, m := range members[2:] {
		fields = append(fields, deltaField{m.key, m.value})
	}
	seq := strings.Trim(string(members[1].value), `"`)
	count, err := splitDelta(fields, nil)
	if err != nil {
		return nil, err
	}
	var out []string
	n, err := splitDelta(fields, func(i int, text []byte) bool {
		out = append(out, partHead(seq, i, count)+string(text))
		return true
	})
	if err == nil && n != count {
		err = fmt.Errorf("%d parts counted and %d cut", count, n)
	}
	return out, err
}

// TestDeltaPartsJoinToTheDelta cuts deltas larger than a frame and joins
// their parts back: every part is a JSON object inside the server frame
// limit, and the join is the delta. The cases hold the items that are
// longer than a part, a row of many large cells and a sort of every row
// of a tab, and items that fit a part exactly at a cut.
func TestDeltaPartsJoinToTheDelta(t *testing.T) {
	cell := func(col int, text string) string {
		value, _ := json.Marshal(text)
		edit, _ := json.Marshal("'" + text)
		return fmt.Sprintf(`[%d,"t",%s,null,%s,1]`, col, value, edit)
	}
	big := strings.Repeat("\x01", formula.MaxTextLen)
	var cells []string
	for row := 1; row <= 30; row++ {
		var list []string
		for col := 1; col <= 1+row%3; col++ {
			list = append(list, cell(col, big[:formula.MaxTextLen-row]))
		}
		cells = append(cells, fmt.Sprintf("[%d,[%s]]", 1000+row, strings.Join(list, ",")))
	}
	var ids, positions []string
	for i := range formula.MaxRows {
		ids = append(ids, strconv.Itoa(9007199254740000-2*i))
		positions = append(positions, fmt.Sprintf("[%d,1]", 2*i))
	}
	sort := fmt.Sprintf(`{"op":"sort","tab":3,"positions":[%s],"ids":[%s]}`, strings.Join(positions, ","), strings.Join(ids, ","))
	insert := fmt.Sprintf(`{"op":"insert_rows","tab":3,"ids":[%s],"after":null}`, strings.Join(positions, ","))
	var styles []string
	for i := range 8000 {
		styles = append(styles, fmt.Sprintf(`{"bold":true,"number":{"kind":"currency","symbol":"S%d","decimals":2}}`, i))
	}
	for _, c := range []struct {
		name, fields string
		continued    int
	}{
		{"cells", `,"from":{"session_id":"s1","n":"4"},"cells":[` + strings.Join(cells, ",") + `],"styles":[{"bold":true}],"note":{"agent":true,"tab":3,"cells":60}`, 20},
		{"a sort and an insert of every row", `,"tabs":[{"id":3,"name":"T","rows":100000,"cols":1,"frozen_rows":0,"frozen_cols":0}],"struct":[{"op":"rename_tab","tab":3,"name":"T"},` + sort + "," + insert + `,{"op":"delete_tab","tab":4}],"touched":[4]`, 6},
		{"styles", `,"cells":[[5,[[7,"t","x",null,null,8000]]]],"styles":[` + strings.Join(styles, ",") + `]`, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			frame := `{"type":"delta","seq":"77"` + c.fields + `}`
			texts, err := cutFrame(frame)
			if err != nil {
				t.Fatal(err)
			}
			if len(texts) < 2 {
				t.Fatalf("%d parts", len(texts))
			}
			var parts []map[string]any
			for i, text := range texts {
				if len(text) > serverFrameMax {
					t.Fatalf("part %d has %d bytes", i, len(text))
				}
				var p map[string]any
				if err := json.Unmarshal([]byte(text), &p); err != nil {
					t.Fatalf("part %d: %v", i, err)
				}
				parts = append(parts, p)
			}
			got, continued, err := joinDeltaPartsCounted(parts)
			if err != nil {
				t.Fatal(err)
			}
			if continued < c.continued {
				t.Fatalf("%d items went on in a later part, want at least %d", continued, c.continued)
			}
			var want map[string]any
			if err := json.Unmarshal([]byte(frame), &want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatal("the joined parts are not the delta")
			}
		})
	}
}

// TestEscapedTextFramesStayInsideTheLimit loads a tab of text that takes
// six times its size as JSON through History and a live connection, and
// formats all of it. Every History line and every frame of the connection
// stays inside the server frame limit, and the real client, given the
// connection's frames, holds the engine's tab. The browser parses one
// frame at a time, so a tab whose frames add up to more than a JavaScript
// string holds loads the same way (the client's own tests hold such a
// tab; this one keeps the engine's part of it to a size a test can run).
func TestEscapedTextFramesStayInsideTheLimit(t *testing.T) {
	heavy(t, "encodes 70 MB of frames")
	node, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("node is not installed, so the client cannot be checked in CI")
		}
		t.Skip("node is not installed")
	}
	e := start(t)
	// 100 cells of an apostrophe, an equals sign and 30,000 U+0001.
	const cells, chars = 100, 30000
	text := strings.Repeat(`\u0001`, chars)
	for row := 1; row <= cells; row += 25 {
		list := make([]string, 25)
		for i := range list {
			list[i] = `["'=` + text + `"]`
		}
		e.mustAgent(fmt.Sprintf(`{"ops":[{"op":"write","tab":"Sheet1","at":"A%d","rows":[%s]}]}`, row, strings.Join(list, ",")))
	}
	rev := e.b.importAll(t)
	s := serve(e)
	tab := e.tabID("Sheet1")
	value := "=" + strings.Repeat("\x01", chars)

	for {
		res, err := http.Get(fmt.Sprintf("%s/private/_sheet/api/revisions/%d/tabs/%d", s.server.URL, rev, tab))
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode == http.StatusServiceUnavailable {
			continue // calculating
		}
		frames, ferr := ndjsonFrames(data)
		if err != nil || ferr != nil || res.StatusCode != http.StatusOK || len(data) < 2*6*cells*chars {
			t.Fatalf("the History tab: %d, %d bytes, %v %v", res.StatusCode, len(data), err, ferr)
		}
		got := 0
		for _, f := range frames {
			if f.(map[string]any)["type"] != "rows" {
				continue
			}
			for _, row := range asList(f.(map[string]any)["rows"]) {
				for _, c := range row.([]any)[1].([]any) {
					if c.([]any)[2] != value || c.([]any)[4] != "'"+value {
						t.Fatalf("a History cell holds %.40q", c)
					}
					got++
				}
			}
		}
		if got != cells {
			t.Fatalf("History holds %d cells", got)
		}
		break
	}

	r := s.record("private", `{"type":"hello","version":1,"name":"Ann","pending":0}`)
	r.watch(tab)
	e.mustAgent(fmt.Sprintf(`{"ops":[{"op":"format","tab":"Sheet1","ranges":["A1:A%d"],"format":{"bold":true}}]}`, cells))
	steps, kinds := r.taken()
	if kinds["delta_part"] < 2*6*cells*chars/serverFrameMax {
		t.Fatalf("the format arrived in %d parts: %v", kinds["delta_part"], kinds)
	}
	for _, step := range steps {
		if len(step.Recv) > serverFrameMax {
			t.Fatalf("a frame of %d bytes", len(step.Recv))
		}
	}
	got := replay(t, node, steps)
	if got["sockets"] != float64(1) {
		t.Errorf("the client opened %v sockets", got["sockets"])
	}
	for _, diff := range compareClient(got, e.clientView("Sheet1")) {
		t.Error(diff)
	}
}

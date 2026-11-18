// Package benchdata generates the sheets the sheet success criteria are
// measured on (spec "Success criteria", technical design section 22.4): the
// reference sheet and the limits fixture.
//
// A fixture is a sequence of update_sheet calls, so that it reaches a sheet
// through the same domain operations an agent uses: the engine's Update,
// the management API or MCP. The calls carry no state; the caller adds the
// state it read before each call. Generation is deterministic: the same
// seed gives the same bytes.
package benchdata

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"math/rand"
	"strconv"
	"strings"
)

// Seed is the seed of the reference sheet.
const Seed = 20261113

// The shape of the reference sheet's large tab.
const (
	LargeTab   = "Orders"
	TotalsTab  = "Totals"
	LargeRows  = 20001 // a frozen header row and 20,000 filled rows
	LargeCols  = 20
	SmallTabs  = 8
	chunkRows  = 2000 // rows of the large tab in one call: 40,000 cells
	smallRows  = 40
	maxCallLen = 8 << 20
)

// Manifest describes a generated fixture.
type Manifest struct {
	Seed     int64  `json:"seed"`
	Tabs     int    `json:"tabs"`
	Calls    int    `json:"calls"`
	Cells    int    `json:"cells"`    // cells with content
	Formulas int    `json:"formulas"` // cells that hold a formula
	Digest   string `json:"digest"`   // SHA-256 of every call, in order
	// Totals are the expected values of the totals tab, B1 to B20: the sum
	// of each column of the large tab. Every value is exact in binary
	// floating point, so the order of summation does not matter.
	Totals []float64 `json:"totals"`
	// Input names a cell of the large tab that feeds its row's two
	// formulas, for the recalculation criterion.
	Input string `json:"input"`
}

var largeHeader = [LargeCols]string{"Order", "Customer", "Quantity", "Unit price", "Amount", "Discount", "Net", "Ordered",
	"Shipped", "Region", "Weight", "Boxes", "Score", "Stock", "Reorder", "Lead days", "Returns", "Rating", "Batch", "Note"}

var regions = [...]string{"North", "South", "East", "West", "Central"}

type emitter struct {
	emit func(call []byte) error
	m    Manifest
	hash hash.Hash
}

func newEmitter(seed int64, emit func([]byte) error) *emitter {
	return &emitter{emit: emit, m: Manifest{Seed: seed}, hash: sha256.New()}
}

func (e *emitter) call(message string, ops ...string) error {
	body := `{"message":` + strconv.Quote(message) + `,"ops":[` + strings.Join(ops, ",") + `]}`
	if len(body) > maxCallLen {
		return fmt.Errorf("benchdata: a call of %d bytes passes the call limit", len(body))
	}
	e.hash.Write([]byte(body))
	e.m.Calls++
	return e.emit([]byte(body))
}

func (e *emitter) manifest() Manifest {
	e.m.Digest = hex.EncodeToString(e.hash.Sum(nil))
	return e.m
}

func column(i int) string {
	name := ""
	for n := i + 1; n > 0; n = (n - 1) / 26 {
		name = string(rune('A'+(n-1)%26)) + name
	}
	return name
}

func num(x float64) string { return strconv.FormatFloat(x, 'f', -1, 64) }

// Reference generates the reference sheet with the fixed seed: ten tabs, a
// large tab of 20,001 rows by 20 columns whose columns E and G are formulas
// that read their own row, a totals tab with one SUM per column of the
// large tab, and eight small tabs. It hands each call to emit and returns
// the manifest. The first call expects a new sheet with its one tab Sheet1.
func Reference(emit func(call []byte) error) (Manifest, error) {
	rng := rand.New(rand.NewSource(Seed))
	e := newEmitter(Seed, emit)
	e.m.Tabs = 2 + SmallTabs
	e.m.Totals = make([]float64, LargeCols)
	e.m.Input = LargeTab + "!C2"

	header := make([]string, LargeCols)
	for i, name := range largeHeader {
		header[i] = strconv.Quote(name)
	}
	ops := []string{
		`{"op":"rename_tab","tab":"Sheet1","name":"` + LargeTab + `"}`,
		`{"op":"add_tab","name":"` + TotalsTab + `"}`,
	}
	for i := 1; i <= SmallTabs; i++ {
		ops = append(ops, fmt.Sprintf(`{"op":"add_tab","name":"Team %d"}`, i))
	}
	ops = append(ops,
		`{"op":"write","tab":"`+LargeTab+`","at":"A1","rows":[[`+strings.Join(header, ",")+`]]}`,
		`{"op":"tab_settings","tab":"`+LargeTab+`","frozen_rows":1,"frozen_cols":1}`,
		`{"op":"format","tab":"`+LargeTab+`","ranges":["A1:T1"],"format":{"bold":true,"fill":"gray-200"}}`,
		`{"op":"widths","tab":"`+LargeTab+`","cols":"B:B","width":160}`,
	)
	e.m.Cells += LargeCols
	if err := e.call("Reference sheet: tabs and header", ops...); err != nil {
		return Manifest{}, err
	}

	// Every number is a multiple of 1/64 and far below 2^53 in total, so
	// the totals are exact whatever order they are summed in.
	var b strings.Builder
	for first := 2; first <= LargeRows; first += chunkRows {
		b.Reset()
		last := min(first+chunkRows-1, LargeRows)
		for row := first; row <= last; row++ {
			qty := float64(1 + rng.Intn(100))
			price := float64(4+rng.Intn(4000)) / 4
			discount := float64(rng.Intn(5)) / 8
			day := 45000 + rng.Intn(700)
			vals := [LargeCols]float64{0: float64(row - 1), 2: qty, 3: price, 4: qty * price, 5: discount, 6: qty * price * (1 - discount),
				7: float64(day), 10: float64(rng.Intn(4000)) / 8, 11: float64(1 + rng.Intn(12)), 12: float64(rng.Intn(1000)),
				13: float64(rng.Intn(500)), 14: float64(rng.Intn(50)), 15: float64(1 + rng.Intn(30)), 16: float64(rng.Intn(5)),
				17: float64(1+rng.Intn(10)) / 2, 18: float64(1000 + rng.Intn(9000))}
			for col, v := range vals {
				// Text and boolean columns add nothing to a SUM.
				if col != 1 && col != 8 && col != 9 && col != 19 {
					e.m.Totals[col] += v
				}
			}
			y, mo, d := civil(day)
			if row > first {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, `[%s,"Customer %d",%s,%s,"=C%d*D%d",%s,"=E%d*(1-F%d)",{"date":"%04d-%02d-%02d"},%v,"%s",%s,%s,%s,%s,%s,%s,%s,%s,%s,"batch %d"]`,
				num(vals[0]), 1+rng.Intn(900), num(qty), num(price), row, row, num(discount), row, row, y, mo, d, rng.Intn(2) == 0,
				regions[rng.Intn(len(regions))], num(vals[10]), num(vals[11]), num(vals[12]), num(vals[13]), num(vals[14]), num(vals[15]),
				num(vals[16]), num(vals[17]), num(vals[18]), rng.Intn(100))
		}
		e.m.Cells += (last - first + 1) * LargeCols
		e.m.Formulas += (last - first + 1) * 2
		if err := e.call(fmt.Sprintf("Reference sheet: rows %d to %d", first, last),
			fmt.Sprintf(`{"op":"write","tab":"%s","at":"A%d","rows":[%s]}`, LargeTab, first, b.String())); err != nil {
			return Manifest{}, err
		}
	}
	if err := e.call("Reference sheet: formats of the large tab",
		fmt.Sprintf(`{"op":"format","tab":"%s","ranges":["D2:E%d","G2:G%d"],"format":{"number":{"kind":"currency","symbol":"$","decimals":2}}}`, LargeTab, LargeRows, LargeRows),
		fmt.Sprintf(`{"op":"format","tab":"%s","ranges":["F2:F%d"],"format":{"number":{"kind":"percent","decimals":1}}}`, LargeTab, LargeRows)); err != nil {
		return Manifest{}, err
	}

	b.Reset()
	for col := 0; col < LargeCols; col++ {
		if col > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `["%s","=SUM(%s!%s:%s)"]`, largeHeader[col], LargeTab, column(col), column(col))
	}
	e.m.Cells += 2 * LargeCols
	e.m.Formulas += LargeCols
	ops = []string{fmt.Sprintf(`{"op":"write","tab":"%s","at":"A1","rows":[%s]}`, TotalsTab, b.String())}
	for i := 1; i <= SmallTabs; i++ {
		b.Reset()
		b.WriteString(`["Task","Owner","Hours","Rate","Cost","Done"]`)
		for row := 2; row <= smallRows; row++ {
			fmt.Fprintf(&b, `,["Task %d","Person %d",%d,%s,"=C%d*D%d",%v]`, row-1, 1+rng.Intn(9), 1+rng.Intn(40), num(float64(40+rng.Intn(120))/2), row, row, rng.Intn(3) == 0)
		}
		fmt.Fprintf(&b, `,["Total",null,"=SUM(C2:C%d)",null,"=SUM(E2:E%d)",null]`, smallRows, smallRows)
		e.m.Cells += 6*smallRows + 3
		e.m.Formulas += smallRows - 1 + 2
		ops = append(ops,
			fmt.Sprintf(`{"op":"write","tab":"Team %d","at":"A1","rows":[%s]}`, i, b.String()),
			fmt.Sprintf(`{"op":"format","tab":"Team %d","ranges":["A1:F1"],"format":{"bold":true}}`, i),
			fmt.Sprintf(`{"op":"tab_settings","tab":"Team %d","frozen_rows":1,"frozen_cols":0}`, i))
	}
	if err := e.call("Reference sheet: totals and small tabs", ops...); err != nil {
		return Manifest{}, err
	}
	return e.manifest(), nil
}

// civil returns the calendar date of a day number, where day 2 is
// 1900-01-01 (the sheet's calendar).
func civil(day int) (y, m, d int) {
	// Days since 1970-01-01: day 25569 is 1970-01-01.
	z := day - 25569 + 719468
	era := z / 146097
	doe := z - era*146097
	yoe := (doe - doe/1460 + doe/36524 - doe/146096) / 365
	y = yoe + era*400
	doy := doe - (365*yoe + yoe/4 - yoe/100)
	mp := (5*doy + 2) / 153
	d = doy - (153*mp+2)/5 + 1
	m = mp + 3
	if m > 12 {
		m -= 12
		y++
	}
	return y, m, d
}

// The shape of the limits fixture.
const (
	LimitsFormulas = 100_000
	LimitsReads    = 20_000_000
	limitsTextLen  = 32767
)

// Limits generates the limits fixture: a sheet at the limits that bound a
// recalculation. It holds 100,000 formulas that together read 20,000,000
// cells, and typed text up to the limit of 50,000,000 characters that the
// text of those formulas leaves room for. textCells is the number of cells
// of 32,767 characters it wrote.
func Limits(emit func(call []byte) error) (m Manifest, textCells int, err error) {
	e := newEmitter(0, emit)
	e.m.Tabs = 3
	if err := e.call("Limits fixture: tabs",
		`{"op":"rename_tab","tab":"Sheet1","name":"Nums"}`, `{"op":"add_tab","name":"Calc"}`, `{"op":"add_tab","name":"Text"}`,
		`{"op":"write","tab":"Calc","at":"CV1000","rows":[[null]]}`, `{"op":"write","tab":"Text","at":"A2000","rows":[[null]]}`); err != nil {
		return m, 0, err
	}
	// 200 numbers that every formula reads: 100,000 * 200 = 20,000,000.
	var b strings.Builder
	for row := 1; row <= 20; row++ {
		if row > 1 {
			b.WriteByte(',')
		}
		b.WriteString(`[1,2,3,4,5,6,7,8,9,10]`)
	}
	e.m.Cells += 200
	if err := e.call("Limits fixture: numbers", `{"op":"write","tab":"Nums","at":"A1","rows":[`+b.String()+`]}`); err != nil {
		return m, 0, err
	}
	const formula = `"=SUM(Nums!$A$1:$J$20)"`
	rowCells := strings.TrimSuffix(strings.Repeat(formula+",", 100), ",")
	typed := 0
	for first := 1; first <= 1000; first += 500 {
		rows := strings.TrimSuffix(strings.Repeat("["+rowCells+"],", 500), ",")
		e.m.Cells += 50_000
		e.m.Formulas += 50_000
		// The text of each formula counts toward the typed text.
		typed += 50_000 * (len(formula) - 2)
		if err := e.call(fmt.Sprintf("Limits fixture: formulas from row %d", first),
			fmt.Sprintf(`{"op":"write","tab":"Calc","at":"A%d","rows":[%s]}`, first, rows)); err != nil {
			return m, 0, err
		}
	}
	text := strconv.Quote(strings.Repeat("t", limitsTextLen))
	const perCall = 200
	for row := 1; typed+limitsTextLen <= 50_000_000; {
		n := min(perCall, (50_000_000-typed)/limitsTextLen)
		rows := strings.TrimSuffix(strings.Repeat("["+text+"],", n), ",")
		if err := e.call(fmt.Sprintf("Limits fixture: text from row %d", row),
			fmt.Sprintf(`{"op":"write","tab":"Text","at":"A%d","rows":[%s]}`, row, rows)); err != nil {
			return m, 0, err
		}
		row, typed, textCells = row+n, typed+n*limitsTextLen, textCells+n
		e.m.Cells += n
	}
	return e.manifest(), textCells, nil
}

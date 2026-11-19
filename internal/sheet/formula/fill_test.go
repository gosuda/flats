package formula

import (
	"fmt"
	"math"
	"strings"
	"testing"
)

func numC(x float64) Content    { return Content{Kind: ContentNumber, Num: x} }
func textC(s string) Content    { return Content{Kind: ContentText, Text: s} }
func dateC(y, m, d int) Content { return Content{Kind: ContentDate, Num: float64(DateOf(y, m, d))} }
func clockC(h, m int) Content {
	return Content{Kind: ContentTime, Num: float64(h*3600+m*60) / daySeconds}
}
func stampC(y, mo, d, h, mi int) Content {
	return Content{Kind: ContentDateTime, Num: float64(DateOf(y, mo, d)) + float64(h*3600+mi*60)/daySeconds}
}

// typedC is the content that typed entry gives for s.
func typedC(s string) Content {
	c, _ := ParseTyped(s)
	return c
}

// isDateKind reports whether a content of kind k is a date, a time or a
// date with time.
func isDateKind(k ContentKind) bool {
	return k == ContentDate || k == ContentTime || k == ContentDateTime
}

// sameAsTyped reports whether a content of a series is, bit for bit, the
// content that typed entry gives for the text it shows.
func sameAsTyped(c Content, text string) bool {
	typed := typedC(text)
	return c.Kind == typed.Kind && math.Float64bits(c.Num) == math.Float64bits(typed.Num) && c.Text == typed.Text
}

func texts(s ...string) []Content {
	out := make([]Content, len(s))
	for i, t := range s {
		out[i] = textC(t)
	}
	return out
}

func nums(x ...float64) []Content {
	out := make([]Content, len(x))
	for i, v := range x {
		out[i] = numC(v)
	}
	return out
}

// shownSeries is what each content of a series shows under General, which
// shows a date, a time and a date with time in their ISO forms.
func shownSeries(cs []Content) string {
	shown := make([]string, len(cs))
	for i, c := range cs {
		shown[i] = Show(c.Value(), c.Kind, NumberFormat{})
	}
	return strings.Join(shown, ", ")
}

// F-01 and F-02: FillSeries gives the series of rule 21a, down and up, and
// no series for every other source. A series is listed from the cell next
// to the source outward, and each content has the kind of its source. A
// date, a time and a date with time is the content that typed entry gives
// for the text it shows, bit for bit.
func TestFillSeries(t *testing.T) {
	cases := []struct {
		name       string
		src        []Content
		n          int
		back, step bool
		want       string // the shown series, or "" for none
		kind       ContentKind
	}{
		// F-01
		{"one date", []Content{dateC(2026, 3, 15)}, 3, false, false, "2026-03-16, 2026-03-17, 2026-03-18", ContentDate},
		{"one date up", []Content{dateC(2026, 3, 1)}, 2, true, false, "2026-02-28, 2026-02-27", ContentDate},
		{"one date with time", []Content{stampC(2026, 3, 15, 14, 30)}, 2, false, false, "2026-03-16 14:30:00, 2026-03-17 14:30:00", ContentDateTime},
		{"one date with time up", []Content{stampC(2026, 3, 15, 14, 30)}, 1, true, false, "2026-03-14 14:30:00", ContentDateTime},
		{"one time from 22:00", []Content{clockC(22, 0)}, 3, false, false, "23:00:00, 00:00:00, 01:00:00", ContentTime},
		{"one time from 01:00 up", []Content{clockC(1, 0)}, 2, true, false, "00:00:00, 23:00:00", ContentTime},
		{"full weekday in capitals", texts("MONDAY"), 2, false, false, "TUESDAY, WEDNESDAY", ContentText},
		{"full weekday in capitals up", texts("MONDAY"), 2, true, false, "SUNDAY, SATURDAY", ContentText},
		{"short weekday with an initial capital", texts("Mon"), 2, false, false, "Tue, Wed", ContentText},
		{"short weekday up", texts("Mon"), 2, true, false, "Sun, Sat", ContentText},
		{"Sunday goes on to Monday", texts("Sunday"), 2, false, false, "Monday, Tuesday", ContentText},
		{"full month in small letters", texts("january"), 2, false, false, "february, march", ContentText},
		{"full month in small letters up", texts("january"), 2, true, false, "december, november", ContentText},
		{"short month in capitals", texts("DEC"), 2, false, false, "JAN, FEB", ContentText},
		{"short month with an initial capital", texts("Sep"), 1, false, false, "Oct", ContentText},
		{"mONDAY", texts("mONDAY"), 1, false, false, "tuesday", ContentText},
		{"Monday, Wednesday", texts("Monday", "Wednesday"), 2, false, false, "Friday, Sunday", ContentText},
		{"Monday, Wednesday up", texts("Monday", "Wednesday"), 2, true, false, "Saturday, Thursday", ContentText},
		{"Jan, Apr, Jul", texts("Jan", "Apr", "Jul"), 2, false, false, "Oct, Jan", ContentText},
		{"Jan, Apr, Jul up", texts("Jan", "Apr", "Jul"), 2, true, false, "Oct, Jul", ContentText},
		{"the form of the last name", texts("Monday", "Tue"), 1, false, false, "Wed", ContentText},
		// The form is that of the last source cell whose name has one
		// form, and the three-letter form when none has; May has both.
		{"May", texts("May"), 1, false, false, "Jun", ContentText},
		{"May up", texts("May"), 1, true, false, "Apr", ContentText},
		{"April, May", texts("April", "May"), 1, false, false, "June", ContentText},
		{"April, May up", texts("April", "May"), 1, true, false, "March", ContentText},
		{"Jan, May", texts("Jan", "May"), 1, false, false, "Sep", ContentText},
		{"January, May", texts("January", "May"), 1, false, false, "September", ContentText},
		{"May alone in capitals", texts("MAY"), 1, false, false, "JUN", ContentText},
		// Equal names step by none and repeat the name.
		{"Mon, Mon", texts("Mon", "Mon"), 2, false, false, "Mon, Mon", ContentText},
		{"Mon, Mon up", texts("Mon", "Mon"), 2, true, false, "Mon, Mon", ContentText},
		{"May, May", texts("May", "May"), 2, false, false, "May, May", ContentText},
		{"Item1", texts("Item1"), 2, false, false, "Item2, Item3", ContentText},
		{"Item09", texts("Item09"), 2, false, false, "Item10, Item11", ContentText},
		{"Item09 up", texts("Item09"), 2, true, false, "Item08, Item07", ContentText},
		{"Item1 up past 0", texts("Item1"), 3, true, false, "Item0, Item1, Item2", ContentText},
		{"a text of digits", texts("007"), 1, false, false, "008", ContentText},
		{"1, 2", nums(1, 2), 3, false, false, "3, 4, 5", ContentNumber},
		{"1, 2 up", nums(1, 2), 2, true, false, "0, -1", ContentNumber},
		{"100, 95", nums(100, 95), 2, false, false, "90, 85", ContentNumber},
		{"1, 2, 4", nums(1, 2, 4), 2, false, false, "5.33333333333333, 6.83333333333333", ContentNumber},
		{"three dates", []Content{dateC(2026, 1, 1), dateC(2026, 1, 2), dateC(2026, 1, 4)}, 2, false, false, "2026-01-05, 2026-01-07", ContentDate},
		{"three dates up", []Content{dateC(2026, 1, 1), dateC(2026, 1, 2), dateC(2026, 1, 4)}, 2, true, false, "2025-12-30, 2025-12-29", ContentDate},
		{"two dates with time", []Content{stampC(2026, 3, 15, 6, 0), stampC(2026, 3, 15, 18, 0)}, 2, false, false, "2026-03-16 06:00:00, 2026-03-16 18:00:00", ContentDateTime},
		{"22:00, 23:00", []Content{clockC(22, 0), clockC(23, 0)}, 2, false, false, "00:00:00, 01:00:00", ContentTime},
		{"00:30, 01:00 up", []Content{clockC(0, 30), clockC(1, 0)}, 2, true, false, "00:00:00, 23:30:00", ContentTime},
		{"09:00, 09:20", []Content{clockC(9, 0), clockC(9, 20)}, 2, false, false, "09:40:00, 10:00:00", ContentTime},
		{"one time from 22:00 past a day", []Content{clockC(22, 0)}, 27, false, false,
			"23:00:00, 00:00:00, 01:00:00, 02:00:00, 03:00:00, 04:00:00, 05:00:00, 06:00:00, 07:00:00, 08:00:00, 09:00:00, 10:00:00, 11:00:00, " +
				"12:00:00, 13:00:00, 14:00:00, 15:00:00, 16:00:00, 17:00:00, 18:00:00, 19:00:00, 20:00:00, 21:00:00, 22:00:00, 23:00:00, 00:00:00, 01:00:00", ContentTime},
		{"00:00 up a whole day", []Content{clockC(0, 0)}, 25, true, false,
			"23:00:00, 22:00:00, 21:00:00, 20:00:00, 19:00:00, 18:00:00, 17:00:00, 16:00:00, 15:00:00, 14:00:00, 13:00:00, 12:00:00, 11:00:00, " +
				"10:00:00, 09:00:00, 08:00:00, 07:00:00, 06:00:00, 05:00:00, 04:00:00, 03:00:00, 02:00:00, 01:00:00, 00:00:00, 23:00:00", ContentTime},
		{"dates with time twenty minutes apart", []Content{typedC("2026-03-15 08:00"), typedC("2026-03-15 08:20")}, 4, false, false,
			"2026-03-15 08:40:00, 2026-03-15 09:00:00, 2026-03-15 09:20:00, 2026-03-15 09:40:00", ContentDateTime},
		{"dates with time through 1900-01-25 20:09:37", []Content{typedC("1900-01-25 19:29:37"), typedC("1900-01-25 19:49:37")}, 2, false, false,
			"1900-01-25 20:09:37, 1900-01-25 20:29:37", ContentDateTime},
		{"dates with time past midnight", []Content{typedC("2026-03-15 23:20"), typedC("2026-03-15 23:40")}, 2, false, false,
			"2026-03-16 00:00:00, 2026-03-16 00:20:00", ContentDateTime},
		// A trend is rounded to the whole second half away from zero before
		// a time goes round the day: 00:00:00, 00:00:02 and 00:00:01 give 2
		// and 2.5 seconds after them, and 0 and -0.5 seconds before.
		{"a trend of half a second", []Content{typedC("00:00:00"), typedC("00:00:02"), typedC("00:00:01")}, 2, false, false, "00:00:02, 00:00:03", ContentTime},
		{"a trend of half a second up", []Content{typedC("00:00:00"), typedC("00:00:02"), typedC("00:00:01")}, 2, true, false, "00:00:00, 23:59:59", ContentTime},
		{"1900-01-01 down", []Content{dateC(1900, 1, 1)}, 2, false, false, "1900-01-02, 1900-01-03", ContentDate},
		{"1900-01-03 up to the first date", []Content{dateC(1900, 1, 3)}, 2, true, false, "1900-01-02, 1900-01-01", ContentDate},
		{"Item1, Item3", texts("Item1", "Item3"), 2, false, false, "Item5, Item7", ContentText},
		{"Item1, Item2, Item4", texts("Item1", "Item2", "Item4"), 2, false, false, "Item5, Item7", ContentText},
		{"one plain number in series", nums(5), 2, false, true, "6, 7", ContentNumber},
		{"one plain number in series up", nums(5), 2, true, true, "4, 3", ContentNumber},
		{"two numbers in series", nums(1, 3), 1, false, true, "5", ContentNumber},
		{"the last date", []Content{dateC(9999, 12, 29)}, 2, false, false, "9999-12-30, 9999-12-31", ContentDate},
		// F-02
		{"one plain number", nums(5), 2, false, false, "", 0},
		{"TRUE", []Content{{Kind: ContentBool, Num: 1}}, 2, false, false, "", 0},
		{"a formula", []Content{{Kind: ContentFormula, Text: "=A1"}}, 2, false, false, "", 0},
		{"an unreadable formula", []Content{{Kind: ContentUnreadable, Text: `="a`}}, 2, false, false, "", 0},
		{"a blank among numbers", []Content{numC(1), {}, numC(3)}, 2, false, false, "", 0},
		{"a number and a text", []Content{numC(1), textC("Item2")}, 2, false, false, "", 0},
		{"a number and a date", []Content{numC(1), dateC(2026, 1, 2)}, 2, false, false, "", 0},
		{"a date and a date with time", []Content{dateC(2026, 1, 1), stampC(2026, 1, 2, 6, 0)}, 2, false, false, "", 0},
		{"text with no final number", texts("Rent"), 2, false, false, "", 0},
		{"Mon, Tue, Thu", texts("Mon", "Tue", "Thu"), 2, false, false, "", 0},
		{"a weekday and a month name", texts("Mon", "Jan"), 2, false, false, "", 0},
		{"a name and a numbered text", texts("Mon", "Item2"), 2, false, false, "", 0},
		{"Item1, Task2", texts("Item1", "Task2"), 2, false, false, "", 0},
		{"stems that differ in capitals", texts("Item1", "item2"), 2, false, false, "", 0},
		{"a final number of 16 digits", texts("Item1234567890123456"), 2, false, false, "", 0},
		{"a date series past 9999-12-31", []Content{dateC(9999, 12, 30)}, 2, false, false, "", 0},
		{"dates whose trend passes 9999-12-31", []Content{dateC(9999, 12, 1), dateC(9999, 12, 15)}, 2, false, false, "", 0},
		{"a date with time past 9999-12-31", []Content{stampC(9999, 12, 31, 12, 0)}, 1, false, false, "", 0},
		{"1900-01-02 up two cells", []Content{dateC(1900, 1, 2)}, 2, true, false, "", 0},
		{"1900-01-01 up", []Content{dateC(1900, 1, 1)}, 1, true, false, "", 0},
		{"dates whose trend comes before 1900-01-01", []Content{dateC(1900, 1, 5), dateC(1900, 1, 3)}, 2, false, false, "", 0},
		{"a date with time before 1900-01-01", []Content{stampC(1900, 1, 1, 12, 0)}, 1, true, false, "", 0},
		{"dates with time whose trend comes before 1900-01-01", []Content{typedC("1900-01-01 00:20"), typedC("1900-01-01 00:10")}, 2, false, false, "", 0},
		{"a trend that is not finite", nums(math.MaxFloat64, -math.MaxFloat64), 1, false, false, "", 0},
		{"Sept", texts("Sept"), 1, false, false, "", 0},
		{"no source", nil, 2, false, false, "", 0},
	}
	for _, c := range cases {
		got, ok := FillSeries(c.src, c.n, c.back, c.step)
		if c.want == "" {
			if ok || got != nil {
				t.Errorf("%s: a series %q, want none", c.name, shownSeries(got))
			}
			continue
		}
		if !ok || len(got) != c.n {
			t.Errorf("%s: %d contents %v, want %q", c.name, len(got), ok, c.want)
			continue
		}
		if s := shownSeries(got); s != c.want {
			t.Errorf("%s: %q, want %q", c.name, s, c.want)
		}
		for i, g := range got {
			if g.Kind != c.kind {
				t.Errorf("%s: a content of kind %d, want %d", c.name, g.Kind, c.kind)
			}
			// A date or a time is compared by its number, not by the text
			// that shows it.
			if text := strings.Split(c.want, ", ")[i]; isDateKind(c.kind) && !sameAsTyped(g, text) {
				t.Errorf("%s: cell %d holds %v, and a typed %s holds %v", c.name, i, g.Num, text, typedC(text).Num)
			}
		}
	}
	// The trend of numbers is not rounded: 1, 2, 4 gives 16/3 and 41/6 as
	// doubles.
	if got, _ := FillSeries(nums(1, 2, 4), 2, false, false); len(got) != 2 || got[0] != numC(16.0/3) || got[1] != numC(41.0/6) {
		t.Errorf("1, 2, 4 gives %v", got)
	}
	// Two dates with time are not rounded to whole days, and the trend of
	// times stays within one day.
	if got, _ := FillSeries([]Content{stampC(2026, 3, 15, 6, 0), stampC(2026, 3, 15, 18, 0)}, 1, false, false); len(got) != 1 || got[0].Num != float64(DateOf(2026, 3, 16))+0.25 {
		t.Errorf("two dates with time give %v", got)
	}
	for _, back := range []bool{false, true} {
		got, _ := FillSeries([]Content{clockC(0, 30), clockC(23, 0)}, 200, back, false)
		for _, g := range got {
			if g.Num < 0 || g.Num >= 1 {
				t.Errorf("a time series holds %v", g.Num)
			}
		}
	}
	if got, _ := FillSeries(nil, 0, false, true); got != nil {
		t.Errorf("no target gives %v", got)
	}
}

// Every date and time a series writes is stored content of technical
// design section 4.3: the content that typed entry gives for the text it
// shows. The sources start at every quarter hour of a day and fill ten
// cells down and up.
func TestFillSeriesWritesStoredContent(t *testing.T) {
	for start := 0; start < daySeconds; start += 900 {
		clock := func(secs int) string {
			secs %= daySeconds
			return fmt.Sprintf("%02d:%02d:%02d", secs/3600, secs/60%60, secs%60)
		}
		sources := [][]string{
			{clock(start)},
			{clock(start), clock(start + 3600)},
			{clock(start), clock(start + 1200)},
			{clock(start), clock(start + 1200), clock(start + 2700)},
			{"2026-03-15 " + clock(start)},
			{"2026-03-15 " + clock(start), "2026-03-15 " + clock(start+3600)},
			{"1900-01-25 " + clock(start), "1900-01-25 " + clock(start+1200)},
		}
		for _, texts := range sources {
			src := make([]Content, len(texts))
			for i, s := range texts {
				src[i] = typedC(s)
			}
			for _, back := range []bool{false, true} {
				got, ok := FillSeries(src, 10, back, false)
				if !ok {
					t.Errorf("%q back %v: no series", texts, back)
					continue
				}
				for i, c := range got {
					if shown := Show(c.Value(), c.Kind, NumberFormat{}); !sameAsTyped(c, shown) {
						t.Errorf("%q back %v: cell %d holds %v, which shows %s, and a typed %s holds %v", texts, back, i, c.Num, shown, shown, typedC(shown).Num)
					}
				}
			}
		}
	}
}

// FillSeries is linear in the source and the target.
func TestFillSeriesIsLinear(t *testing.T) {
	src := make([]Content, 50000)
	for i := range src {
		src[i] = textC(fmt.Sprintf("Row%05d", 2*i))
	}
	got, ok := FillSeries(src, 50000, false, false)
	if !ok || len(got) != 50000 || got[0].Text != "Row100000" || got[49999].Text != "Row199998" {
		t.Fatalf("the series of %d texts: %v, %d contents", len(src), ok, len(got))
	}
}

package formula

import "strconv"

// The calendar is the proleptic Gregorian one. Day 0 is 1899-12-30, so day 2
// is 1900-01-01 and there is no 1900-02-29 (spec rule 4).

const (
	epochDays   = -25569  // 1899-12-30 counted from 1970-01-01
	lastDateDay = 2958465 // 9999-12-31
	daySeconds  = 86400
)

var monthNames = [12]string{"Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"}

// daysFromCivil counts the days from 1970-01-01 to a calendar date.
func daysFromCivil(y, m, d int64) int64 {
	if m <= 2 {
		y--
	}
	era := y / 400
	if y < 0 {
		era = (y - 399) / 400
	}
	yoe := y - era*400
	mp := m - 3
	if m <= 2 {
		mp = m + 9
	}
	doy := (153*mp+2)/5 + d - 1
	doe := yoe*365 + yoe/4 - yoe/100 + doy
	return era*146097 + doe - 719468
}

// serialOf returns the day number of a calendar date. The day may lie
// outside its month.
func serialOf(y, m, d int64) int64 { return daysFromCivil(y, m, d) - epochDays }

// civilOf returns the calendar date of a day number.
func civilOf(serial int64) (y, m, d int64) {
	z := serial + epochDays + 719468
	era := z / 146097
	if z < 0 {
		era = (z - 146096) / 146097
	}
	doe := z - era*146097
	yoe := (doe - doe/1460 + doe/36524 - doe/146096) / 365
	y = yoe + era*400
	doy := doe - (365*yoe + yoe/4 - yoe/100)
	mp := (5*doy + 2) / 153
	d = doy - (153*mp+2)/5 + 1
	m = mp + 3
	if mp >= 10 {
		m = mp - 9
	}
	if m <= 2 {
		y++
	}
	return y, m, d
}

func monthLen(y, m int64) int64 {
	switch m {
	case 2:
		if y%4 == 0 && (y%100 != 0 || y%400 == 0) {
			return 29
		}
		return 28
	case 4, 6, 9, 11:
		return 30
	}
	return 31
}

// validDate reports whether typed entry reads the date: a real day of the
// years 1900 to 9999.
func validDate(y, m, d int64) bool {
	return y >= 1900 && y <= 9999 && m >= 1 && m <= 12 && d >= 1 && d <= monthLen(y, m)
}

// DateOf returns the day number of a calendar date.
func DateOf(year, month, day int) Date {
	return Date(serialOf(int64(year), int64(month), int64(day)))
}

// ParseDate reads a date written as yyyy-mm-dd.
func ParseDate(s string) (Date, bool) {
	serial, pattern, ok := readDate(s)
	if !ok || pattern != DateISO {
		return 0, false
	}
	return Date(serial), true
}

// Civil returns the calendar date of d.
func (d Date) Civil() (year, month, day int) {
	y, m, dd := civilOf(int64(d))
	return int(y), int(m), int(dd)
}

// String returns d as yyyy-mm-dd.
func (d Date) String() string {
	var buf [16]byte
	return string(appendDate(buf[:0], int64(d), DateISO))
}

func appendPadded(dst []byte, n int64, width int) []byte {
	var buf [20]byte
	s := strconv.AppendInt(buf[:0], n, 10)
	for i := len(s); i < width; i++ {
		dst = append(dst, '0')
	}
	return append(dst, s...)
}

func appendDate(dst []byte, serial int64, p DatePattern) []byte {
	y, m, d := civilOf(serial)
	switch p {
	case DateMDY:
		dst = strconv.AppendInt(dst, m, 10)
		dst = append(dst, '/')
		dst = strconv.AppendInt(dst, d, 10)
		dst = append(dst, '/')
		return appendPadded(dst, y, 4)
	case DateMonth:
		dst = append(dst, monthNames[m-1]...)
		dst = append(dst, ' ')
		dst = strconv.AppendInt(dst, d, 10)
		dst = append(dst, ',', ' ')
		return appendPadded(dst, y, 4)
	}
	dst = appendPadded(dst, y, 4)
	dst = append(dst, '-')
	dst = appendPadded(dst, m, 2)
	dst = append(dst, '-')
	return appendPadded(dst, d, 2)
}

func appendTime(dst []byte, secs int, p TimePattern) []byte {
	h, mi, s := int64(secs/3600), int64(secs/60%60), int64(secs%60)
	if p == TimeAMPM {
		h12 := h % 12
		if h12 == 0 {
			h12 = 12
		}
		dst = strconv.AppendInt(dst, h12, 10)
		dst = append(dst, ':')
		dst = appendPadded(dst, mi, 2)
		if h < 12 {
			return append(dst, " AM"...)
		}
		return append(dst, " PM"...)
	}
	dst = appendPadded(dst, h, 2)
	dst = append(dst, ':')
	dst = appendPadded(dst, mi, 2)
	if p == TimeHMS {
		dst = append(dst, ':')
		dst = appendPadded(dst, s, 2)
	}
	return dst
}

var datePatternText = [...]string{DateISO: "yyyy-mm-dd", DateMDY: "m/d/yyyy", DateMonth: "mmm d, yyyy"}
var timePatternText = [...]string{TimeHM: "hh:mm", TimeHMS: "hh:mm:ss", TimeAMPM: "h:mm AM/PM"}

// String returns the pattern as rule 6 writes it, such as "m/d/yyyy".
func (p DatePattern) String() string {
	if int(p) < len(datePatternText) {
		return datePatternText[p]
	}
	return ""
}

// String returns the pattern as rule 6 writes it, such as "h:mm AM/PM".
func (p TimePattern) String() string {
	if int(p) < len(timePatternText) {
		return timePatternText[p]
	}
	return ""
}

func parseDatePattern(s string) (DatePattern, bool) {
	for i, t := range datePatternText {
		if s == t {
			return DatePattern(i), true
		}
	}
	return 0, false
}

func parseTimePattern(s string) (TimePattern, bool) {
	for i, t := range timePatternText {
		if s == t {
			return TimePattern(i), true
		}
	}
	return 0, false
}

package formula

import "strings"

// AppendCSVField appends one CSV field for the text a cell shows (spec
// rule 60): with a leading apostrophe where a program could read the text
// as a formula, and in quotation marks where the field needs them.
func AppendCSVField(dst []byte, shown string) []byte {
	t := strings.TrimLeft(shown, " ")
	apostrophe := false
	if t != "" {
		switch t[0] {
		case '=', '+', '@', '\t', '\r', '\n':
			apostrophe = true
		case '-':
			apostrophe = !csvNumber(shown)
		}
	}
	if !apostrophe && !strings.ContainsAny(shown, ",;\t\"\r\n") {
		return append(dst, shown...)
	}
	if !strings.ContainsAny(shown, ",;\t\"\r\n") {
		dst = append(dst, '\'')
		return append(dst, shown...)
	}
	dst = append(dst, '"')
	if apostrophe {
		dst = append(dst, '\'')
	}
	for i := 0; i < len(shown); i++ {
		if shown[i] == '"' {
			dst = append(dst, '"')
		}
		dst = append(dst, shown[i])
	}
	return append(dst, '"')
}

// csvNumber reports whether a whole field that starts with a minus sign
// reads as a number by rule 5: at most one currency sign, digits with
// optional commas between groups of three, optional decimals, and then an
// exponent or a percent sign, or neither.
func csvNumber(field string) bool {
	p, ok := scanNumber(field)
	return ok && p.neg
}

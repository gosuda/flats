package notebook

import "strings"

const positionDigits = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// Trailing zeroes are excluded so a lexical interval always has a midpoint.
func positionBetween(a, b string) (string, error) {
	if b != "" && a >= b {
		return "", invalidStructure()
	}
	return positionMidpoint(a, b), nil
}

func positionMidpoint(a, b string) string {
	if b != "" {
		n := 0
		for n < len(b) {
			c := byte('0')
			if n < len(a) {
				c = a[n]
			}
			if c != b[n] {
				break
			}
			n++
		}
		if n > 0 {
			rest := ""
			if n < len(a) {
				rest = a[n:]
			}
			return b[:n] + positionMidpoint(rest, b[n:])
		}
	}
	da, db := 0, len(positionDigits)
	if a != "" {
		da = strings.IndexByte(positionDigits, a[0])
	}
	if b != "" {
		db = strings.IndexByte(positionDigits, b[0])
	}
	if db-da > 1 {
		return string(positionDigits[(da+db)/2])
	}
	if len(b) > 1 {
		return b[:1]
	}
	rest := ""
	if len(a) > 1 {
		rest = a[1:]
	}
	return string(positionDigits[da]) + positionMidpoint(rest, "")
}

// Allocate around midpoints so a batch needs logarithmic key growth.
func positionsBetween(left, right string, count int) ([]string, error) {
	keys := make([]string, count)
	var fill func(int, int, string, string) error
	fill = func(start, end int, left, right string) error {
		if start == end {
			return nil
		}
		mid := start + (end-start)/2
		key, err := positionBetween(left, right)
		if err != nil {
			return err
		}
		if len(key) > MaxPositionBytes {
			return invalidStructure()
		}
		keys[mid] = key
		if err := fill(start, mid, left, key); err != nil {
			return err
		}
		return fill(mid+1, end, key, right)
	}
	if err := fill(0, count, left, right); err != nil {
		return nil, err
	}
	return keys, nil
}

func rebalancePositions(n int) []string {
	width, span := 1, len(positionDigits)
	for span <= n+1 {
		width++
		span *= len(positionDigits)
	}
	result := make([]string, n)
	for i := range result {
		v := (i + 1) * span / (n + 1)
		buf := make([]byte, width)
		for j := width - 1; j >= 0; j-- {
			buf[j] = positionDigits[v%len(positionDigits)]
			v /= len(positionDigits)
		}
		result[i] = strings.TrimRight(string(buf), "0")
	}
	return result
}

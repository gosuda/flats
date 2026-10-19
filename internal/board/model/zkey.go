package model

import (
	"errors"
	"strings"
)

// Stacking keys are fractional indexes: base62 digits after an implicit
// "0.", compared as strings. A key never ends with the zero digit, so string
// order equals numeric order and every pair of distinct keys has a key
// between them.
const zDigits = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

const zBase = len(zDigits)

func zDigit(c byte) int { return strings.IndexByte(zDigits, c) }

// ValidKey reports whether k is a stored stacking key.
func ValidKey(k string) bool { return k != "" && len(k) <= MaxZLen && wellFormedKey(k) }

func validKeyInput(k string) bool { return k != "" && len(k) <= maxZInput && wellFormedKey(k) }

func wellFormedKey(k string) bool {
	for i := 0; i < len(k); i++ {
		if zDigit(k[i]) < 0 {
			return false
		}
	}
	return k[len(k)-1] != '0'
}

// ErrKeyOrder refuses KeyBetween when a is not below b.
var ErrKeyOrder = errors.New("stacking keys out of order")

// KeyBetween returns a key strictly between a and b. An empty a means the
// bottom and an empty b the top.
func KeyBetween(a, b string) (string, error) {
	if b != "" && a >= b {
		return "", ErrKeyOrder
	}
	return midpoint(a, b), nil
}

func midpoint(a, b string) string {
	if b != "" {
		n := 0
		for n < len(b) {
			ca := byte('0')
			if n < len(a) {
				ca = a[n]
			}
			if ca != b[n] {
				break
			}
			n++
		}
		if n > 0 {
			rest := ""
			if n < len(a) {
				rest = a[n:]
			}
			return b[:n] + midpoint(rest, b[n:])
		}
	}
	da, db := 0, zBase
	if a != "" {
		da = zDigit(a[0])
	}
	if b != "" {
		db = zDigit(b[0])
	}
	if db-da > 1 {
		return string(zDigits[(da+db)/2])
	}
	if len(b) > 1 {
		return b[:1]
	}
	rest := ""
	if len(a) > 1 {
		rest = a[1:]
	}
	return string(zDigits[da]) + midpoint(rest, "")
}

// Rekey returns n short, evenly spread keys in increasing order.
func Rekey(n int) []string {
	width, span := 1, zBase
	for span <= n+1 {
		width++
		span *= zBase
	}
	keys := make([]string, n)
	for i := range keys {
		v := (i + 1) * span / (n + 1)
		buf := make([]byte, width)
		for j := width - 1; j >= 0; j-- {
			buf[j] = zDigits[v%zBase]
			v /= zBase
		}
		keys[i] = strings.TrimRight(string(buf), "0")
	}
	return keys
}

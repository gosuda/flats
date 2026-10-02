// Package slug validates flat names and builds preview host names.
package slug

import (
	"crypto/rand"
	"errors"
	"fmt"
)

// MinLen and MaxLen bound a flat slug. MaxLen leaves room for a preview
// suffix ("-" plus RandomLen characters) inside one 63-byte DNS label.
const (
	MinLen    = 3
	MaxLen    = 54
	RandomLen = 8
)

// ErrInvalid is wrapped by every validation error.
var ErrInvalid = errors.New("invalid slug")

// Validate reports whether s is a valid flat slug: 3-54 characters of
// [a-z0-9] and single hyphens, starting with a letter and not ending with a
// hyphen.
func Validate(s string) error {
	if len(s) < MinLen || len(s) > MaxLen {
		return fmt.Errorf("%w: %q must be %d-%d characters long", ErrInvalid, s, MinLen, MaxLen)
	}
	if s[0] < 'a' || s[0] > 'z' {
		return fmt.Errorf("%w: %q must start with a lowercase letter", ErrInvalid, s)
	}
	if s[len(s)-1] == '-' {
		return fmt.Errorf("%w: %q must not end with a hyphen", ErrInvalid, s)
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-':
			if s[i-1] == '-' {
				return fmt.Errorf("%w: %q must not contain consecutive hyphens", ErrInvalid, s)
			}
		default:
			return fmt.Errorf("%w: %q may only contain lowercase letters, digits and single hyphens", ErrInvalid, s)
		}
	}
	return nil
}

const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

// Random returns n random characters from [a-z0-9].
func Random(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}

// PreviewHost returns the host label of a new preview for slug.
func PreviewHost(s string) string {
	return s + "-" + Random(RandomLen)
}

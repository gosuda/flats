// Package winname knows which file names Windows cannot store under the name
// they carry. It has no build tag, so its rules are tested on every platform;
// callers apply it only on Windows.
package winname

import (
	"errors"
	"fmt"
	"strings"
)

var devices = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"CONIN$": true, "CONOUT$": true, "CLOCK$": true,
}

func init() {
	for _, n := range "0123456789¹²³" {
		devices["COM"+string(n)] = true
		devices["LPT"+string(n)] = true
	}
}

// Device reports whether name is, or with an extension resolves to, a device
// on some supported Windows version. Older versions treat NUL.txt as NUL.
func Device(name string) bool {
	stem, _, _ := strings.Cut(name, ".")
	return devices[strings.ToUpper(strings.TrimRight(stem, " "))]
}

// shortAlias reports whether name has the form of an 8.3 alias, such as
// LONGFI~1.TXT. Such a name can address another file of the directory.
func shortAlias(name string) bool {
	stem, _, _ := strings.Cut(name, ".")
	i := strings.LastIndexByte(stem, '~')
	if i < 1 || i == len(stem)-1 || len(stem) > 8 {
		return false
	}
	for _, c := range stem[i+1:] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// CheckSegment returns why Windows cannot store one path segment exactly.
func CheckSegment(seg string) error {
	if seg == "" {
		return errors.New("empty name")
	}
	for _, c := range seg {
		if c < 0x20 || strings.ContainsRune(`<>:"|?*\`, c) {
			return fmt.Errorf("%q has a character that Windows does not allow in a file name", seg)
		}
	}
	if last := seg[len(seg)-1]; last == '.' || last == ' ' {
		return fmt.Errorf("%q ends in a dot or a space, which Windows removes", seg)
	}
	if Device(seg) {
		return fmt.Errorf("%q is a device name on Windows", seg)
	}
	if shortAlias(seg) {
		return fmt.Errorf("%q has the form of a short name on Windows", seg)
	}
	return nil
}

// CheckPath applies CheckSegment to each segment of a slash-separated path.
func CheckPath(p string) error {
	for _, seg := range strings.Split(p, "/") {
		if err := CheckSegment(seg); err != nil {
			return err
		}
	}
	return nil
}

// FoldKey returns the key under which two paths collide on a file system that
// ignores case.
func FoldKey(p string) string { return strings.ToUpper(p) }

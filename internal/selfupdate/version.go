package selfupdate

import "strings"

// Direction compares release versions; development identities are unordered.
func Direction(from, to string) string {
	if !ValidTag(from) || !ValidTag(to) {
		return "unordered"
	}
	c := compareVersion(from, to)
	if c < 0 {
		return "upgrade"
	}
	if c > 0 {
		return "downgrade"
	}
	return "same"
}
func numeric(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
func numberCompare(a, b string) int {
	a = strings.TrimLeft(a, "0")
	b = strings.TrimLeft(b, "0")
	if len(a) < len(b) {
		return -1
	}
	if len(a) > len(b) {
		return 1
	}
	return strings.Compare(a, b)
}
func compareVersion(a, b string) int {
	ac, ap, _ := strings.Cut(strings.TrimPrefix(a, "v"), "-")
	bc, bp, _ := strings.Cut(strings.TrimPrefix(b, "v"), "-")
	aa, bb := strings.Split(ac, "."), strings.Split(bc, ".")
	for i := range aa {
		if c := numberCompare(aa[i], bb[i]); c != 0 {
			return c
		}
	}
	if ap == bp {
		return 0
	}
	if ap == "" {
		return 1
	}
	if bp == "" {
		return -1
	}
	aa, bb = strings.Split(ap, "."), strings.Split(bp, ".")
	for i := 0; i < len(aa) && i < len(bb); i++ {
		an, bn := numeric(aa[i]), numeric(bb[i])
		c := strings.Compare(aa[i], bb[i])
		if an && bn {
			c = numberCompare(aa[i], bb[i])
		} else if an {
			c = -1
		} else if bn {
			c = 1
		}
		if c != 0 {
			return c
		}
	}
	if len(aa) < len(bb) {
		return -1
	}
	return 1
}

package slug

import "testing"

func TestWindowsRefusesDeviceNames(t *testing.T) {
	for _, s := range []string{"con", "prn", "aux", "nul", "com0", "com1", "com9", "lpt0", "lpt1", "lpt9"} {
		if err := Validate(s); err == nil {
			t.Errorf("%q was accepted", s)
		}
	}
	for _, s := range []string{"console", "nulls", "com10", "lpt-1", "auxiliary"} {
		if err := Validate(s); err != nil {
			t.Errorf("%q: %v", s, err)
		}
	}
}

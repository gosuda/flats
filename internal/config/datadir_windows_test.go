package config

import "testing"

func TestDataDirIsCleanOnWindows(t *testing.T) {
	for in, want := range map[string]string{
		`C:\data\flats`:        `C:\data\flats`,
		`C:\data\flats\`:       `C:\data\flats`,
		`C:/data/flats`:        `C:\data\flats`,
		`C:\data\..\flats\.\x`: `C:\flats\x`,
		`\\server\share\flats`: `\\server\share\flats`,
	} {
		if got := cleanDataDir(in); got != want {
			t.Errorf("cleanDataDir(%q) = %q, want %q", in, got, want)
		}
	}
}

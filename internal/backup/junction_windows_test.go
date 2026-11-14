//go:build windows

package backup

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// A directory junction needs no privilege on Windows, unlike a symbolic link, so this is the case of rule 9 that
// the Windows job can run: the junction is skipped and listed, and the command does not enter it.
func TestAJunctionIsSkippedAndNotEntered(t *testing.T) {
	t.Parallel()
	f := populated(t)
	outside := filepath.Join(f.root, "outside-dir")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("must not be copied"), 0o600); err != nil {
		t.Fatal(err)
	}
	junction := filepath.Join(f.data, "flats", "s", "data", "files", "junction")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", junction, outside).CombinedOutput(); err != nil {
		t.Skipf("cannot make a junction here: %v: %s", err, out)
	}
	res, out := f.mustRun("c1")
	skipped := map[string]string{}
	for _, s := range res.Skipped {
		skipped[s.Path] = s.Reason
	}
	const path = "data/flats/s/data/files/junction"
	if skipped[path] == "" {
		t.Errorf("the junction is not listed as skipped: %v", skipped)
	}
	mustNotExist(t, filepath.Join(out, filepath.FromSlash(path)))
	if readCopyManifest(t, out).has(path + "/secret.txt") {
		t.Error("the command entered the junction")
	}
	t.Logf("the junction is listed as skipped with the kind %q", skipped[path])
}

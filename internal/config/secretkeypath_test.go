package config

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// host.secret_key_path keeps the host key out of the data directory (docs/spec/secret-key-path.md, rules 1 and 2).
func TestSecretKeyPathValidation(t *testing.T) {
	t.Parallel()
	const key = "/etc/flats/secret.key"
	l, err := Parse([]byte(fileWith("host", `"secret_key_path": "`+key+`"`)))
	if err != nil {
		t.Fatal(err)
	}
	c, err := l.Doc.Effective(nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Clean(key); c.Host.SecretKeyPath != want {
		t.Errorf("SecretKeyPath = %q, want %q", c.Host.SecretKeyPath, want)
	}
	for name, tc := range map[string]struct {
		file  string
		empty bool // the effective value is empty
	}{
		// Empty is the default and keeps the key in the data directory.
		"absent": {fileWith(), true},
		"empty":  {fileWith("host", `"secret_key_path": ""`), true},
		// A sibling whose name starts with the data directory's name is outside it.
		"sibling": {fileWith("host", `"secret_key_path": "/var/lib/flats-keys/secret.key"`), false},
		// Clean resolves the dots before the comparison.
		"dots": {fileWith("host", `"secret_key_path": "/var/lib/flats/../keys/secret.key"`), false},
	} {
		l, err := Parse([]byte(tc.file))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		c, err := l.Doc.Effective(nil)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if (c.Host.SecretKeyPath == "") != tc.empty {
			t.Errorf("%s: SecretKeyPath = %q", name, c.Host.SecretKeyPath)
		}
	}
	for name, value := range map[string]string{
		"relative":          "keys/secret.key",
		"NUL byte":          `/etc/flats/secret\u0000.key`,
		"inside":            "/var/lib/flats/keys/secret.key",
		"the default path":  "/var/lib/flats/secret.key",
		"the data dir":      "/var/lib/flats",
		"dots lead inside":  "/etc/../var/lib/flats/secret.key",
		"trailing slashes":  "/var/lib/flats/keys//",
		"inside, any depth": "/var/lib/flats/a/b/c/d",
	} {
		_, err := Parse([]byte(fileWith("host", `"secret_key_path": "`+value+`"`)))
		if err == nil {
			t.Errorf("%s: Parse accepted %q", name, value)
			continue
		}
		if got := errPath(err); got != "host.secret_key_path" {
			t.Errorf("%s: error path = %q, want host.secret_key_path (%v)", name, got, err)
		}
	}
}

// The message of a refused path says what to do and that the file is edited by hand (rule 2).
func TestSecretKeyPathMessage(t *testing.T) {
	t.Parallel()
	_, err := Parse([]byte(fileWith("host", `"secret_key_path": "/var/lib/flats/secret.key"`)))
	if err == nil {
		t.Fatal("Parse accepted a key path inside the data directory")
	}
	for _, want := range []string{"outside host.data_dir", "leave it empty", "edit config.json by hand"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the message lacks %q: %v", want, err)
		}
	}
}

func TestSecretKeyPathSetUnset(t *testing.T) {
	t.Parallel()
	doc, err := New(testID, "/var/lib/flats")
	if err != nil {
		t.Fatal(err)
	}
	if err := doc.Set("host.secret_key_path", "/etc/flats/secret.key"); err != nil {
		t.Fatal(err)
	}
	if err := doc.Validate(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(doc.Encode()), `"secret_key_path": "/etc/flats/secret.key"`) {
		t.Errorf("Encode does not hold the key path:\n%s", doc.Encode())
	}
	if !strings.Contains(string(doc.Encode()), `"schema_version": 5`) {
		t.Errorf("the schema version changed:\n%s", doc.Encode())
	}
	// Set checks one key. Validate runs the rule that spans two keys: a new data directory that contains the key path.
	if err := doc.Set("host.data_dir", "/etc/flats"); err != nil {
		t.Fatal(err)
	}
	if err := doc.Validate(); errPath(err) != "host.secret_key_path" {
		t.Errorf("Validate = %v, want a refusal at host.secret_key_path", err)
	}
	if err := doc.Set("host.data_dir", "/var/lib/flats"); err != nil {
		t.Fatal(err)
	}
	if err := doc.Set("host.secret_key_path", "/var/lib/flats/secret.key"); err != nil {
		t.Fatal(err)
	}
	if err := doc.Validate(); errPath(err) != "host.secret_key_path" {
		t.Errorf("Validate accepted the default path: %v", err)
	}
	if err := doc.Unset("host.secret_key_path"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(doc.Encode()), "secret_key_path") {
		t.Errorf("Unset left the key:\n%s", doc.Encode())
	}
	if err := doc.Validate(); err != nil {
		t.Errorf("Validate after Unset: %v", err)
	}
}

// `flats config set host.data_dir` saves through the writer, which validates the whole file. A directory that
// contains the key path is refused, and the file and its history stay as they were (rule 2).
func TestSaveRefusesADataDirThatContainsTheKeyPath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	orig := fileWith("host", `"secret_key_path": "/etc/flats/secret.key"`)
	hash := writeFile(t, path, orig)
	_, err := NewWriter(path).Save(hash, func(d *Document) error { return d.Set("host.data_dir", "/etc/flats") })
	if errPath(err) != "host.secret_key_path" {
		t.Fatalf("Save = %v, want a refusal at host.secret_key_path", err)
	}
	if readString(t, path) != orig || historyFiles(t, dir) != nil {
		t.Error("a refused save changed the file or its history")
	}
}

// An older release refuses the new key because the parser refuses any key it does not know (rule 23). The test
// builds no older binary. It shows that the rule holds in the host object.
func TestUnknownHostKeyIsRefused(t *testing.T) {
	t.Parallel()
	if _, err := Parse([]byte(fileWith("host", `"secret_key_file": "/etc/flats/secret.key"`))); err == nil {
		t.Error("Parse accepted an unknown key in the host object")
	}
}

func TestInsideDir(t *testing.T) {
	t.Parallel()
	cases := []struct {
		dir, child string
		want       bool
	}{
		{"/a/b", "/a/b", true},
		{"/a/b", "/a/b/c", true},
		{"/a/b", "/a/b/../b/c", true},
		{"/a/b", "/a/bc", false},
		{"/a/b", "/a", false},
		{"/a/b", "/x/a/b", false},
	}
	for _, tc := range cases {
		if got := InsideDir(filepath.FromSlash(tc.dir), filepath.FromSlash(tc.child)); got != tc.want {
			t.Errorf("InsideDir(%q, %q) = %v, want %v", tc.dir, tc.child, got, tc.want)
		}
	}
	if runtime.GOOS == "windows" {
		if !InsideDir(`C:\Data\Flats`, `c:\data\flats\SECRET.KEY`) {
			t.Error("Windows paths that differ in case only are different")
		}
	} else if InsideDir("/Data/Flats", "/data/flats/secret.key") {
		t.Error("paths that differ in case are the same")
	}
}

package core

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"

	"github.com/gosuda/flats/internal/config"
	"github.com/gosuda/flats/internal/durable"
)

// KeySetting is the name of the setting that moves the host key, as messages name it.
const KeySetting = "host.secret_key_path"

// ErrKeyLocation reports that host.secret_key_path names a key that the host must not use or cannot
// read: nothing at the path, a second key at <data>/secret.key, a key that resolves into the data
// directory, a file that is no key, or a file that cannot be read. Flats never creates a key at a
// configured path (docs/spec/secret-key-path.md).
var ErrKeyLocation = errors.New("the configured secret key cannot be used")

// KeyFile is where the host key lives: <data>/secret.key, or the file that host.secret_key_path
// names. Everything that reads or writes the key gets its path from here.
type KeyFile struct {
	dataDir    string
	path       string
	configured bool
}

// NewKeyFile returns the location of the key of the data directory dataDir. configured is
// host.secret_key_path, and "" keeps the key at <data>/secret.key. It touches no file.
func NewKeyFile(dataDir, configured string) KeyFile {
	if configured == "" {
		return KeyFile{dataDir: dataDir, path: filepath.Join(dataDir, SecretKeyFile)}
	}
	return KeyFile{dataDir: dataDir, path: configured, configured: true}
}

// Path is the file that holds the key.
func (k KeyFile) Path() string { return k.path }

// Configured reports whether host.secret_key_path chose the path.
func (k KeyFile) Configured() bool { return k.configured }

// DataDir is the data directory that the key belongs to.
func (k KeyFile) DataDir() string { return k.dataDir }

// Check makes the two checks of a configured key that the lexical check of the setting cannot make
// (rule 3 of the spec). It refuses a file, directory or link at <data>/secret.key, because two keys
// would be in use and a copy of the data directory would hold the one that the setting keeps out. It
// also refuses a key whose resolved path lies inside the resolved data directory. A default key has
// nothing to check.
func (k KeyFile) Check() error {
	if !k.configured {
		return nil
	}
	leftover := filepath.Join(k.dataDir, SecretKeyFile)
	if _, err := os.Lstat(leftover); err == nil {
		if sameFile(leftover, k.path) {
			return fmt.Errorf("%w: %s names %s, which is the same file as %s, so the data directory holds the key; keep the key in a folder that no link reaches from the data directory",
				ErrKeyLocation, KeySetting, k.path, leftover)
		}
		return fmt.Errorf("%w: %s is set, and %s exists too; move that file away, because Flats uses the configured key and a copy of the data directory would hold the old one",
			ErrKeyLocation, KeySetting, leftover)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%w: look at %s: %v", ErrKeyLocation, leftover, err)
	}
	real, err := filepath.EvalSymlinks(k.path)
	if err != nil {
		return nil // nothing to resolve; Read says what is wrong with the path
	}
	dataReal, err := filepath.EvalSymlinks(k.dataDir)
	if err != nil {
		return nil
	}
	if config.InsideDir(dataReal, real) {
		return fmt.Errorf("%w: %s names %s, which resolves to %s inside the data directory %s; keep the key outside the data directory",
			ErrKeyLocation, KeySetting, k.path, real, dataReal)
	}
	return nil
}

// Read reads the key. With a configured path it makes the checks of Check first and refuses with an
// error that names the path and the setting. The default path gets the checks that the host has
// always made.
func (k KeyFile) Read() ([]byte, error) {
	if !k.configured {
		return readSecretKey(k.path)
	}
	if err := k.Check(); err != nil {
		return nil, err
	}
	return k.readConfigured()
}

// ReadUnchecked reads the key without the checks of Check. A backup makes those checks once, in its
// preflight, and then reads the key twice (rule 22 of the backup spec). A second key that appears in the data
// directory during the copy is left out of the copy and does not fail those reads.
func (k KeyFile) ReadUnchecked() ([]byte, error) {
	if !k.configured {
		return readSecretKey(k.path)
	}
	return k.readConfigured()
}

// readConfigured reads the file of a configured key and names the path and the setting in every refusal.
func (k KeyFile) readConfigured() ([]byte, error) {
	if _, err := os.Stat(k.path); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s names %s: %v", ErrKeyLocation, KeySetting, k.path, err)
		}
		if _, lerr := os.Lstat(k.path); lerr == nil {
			return nil, fmt.Errorf("%w: %s names %s, which is a link to nothing; repair the link or the mount, or clear the setting with `flats config unset %s`, and Flats never creates a key at a configured path",
				ErrKeyLocation, KeySetting, k.path, KeySetting)
		}
		return nil, fmt.Errorf("%w: %s names %s, and nothing is there. Flats never creates a key at a configured path. Put a key of 32 bytes there%s, restore it with `flats secret restore-key`, or clear the setting with `flats config unset %s`",
			ErrKeyLocation, KeySetting, k.path, newKeyHint(k.path), KeySetting)
	}
	b, err := readSecretKey(k.path)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrKeyLocation, KeySetting, err)
	}
	return b, nil
}

// LoadOrCreate reads the key. A default key is created when there is none. A configured key never is:
// a missing file is a refusal.
func (k KeyFile) LoadOrCreate() ([]byte, error) {
	if k.configured {
		return k.Read()
	}
	return loadOrCreateKey(k.path)
}

// Restore writes key at the path through a synced temporary file beside it, and never replaces
// anything there: os.IsExist reports that the name is taken. With a configured path the temporary
// file lies in the folder of that path.
func (k KeyFile) Restore(key []byte) error {
	if err := publishKey(k.path, key); err != nil {
		return err
	}
	return durable.SyncDir(filepath.Dir(k.path))
}

// newKeyHint is the shell line that makes a key, for a system that has /dev/urandom. Windows gets none: the
// docs give a PowerShell line there.
func newKeyHint(path string) string {
	if runtime.GOOS == "windows" {
		return ""
	}
	return fmt.Sprintf(" (for example `head -c 32 /dev/urandom > %s`)", path)
}

// sameFile reports whether a and b are the same file once links are followed.
func sameFile(a, b string) bool {
	fa, err := os.Stat(a)
	if err != nil {
		return false
	}
	fb, err := os.Stat(b)
	return err == nil && os.SameFile(fa, fb)
}

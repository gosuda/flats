package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/term"

	"github.com/gosuda/flats/internal/config"
	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/prompt"
	"github.com/gosuda/flats/internal/store"
)

// Recovery password limits.
const (
	minPasswordChars = 12
	maxPasswordBytes = 1024
)

// recoveryWorkFactor is the scrypt work factor of new recovery copies. Tests
// lower it: one copy at the real factor takes a second, and ten under -race.
var recoveryWorkFactor = core.RecoveryWorkFactor

// SecretKeyCommand runs `flats secret backup-key|restore-key|reset` (see
// docs/spec/secret-key-recovery.md). backup-key works beside a running host;
// restore-key and reset take the host lock.
func SecretKeyCommand(ctx context.Context, args []string, in io.Reader, out io.Writer) error {
	if len(args) == 0 {
		return usagef("missing subcommand")
	}
	k := &secretKeyCmd{configCmd: configCmd{ctx: ctx, out: out}, in: in}
	switch name, rest := args[0], args[1:]; name {
	case "backup-key":
		return k.backup(rest)
	case "restore-key":
		return k.restore(rest)
	case "reset":
		return k.reset(rest)
	default:
		return usagef("unknown subcommand %q", name)
	}
}

type secretKeyCmd struct {
	configCmd
	in io.Reader
}

func (k *secretKeyCmd) backup(args []string) error {
	f := k.flags("backup-key")
	outFlag := f.String("out", "", "also write the copy to this new file")
	passwordFile := f.String("password-file", "", "read the recovery password from this file")
	if err := k.parseFlags(args); err != nil {
		return err
	}
	dataDir, keyFile, err := k.location()
	if err != nil {
		return err
	}
	keyPath := keyFile.Path()
	if _, err := os.Lstat(keyPath); errors.Is(err, fs.ErrNotExist) && !keyFile.Configured() {
		return actionf("%s is missing: start the host once to create it, or restore it with `flats secret restore-key`", keyPath)
	}
	key, err := readHostKey(keyFile)
	if err != nil {
		return err
	}
	outPath := ""
	if *outFlag != "" {
		if outPath, err = outFile(*outFlag, dataDir); err != nil {
			return err
		}
	}
	password, err := k.password(*passwordFile, true)
	if err != nil {
		return err
	}
	data, err := core.SealKeyCopy(key, password, recoveryWorkFactor)
	if err != nil {
		return err
	}
	if err := k.ctx.Err(); err != nil {
		return interrupted(err)
	}
	replaced, err := core.WriteKeyCopy(dataDir, data, key)
	if err != nil {
		return err
	}
	verb := "wrote"
	if replaced {
		verb = "replaced"
	}
	k.printf("%s %s and its key ID in %s\n", verb, filepath.Join(dataDir, core.RecoveryKeyFile), core.RecoveryKeyIDFile)
	if outPath != "" {
		if err := writeNewFile(outPath, data); err != nil {
			return fmt.Errorf("--out %s: %w; the copy in %s stays", outPath, err, dataDir)
		}
		k.printf("wrote %s\n", outPath)
	}
	k.printf("Flats keeps no copy of the password. Without it the recovery file opens nothing; keep it apart from this host.\n")
	return nil
}

// outFile checks --out before anything is written: a new file in an existing
// folder other than the data directory, whose own copy and ID would collide.
func outFile(flag, dataDir string) (string, error) {
	path, err := cleanPath(flag)
	if err != nil {
		return "", actionf("--out: %v", err)
	}
	if _, err := os.Lstat(path); err == nil {
		return "", actionf("--out: %s exists; name a new file", path)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", actionf("--out: %v", err)
	}
	dir := filepath.Dir(path)
	fi, err := os.Stat(dir)
	if err != nil {
		return "", actionf("--out: %v", err)
	}
	if !fi.IsDir() {
		return "", actionf("--out: %s is not a folder", dir)
	}
	if data, err := os.Stat(dataDir); err == nil && os.SameFile(fi, data) {
		return "", actionf("--out: name a file outside the data directory %s, which holds its own copy", dataDir)
	}
	return path, nil
}

// readHostKey reads the host key for backup-key and reset. With a configured path it first makes the checks
// of rule 3 of the spec. A key that is no key (the wrong size, a directory, a link to nothing) is a refusal
// that names the path and says to move it away. Any other failure to read a configured key is a refusal too.
func readHostKey(keyFile core.KeyFile) ([]byte, error) {
	if err := keyFile.Check(); err != nil {
		return nil, &ActionError{err}
	}
	key, err := keyFile.ReadUnchecked()
	switch {
	case err == nil:
		return key, nil
	case keyUnusable(err, keyFile.Path()):
		return nil, actionf("%v: move %s away first, because the next start would stop on it", err, keyFile.Path())
	case keyFile.Configured():
		return nil, &ActionError{err}
	}
	return nil, err
}

// keyUnusable reports whether err from KeyFile.Read means that what is
// at keyPath is no key: the wrong size or type, a target that does not exist
// (also behind a Windows junction), or a link that leads to no file through a
// loop or a file on its path. A permission error is not, and stays a failure
// with status 1.
func keyUnusable(err error, keyPath string) bool {
	switch {
	case err == nil, errors.Is(err, fs.ErrPermission):
		return false
	case errors.Is(err, core.ErrInvalidSecretKey), errors.Is(err, fs.ErrNotExist):
		return true
	}
	fi, lerr := os.Lstat(keyPath)
	return lerr == nil && fi.Mode()&fs.ModeSymlink != 0
}

func (k *secretKeyCmd) restore(args []string) error {
	f := k.flags("restore-key")
	from := f.String("from", "", "recovery file (default: <data>/secret.key.recovery)")
	passwordFile := f.String("password-file", "", "read the recovery password from this file")
	outFlag := f.String("out", "", "write the recovered key to this new file instead of the key path")
	if err := k.parseFlags(args); err != nil {
		return err
	}
	dataDir, keyFile, st, done, err := k.openOffline()
	if err != nil {
		return err
	}
	defer done()
	keyPath := keyFile.Path()
	// A second key at <data>/secret.key is a refusal, with or without --out.
	if err := keyFile.Check(); err != nil {
		return &ActionError{err}
	}
	outPath := ""
	if *outFlag != "" {
		// --out leaves the location alone, which may hold a wrong key.
		if outPath, err = keyOutFile(*outFlag, dataDir); err != nil {
			return err
		}
	} else if _, err := os.Lstat(keyPath); err == nil {
		return actionf("%s exists, and restore-key never replaces a key: move it away first if it is wrong", keyPath)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return &ActionError{err}
	}
	src := filepath.Join(dataDir, core.RecoveryKeyFile)
	if *from != "" {
		if src, err = cleanPath(*from); err != nil {
			return actionf("--from: %v", err)
		}
	}
	data, err := readLimited(src, core.MaxRecoveryFileSize)
	if errors.Is(err, fs.ErrNotExist) && *from == "" {
		return actionf("there is no recovery copy at %s: name a copy kept apart from the host with --from", src)
	}
	if err != nil {
		return actionf("read the recovery file: %v", err)
	}
	password, err := k.password(*passwordFile, false)
	if err != nil {
		return err
	}
	key, err := core.OpenKeyCopy(data, password)
	if err != nil {
		return actionf("%s: %v; nothing was written", src, err)
	}
	check, err := core.CheckKey(k.ctx, st, key)
	if err != nil {
		return err
	}
	if !check.All() {
		return actionf("the key in %s opens %d of the %d sealed values in flats.db; nothing was written", src, check.Opened, check.Total)
	}
	if check.Total == 0 {
		k.printf("flats.db holds no flat secrets or operator keys, so there is nothing to check the key against\n")
	}
	if err := k.ctx.Err(); err != nil {
		return interrupted(err)
	}
	if outPath != "" {
		if err := writeNewFile(outPath, key); err != nil {
			return fmt.Errorf("--out %s: %w; nothing was written at the key path", outPath, err)
		}
		k.printf("wrote the recovered key to %s with mode 0600 from %s; the host reads its key at %s. Put the key there, or in the secret store behind that path, and remove %s\n", outPath, src, keyPath, outPath)
		return nil
	}
	if err := keyFile.Restore(key); err != nil {
		if os.IsExist(err) {
			return actionf("%s appeared while restoring, and restore-key never replaces a key; nothing was written", keyPath)
		}
		if keyFile.Configured() {
			return actionf("cannot write the key at %s: %v; nothing was written. Write it to a new file with --out FILE, and put that file in your secret store", keyPath, err)
		}
		return err
	}
	k.printf("restored %s from %s; the host can start\n", keyPath, src)
	return nil
}

// keyOutFile checks --out of restore-key: the checks of outFile, and the file may not lie
// anywhere inside the data directory, which would put the key back into what a copy holds.
func keyOutFile(flag, dataDir string) (string, error) {
	path, err := outFile(flag, dataDir)
	if err != nil {
		return "", err
	}
	dir, derr := filepath.EvalSymlinks(filepath.Dir(path))
	data, serr := filepath.EvalSymlinks(dataDir)
	if derr == nil && serr == nil && config.InsideDir(data, dir) {
		return "", actionf("--out: name a file outside the data directory %s, because a copy of the data directory would hold the key", dataDir)
	}
	return path, nil
}

func (k *secretKeyCmd) reset(args []string) error {
	f := k.flags("reset")
	yes := f.Bool("yes", false, "delete without asking")
	if err := k.parseFlags(args); err != nil {
		return err
	}
	dataDir, keyFile, st, done, err := k.openOffline()
	if err != nil {
		return err
	}
	defer done()
	secrets, err := st.ListAllSecrets(k.ctx)
	if err != nil {
		return err
	}
	records, err := st.ListHostKeys(k.ctx)
	if err != nil {
		return err
	}
	var values []core.SealedName
	for _, s := range secrets {
		values = append(values, core.SealedName{Flat: s.Flat, Name: s.Name})
	}
	for _, r := range records {
		if r.HasValue() {
			values = append(values, core.SealedName{Name: r.ID})
		}
	}
	total := len(values)
	if total == 0 {
		k.printf("flats.db holds no flat secrets or operator keys; nothing to reset\n")
		return nil
	}
	keyPath := keyFile.Path()
	keyMissing, err := k.resetAllowed(st, keyFile)
	if err != nil {
		return err
	}
	k.printf("This deletes:\n")
	for _, v := range values {
		k.printf("  %s\n", v)
	}
	if !*yes {
		p := openTerminal(k.ctx, k.in, k.out)
		if p == nil {
			return usagef("no terminal to confirm on: run again with --yes to delete these %d values", total)
		}
		answer, err := p.line("Type reset to delete them: ")
		if err != nil {
			return err
		}
		if strings.TrimSpace(answer) != "reset" {
			k.printf("Nothing was deleted.\n")
			return nil
		}
	}
	now := time.Now()
	events := make([]store.Event, 0, len(secrets))
	for _, s := range secrets {
		events = append(events, store.Event{Flat: s.Flat, Time: now, Level: "warn", Kind: "secret",
			Message: fmt.Sprintf("secret %s deleted by `flats secret reset`; set it again and redeploy", s.Name)})
	}
	if err := k.ctx.Err(); err != nil {
		return interrupted(err)
	}
	// Once the delete starts it completes and is made durable, also when an
	// interrupt arrives meanwhile.
	write := context.WithoutCancel(k.ctx)
	if err := st.DeleteSealedValues(write, events); err != nil {
		return err
	}
	// The delete has committed, so the command reports it and the next steps
	// even when the checkpoint after it fails.
	synced := st.Sync(write)
	if total == 1 {
		k.printf("deleted 1 value.\n")
	} else {
		k.printf("deleted %d values.\n", total)
	}
	if keyMissing && keyFile.Configured() {
		k.printf("The next start needs a key at %s, because Flats never creates a key at a configured path. Put a new key of 32 bytes there%s before you start the host.\n", keyPath, core.NewKeyHint(keyPath))
	} else if keyMissing {
		why := "to keep a recovery copy of the new key"
		if _, err := os.Lstat(filepath.Join(dataDir, core.RecoveryKeyFile)); err == nil {
			why = "because the existing " + core.RecoveryKeyFile + " holds the lost key"
		}
		k.printf("The next start creates a new %s. Run `flats secret backup-key` after it %s.\n", keyPath, why)
	}
	k.printf("Set the secrets again with `flats secret set` and redeploy, and the operator keys with `flats key set`.\n")
	if synced != nil {
		return fmt.Errorf("the values are deleted, but %w", synced)
	}
	return nil
}

// resetAllowed checks what is at the key's path: reset goes on only when nothing
// is there, or when a valid key there opens none of the sealed values. With a
// configured path it first makes the checks of rule 3 of the spec.
func (k *secretKeyCmd) resetAllowed(st *store.Store, keyFile core.KeyFile) (keyMissing bool, err error) {
	keyPath := keyFile.Path()
	if err := keyFile.Check(); err != nil {
		return false, &ActionError{err}
	}
	if _, err := os.Lstat(keyPath); errors.Is(err, fs.ErrNotExist) {
		return true, nil
	} else if err != nil {
		return false, err
	}
	key, err := readHostKey(keyFile)
	if err != nil {
		return false, err
	}
	check, err := core.CheckKey(k.ctx, st, key)
	if err != nil {
		return false, err
	}
	switch {
	case check.All():
		return false, actionf("%s opens all %d sealed values, so nothing is lost: remove single values with `flats secret rm` while the host runs, or with `flats key rm`", keyPath, check.Total)
	case check.Opened > 0:
		var keys, secrets []string
		for _, n := range check.Unopened {
			if n.Flat == "" {
				keys = append(keys, n.Name)
			} else {
				secrets = append(secrets, n.String())
			}
		}
		msg := fmt.Sprintf("%s opens %d of the %d sealed values, so reset would delete values that still open. Remove only those it does not open", keyPath, check.Opened, check.Total)
		if len(keys) > 0 {
			msg += fmt.Sprintf(": first, with the host stopped, the operator keys %s with `flats key rm`, because the start refuses while a stored Tailscale or NetBird key does not open", strings.Join(keys, ", "))
		}
		if len(secrets) > 0 {
			msg += fmt.Sprintf("; then start the host and remove %s with `flats secret rm`", strings.Join(secrets, ", "))
		}
		return false, actionf("%s", msg)
	}
	k.printf("%s opens none of the %d sealed values.\n", keyPath, check.Total)
	return false, nil
}

// location resolves the data directory and the host key of the selected
// configuration. A missing or invalid configuration file is a refusal.
func (c *configCmd) location() (string, core.KeyFile, error) {
	configPath, _, err := c.paths()
	if err != nil {
		return "", core.KeyFile{}, err
	}
	_, cfg, err := load(configPath)
	if err != nil {
		return "", core.KeyFile{}, err
	}
	return cfg.Host.DataDir, core.NewKeyFile(cfg.Host.DataDir, cfg.Host.SecretKeyPath), nil
}

// password reads the recovery password from --password-file or the
// terminal, never from an argument. A new password is asked twice and must
// be long enough.
func (k *secretKeyCmd) password(file string, isNew bool) (string, error) {
	var pw string
	switch p := openTerminal(k.ctx, k.in, k.out); {
	case file != "":
		path, err := cleanPath(file)
		if err != nil {
			return "", actionf("--password-file: %v", err)
		}
		// Room for line ends: the length check below runs after trimming.
		b, err := readLimited(path, 4*maxPasswordBytes)
		if err != nil {
			return "", actionf("--password-file: %v", err)
		}
		pw = strings.TrimRight(string(b), "\r\n")
	case p != nil:
		var err error
		if pw, err = p.hidden("Recovery password: "); err != nil {
			return "", err
		}
		if isNew {
			again, err := p.hidden("Repeat the password: ")
			if err != nil {
				return "", err
			}
			if again != pw {
				return "", actionf("the two passwords differ; nothing was written")
			}
		}
	default:
		return "", usagef("no terminal to ask for the password on: use --password-file")
	}
	switch {
	case pw == "":
		return "", actionf("the password is empty; nothing was written")
	case len(pw) > maxPasswordBytes:
		return "", actionf("the password is longer than %d bytes; nothing was written", maxPasswordBytes)
	case isNew && utf8.RuneCountInString(pw) < minPasswordChars:
		return "", actionf("the password has %d characters; use at least %d", utf8.RuneCountInString(pw), minPasswordChars)
	}
	return pw, nil
}

// prompter asks the operator on a terminal.
type prompter interface {
	hidden(question string) (string, error)
	line(question string) (string, error)
}

// openTerminal returns a prompter for in when it is a terminal, else nil.
// Tests replace it.
var openTerminal = func(ctx context.Context, in io.Reader, out io.Writer) prompter {
	f, ok := in.(*os.File)
	if !ok || !term.IsTerminal(int(f.Fd())) {
		return nil
	}
	return &terminal{ctx: ctx, f: f, out: out}
}

type terminal struct {
	ctx context.Context
	f   *os.File
	out io.Writer
}

func (t *terminal) hidden(question string) (string, error) {
	return t.answer(prompt.Hidden(t.ctx, t.f, t.out, question))
}

func (t *terminal) line(question string) (string, error) {
	return t.answer(prompt.Line(t.ctx, t.f, t.out, question))
}

// answer reports a prompt that an interrupt ended as one that wrote nothing.
func (t *terminal) answer(s string, err error) (string, error) {
	if err != nil && t.ctx.Err() != nil {
		return "", interrupted(err)
	}
	return s, err
}

func interrupted(err error) error {
	return fmt.Errorf("interrupted; nothing was written: %w", err)
}

// readLimited reads a whole file of at most limit bytes.
func readLimited(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, limit)
	}
	return b, nil
}

// writeNewFile writes data to a file that must not exist, with mode 0600 on
// Unix. It never replaces a file, also on a volume without hard links, and
// removes what it wrote when a write fails.
func writeNewFile(path string, data []byte) (err error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			os.Remove(path)
		}
	}()
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

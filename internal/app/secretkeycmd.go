package app

import (
	"bufio"
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

	"github.com/gosuda/flats/internal/core"
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
	dataDir, err := k.dataDir()
	if err != nil {
		return err
	}
	keyPath := filepath.Join(dataDir, core.SecretKeyFile)
	if _, err := os.Lstat(keyPath); errors.Is(err, fs.ErrNotExist) {
		return actionf("%s is missing: start the host once to create it, or restore it with `flats secret restore-key`", keyPath)
	}
	key, err := core.ReadSecretKey(dataDir)
	if errors.Is(err, core.ErrInvalidSecretKey) || errors.Is(err, fs.ErrNotExist) {
		return actionf("%v: move %s away first, because the next start would stop on it", err, keyPath)
	}
	if err != nil {
		return err
	}
	outPath := ""
	if *outFlag != "" {
		if outPath, err = cleanPath(*outFlag); err != nil {
			return actionf("--out: %v", err)
		}
		if _, err := os.Lstat(outPath); err == nil {
			return actionf("--out: %s exists; name a new file", outPath)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return actionf("--out: %v", err)
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

func (k *secretKeyCmd) restore(args []string) error {
	f := k.flags("restore-key")
	from := f.String("from", "", "recovery file (default: <data>/secret.key.recovery)")
	passwordFile := f.String("password-file", "", "read the recovery password from this file")
	if err := k.parseFlags(args); err != nil {
		return err
	}
	dataDir, st, done, err := k.openOffline()
	if err != nil {
		return err
	}
	defer done()
	keyPath := filepath.Join(dataDir, core.SecretKeyFile)
	if _, err := os.Lstat(keyPath); err == nil {
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
		return actionf("the key in %s opens %d of the %d sealed values in flats.db, so it is not this host's key; nothing was written", src, check.Opened, check.Total)
	}
	if check.Total == 0 {
		k.printf("flats.db holds no flat secrets or operator keys, so there is nothing to check the key against\n")
	}
	if err := core.RestoreSecretKey(dataDir, key); err != nil {
		if os.IsExist(err) {
			return actionf("%s appeared while restoring, and restore-key never replaces a key; nothing was written", keyPath)
		}
		return err
	}
	k.printf("restored %s from %s; the host can start\n", keyPath, src)
	return nil
}

func (k *secretKeyCmd) reset(args []string) error {
	f := k.flags("reset")
	yes := f.Bool("yes", false, "delete without asking")
	if err := k.parseFlags(args); err != nil {
		return err
	}
	dataDir, st, done, err := k.openOffline()
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
	keyPath := filepath.Join(dataDir, core.SecretKeyFile)
	keyMissing, err := k.resetAllowed(st, dataDir)
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
	if err := st.DeleteSealedValues(k.ctx, events); err != nil {
		return err
	}
	if err := st.Sync(k.ctx); err != nil {
		return err
	}
	if total == 1 {
		k.printf("deleted 1 value.\n")
	} else {
		k.printf("deleted %d values.\n", total)
	}
	if keyMissing {
		k.printf("The next start creates a new %s. Run `flats secret backup-key` after it: an existing %s holds the lost key.\n", keyPath, core.RecoveryKeyFile)
	}
	k.printf("Set the secrets again with `flats secret set` and redeploy, and the operator keys with `flats key set`.\n")
	return nil
}

// resetAllowed checks what is at secret.key: reset goes on only when nothing
// is there, or when a valid key there opens none of the sealed values.
func (k *secretKeyCmd) resetAllowed(st *store.Store, dataDir string) (keyMissing bool, err error) {
	keyPath := filepath.Join(dataDir, core.SecretKeyFile)
	if _, err := os.Lstat(keyPath); errors.Is(err, fs.ErrNotExist) {
		return true, nil
	} else if err != nil {
		return false, err
	}
	key, err := core.ReadSecretKey(dataDir)
	if errors.Is(err, core.ErrInvalidSecretKey) || errors.Is(err, fs.ErrNotExist) {
		return false, actionf("%v: move %s away first, because the next start would stop on it", err, keyPath)
	}
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

// dataDir resolves the data directory of the selected configuration.
func (c *configCmd) dataDir() (string, error) {
	configPath, _, err := c.paths()
	if err != nil {
		return "", err
	}
	_, cfg, err := load(configPath)
	if err != nil {
		return "", err
	}
	return cfg.Host.DataDir, nil
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
	hidden(prompt string) (string, error)
	line(prompt string) (string, error)
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

// hidden reads without echo. main catches SIGINT, so Ctrl-C does not end the
// read itself: wait for it or for cancellation, and restore echo on the way out.
func (t *terminal) hidden(prompt string) (string, error) {
	fd := int(t.f.Fd())
	saved, err := term.GetState(fd)
	if err != nil {
		return "", err
	}
	fmt.Fprint(t.out, prompt)
	return t.wait(func() (string, error) {
		b, err := term.ReadPassword(fd)
		return string(b), err
	}, func() { _ = term.Restore(fd, saved) })
}

func (t *terminal) line(prompt string) (string, error) {
	fmt.Fprint(t.out, prompt)
	return t.wait(func() (string, error) {
		s, err := bufio.NewReader(t.f).ReadString('\n')
		if errors.Is(err, io.EOF) {
			err = nil
		}
		return s, err
	}, func() {})
}

func (t *terminal) wait(read func() (string, error), restore func()) (string, error) {
	type reply struct {
		s   string
		err error
	}
	ch := make(chan reply, 1)
	go func() {
		s, err := read()
		ch <- reply{s, err}
	}()
	select {
	case r := <-ch:
		fmt.Fprintln(t.out)
		return r.s, r.err
	case <-t.ctx.Done():
		restore()
		fmt.Fprintln(t.out)
		return "", fmt.Errorf("interrupted; nothing was written: %w", t.ctx.Err())
	}
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

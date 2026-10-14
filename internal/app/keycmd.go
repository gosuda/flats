package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/store"
)

// KeyUsage lists the `flats key` subcommands.
const KeyUsage = `key ls [--config path | --data dir]
       flats key set tailscale|zrok|netbird [--from-file path] [--config path | --data dir]   (value from stdin)
       flats key rm tailscale|zrok|netbird [--config path | --data dir]`

// KeyCommand runs `flats key <subcommand>`: it manages the operator keys in
// flats.db while the host is stopped, like `flats config set`. A key is read
// from stdin (without echo on a terminal) or --from-file, never from the
// command line. zrok and NetBird keys wait for the next start, which connects
// them when no account or peer is connected yet.
func KeyCommand(ctx context.Context, args []string, in io.Reader, out io.Writer) error {
	if len(args) == 0 {
		return usagef("missing subcommand")
	}
	c := &configCmd{ctx: ctx, out: out}
	name, rest := args[0], args[1:]
	switch name {
	case "ls", "set", "rm":
	default:
		return usagef("unknown subcommand %q", name)
	}
	fs := c.flags("key " + name)
	fromFile := ""
	if name == "set" {
		fs.StringVar(&fromFile, "from-file", "", "read the key from this file instead of stdin")
	}
	want := 1
	if name == "ls" {
		want = 0
	}
	pos, extra, err := c.parse(rest, want)
	if err != nil {
		return err
	}
	if len(extra) > 0 {
		return usagef("unexpected argument %q", extra[0])
	}
	id := ""
	if want == 1 {
		if id = pos[0]; !slices.Contains(core.HostKeyIDs, id) {
			return usagef("unknown key %q: use tailscale, zrok or netbird", id)
		}
	}

	configPath, _, err := c.paths()
	if err != nil {
		return err
	}
	_, cfg, err := load(configPath)
	if err != nil {
		return err
	}
	dataDir := cfg.Host.DataDir
	lock, err := lockOffline(dataDir)
	if err != nil {
		return err
	}
	defer lock.Close()
	dbPath := filepath.Join(dataDir, "flats.db")
	info, err := inspectDB(dbPath)
	if err != nil {
		return err
	}
	if info.Binding == nil || info.Binding.InstanceID != cfg.Host.InstanceID {
		return actionf("%s is not the database of %s; run `flats config init` or start the host once first", dbPath, configPath)
	}
	st, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	switch name {
	case "ls":
		return listKeys(ctx, st, out)
	case "rm":
		return removeKey(ctx, st, id, out)
	}
	value, err := readKeyValue(id, fromFile, in, out)
	if err != nil {
		return err
	}
	if err := core.CheckHostKeyValue(id, value); err != nil {
		return &ActionError{err}
	}
	if err := core.CheckSecretKey(ctx, dataDir, st); err != nil {
		return &ActionError{err}
	}
	if err := core.PutHostKeyValue(ctx, dataDir, st, id, value, core.KeySourceCLI, time.Now()); err != nil {
		return err
	}
	if id == core.HostKeyTailscale {
		c.printf("stored the Tailscale auth key in %s; new tailnet nodes use it from the next start\n", dbPath)
	} else {
		c.printf("stored the %s key in %s; the next start connects %s with it if none is connected yet\n", id, dbPath, id)
	}
	return nil
}

// readKeyValue reads a key from --from-file, or from in: on a terminal it
// prompts without echo.
func readKeyValue(id, fromFile string, in io.Reader, out io.Writer) (string, error) {
	if fromFile != "" {
		return readKeyFile(fromFile, "--from-file")
	}
	if f, ok := in.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		fmt.Fprintf(out, "%s key: ", id)
		b, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(out)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(b)), nil
	}
	if in == nil {
		return "", usagef("no key on stdin: pipe the key in, or use --from-file")
	}
	b, err := io.ReadAll(io.LimitReader(in, 64<<10))
	if err != nil {
		return "", err
	}
	v := strings.TrimSpace(string(b))
	if v == "" {
		return "", usagef("no key on stdin: pipe the key in, or use --from-file")
	}
	return v, nil
}

func listKeys(ctx context.Context, st *store.Store, out io.Writer) error {
	rows, err := st.ListHostKeys(ctx)
	if err != nil {
		return err
	}
	byID := map[string]store.HostKey{}
	for _, r := range rows {
		byID[r.ID] = r
	}
	for _, id := range core.HostKeyIDs {
		r, ok := byID[id]
		state := "none"
		switch {
		case !ok:
		case r.HasValue() && id == core.HostKeyTailscale:
			state = "stored"
		case r.HasValue():
			state = "waiting for the next start"
		default:
			state = "connected"
		}
		line := fmt.Sprintf("%-10s %s", id, state)
		if ok {
			line += fmt.Sprintf(" (from %s, %s)", r.Source, r.UpdatedAt.Local().Format(time.DateTime))
			if r.LastError != "" {
				line += "; last error: " + r.LastError
			}
		}
		fmt.Fprintln(out, line)
	}
	return nil
}

func removeKey(ctx context.Context, st *store.Store, id string, out io.Writer) error {
	r, err := st.GetHostKey(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		fmt.Fprintf(out, "no %s key is stored\n", id)
		return nil
	}
	if err != nil {
		return err
	}
	if !r.HasValue() {
		return actionf("%s is connected and keeps no key in flats.db; disconnect it in the console (Settings > Keys)", id)
	}
	if err := st.DeleteHostKey(ctx, id); err != nil {
		return err
	}
	if id == core.HostKeyTailscale {
		fmt.Fprintln(out, "removed the Tailscale auth key; new tailnet nodes wait for an interactive login")
	} else {
		fmt.Fprintf(out, "removed the waiting %s key\n", id)
	}
	return nil
}

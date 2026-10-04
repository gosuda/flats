package app

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/gosuda/flats/internal/config"
	"github.com/gosuda/flats/internal/store"
)

// UsageError is a command-line mistake; commands exit with status 2.
type UsageError struct{ Msg string }

func (e *UsageError) Error() string { return e.Msg }

// ExitCode implements the exit status hook of package cli.
func (e *UsageError) ExitCode() int { return 2 }

func usagef(format string, args ...any) error { return &UsageError{fmt.Sprintf(format, args...)} }

// ConfigUsage lists the `flats config` subcommands.
const ConfigUsage = `config init [--data dir] [--config path] [--listen addr] [--local-addr addr] [--console-host name] [--runtime=bool]
       flats config show [--config path | --data dir]
       flats config validate [--config path | --data dir]
       flats config set KEY VALUE [--config path | --data dir]
       flats config unset KEY [--config path | --data dir]
       flats config migrate [--data dir] [--config path] [--dry-run] [--yes] [-- legacy serve flags]
       flats config rebind --yes [--config path | --data dir]`

// ConfigCommand runs `flats config <subcommand>`. Offline subcommands take
// the data lock and refuse while a host runs; show and validate only read.
func ConfigCommand(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 {
		return usagef("missing subcommand")
	}
	c := &configCmd{ctx: ctx, out: out}
	switch name, rest := args[0], args[1:]; name {
	case "init":
		return c.init(rest)
	case "show":
		return c.show(rest)
	case "validate":
		return c.validate(rest)
	case "set":
		return c.set(rest, false)
	case "unset":
		return c.set(rest, true)
	case "migrate":
		return c.migrate(rest)
	case "rebind":
		return c.rebind(rest)
	default:
		return usagef("unknown subcommand %q", name)
	}
}

type configCmd struct {
	ctx        context.Context
	out        io.Writer
	fs         *flag.FlagSet
	configFlag string
	dataFlag   string
}

func (c *configCmd) flags(name string) *flag.FlagSet {
	c.fs = flag.NewFlagSet("config "+name, flag.ContinueOnError)
	c.fs.SetOutput(io.Discard)
	c.fs.StringVar(&c.configFlag, "config", "", "config.json path (default: FLATS_CONFIG, else <data>/config.json)")
	c.fs.StringVar(&c.dataFlag, "data", "", "data directory (default: FLATS_DATA or the user config directory)")
	return c.fs
}

// parse parses flags around positional arguments. Arguments after "--" are
// returned as rest; positional is the number of other arguments.
func (c *configCmd) parse(args []string, positional int) ([]string, []string, error) {
	var pos, rest []string
	for {
		if err := c.fs.Parse(args); err != nil {
			return nil, nil, usagef("%v", err)
		}
		left := c.fs.Args()
		if n := len(args) - len(left); n > 0 && args[n-1] == "--" {
			rest = left
			break
		}
		if len(left) == 0 {
			break
		}
		pos, args = append(pos, left[0]), left[1:]
	}
	if positional >= 0 && len(pos) != positional {
		return nil, nil, usagef("want %d arguments, got %d", positional, len(pos))
	}
	return pos, rest, nil
}

// paths resolves the config path and the data directory: --config, else
// <--data>/config.json, else FLATS_CONFIG, else <FLATS_DATA or default>/config.json.
func (c *configCmd) paths() (configPath, dataDir string, err error) {
	dataDir = DefaultDataDir()
	if c.dataFlag != "" {
		if dataDir, err = filepath.Abs(c.dataFlag); err != nil {
			return "", "", err
		}
	}
	switch env := os.Getenv("FLATS_CONFIG"); {
	case c.configFlag != "":
		configPath, err = filepath.Abs(c.configFlag)
	case c.dataFlag == "" && env != "":
		if !filepath.IsAbs(env) {
			return "", "", actionf("FLATS_CONFIG must be an absolute path, not %q", env)
		}
		configPath = env
	default:
		configPath = filepath.Join(dataDir, "config.json")
	}
	return configPath, dataDir, err
}

// parseFlags parses a subcommand that takes only flags.
func (c *configCmd) parseFlags(args []string) error {
	_, rest, err := c.parse(args, 0)
	if err == nil && len(rest) > 0 {
		err = usagef("unexpected argument %q", rest[0])
	}
	return err
}

func (c *configCmd) printf(format string, args ...any) { fmt.Fprintf(c.out, format, args...) }

// load reads an existing config for an offline command.
func load(path string) (*config.Loaded, *config.Config, error) {
	l, err := config.Load(path)
	if err != nil {
		return nil, nil, &ActionError{fmt.Errorf("config %s: %w", path, err)}
	}
	cfg, err := l.Doc.Effective(nil)
	if err != nil {
		return nil, nil, &ActionError{err}
	}
	return l, cfg, nil
}

// lockOffline takes the data lock for an offline change.
func lockOffline(dir string) (*os.File, error) {
	f, err := lockDataDir(dir)
	if errors.Is(err, errDataDirInUse) {
		return nil, actionf("%v: stop the running Flats host first", err)
	}
	return f, err
}

func (c *configCmd) init(args []string) error {
	f := c.flags("init")
	host := map[string]*string{}
	for _, hf := range hostFlags {
		host[hf.flag] = new(string)
	}
	f.StringVar(host["listen"], "listen", "", "loopback management address")
	f.StringVar(host["local-addr"], "local-addr", "", "address of the local network")
	f.StringVar(host["console-host"], "console-host", "", "tailnet host name of the console")
	serverRuntime := f.Bool("runtime", true, "enable server flats")
	if err := c.parseFlags(args); err != nil {
		return err
	}
	values := map[string]string{}
	f.Visit(func(fl *flag.Flag) {
		for _, hf := range hostFlags {
			if hf.flag == fl.Name {
				if fl.Name == "runtime" {
					values[hf.key] = strconv.FormatBool(*serverRuntime)
				} else {
					values[hf.key] = *host[fl.Name]
				}
			}
		}
	})
	configPath, dataDir, err := c.paths()
	if err != nil {
		return err
	}
	msg, err := initHost(c.ctx, configPath, dataDir, values)
	if err != nil {
		return err
	}
	c.printf("%s\n", msg)
	return nil
}

// initHost creates config.json, the database and its binding for a new
// host. A config left by an interrupted init is reused with its instance
// id; a data directory whose database already holds data is refused.
func initHost(ctx context.Context, configPath, dataDir string, host map[string]string) (string, error) {
	l, err := config.Load(configPath)
	exists := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", &ActionError{fmt.Errorf("config %s: %w", configPath, err)}
	}
	var doc *config.Document
	if exists {
		if l.NeedsConfirm {
			return "", actionf("config %s needs `flats config migrate --config %s --yes` first", configPath, configPath)
		}
		doc = l.Doc
		cfg, err := doc.Effective(nil)
		if err != nil {
			return "", &ActionError{err}
		}
		dataDir = cfg.Host.DataDir
		for key, v := range host {
			if have, _, _ := cfg.Lookup(key); fmt.Sprint(have) != v {
				return "", actionf("%s already exists with %s %s; change it with `flats config set %s %s`", configPath, key, show(have), key, v)
			}
		}
	} else {
		if doc, err = config.New(config.NewInstanceID(), dataDir); err != nil {
			return "", &ActionError{err}
		}
		for key, v := range host {
			if err := doc.Set(key, v); err != nil {
				return "", &ActionError{err}
			}
		}
	}
	cfg, err := doc.Effective(nil)
	if err != nil {
		return "", &ActionError{err}
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return "", err
	}
	lock, err := lockOffline(dataDir)
	if err != nil {
		return "", err
	}
	defer lock.Close()
	var hash string
	if exists {
		hash = l.Hash
	}
	if err := sameConfig(configPath, exists, hash); err != nil {
		return "", err
	}
	dbPath := filepath.Join(dataDir, "flats.db")
	info, err := inspectDB(dbPath)
	if err != nil {
		return "", err
	}
	switch {
	case info.Binding != nil && exists && info.Binding.InstanceID == cfg.Host.InstanceID:
		return fmt.Sprintf("%s is already initialized (instance %s)", configPath, cfg.Host.InstanceID), nil
	case info.Binding != nil:
		return "", actionf("%s is already bound to the config %s (instance %s)", dbPath, info.Binding.ConfigPath, info.Binding.InstanceID)
	case info.HasData:
		return "", actionf("%s already holds a Flats host; convert it with `flats config migrate --data %s`", dbPath, dataDir)
	}
	if exists && l.Migrated {
		if _, err := config.NewWriter(configPath).Save(l.Hash, nil); err != nil {
			return "", err
		}
	}
	if !exists {
		if _, err := config.Create(configPath, doc); err != nil {
			return "", err
		}
	}
	if err := stop("config"); err != nil {
		return "", err
	}
	st, err := store.Open(dbPath)
	if err != nil {
		return "", err
	}
	defer st.Close()
	if err := st.BindHost(ctx, store.HostBinding{InstanceID: cfg.Host.InstanceID, ConfigPath: configPath, BoundAt: time.Now()}); err != nil {
		return "", err
	}
	return fmt.Sprintf("initialized %s (instance %s, data %s)", configPath, cfg.Host.InstanceID, dataDir), nil
}

func (c *configCmd) show(args []string) error {
	c.flags("show")
	if err := c.parseFlags(args); err != nil {
		return err
	}
	configPath, _, err := c.paths()
	if err != nil {
		return err
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		return &ActionError{fmt.Errorf("config %s: %w", configPath, err)}
	}
	c.printf("# %s (sha256 %s)\n%s", configPath, config.Hash(data), data)
	l, err := config.Parse(data)
	if err != nil {
		return &ActionError{err}
	}
	cfg, err := l.Doc.Effective(nil)
	if err != nil {
		return &ActionError{err}
	}
	c.printf("\n# effective values (source)\n")
	for _, key := range config.Keys() {
		v, src, _ := cfg.Lookup(key)
		raw := "null"
		if v != nil {
			b, _ := json.Marshal(v)
			raw = string(b)
		}
		c.printf("%-34s %s (%s)\n", key, raw, src)
	}
	return nil
}

func (c *configCmd) validate(args []string) error {
	c.flags("validate")
	if err := c.parseFlags(args); err != nil {
		return err
	}
	configPath, _, err := c.paths()
	if err != nil {
		return err
	}
	l, _, err := load(configPath)
	if err != nil {
		return err
	}
	if l.Migrated {
		c.printf("%s is valid (schema_version %d; Flats converts it to %d on the next start)\n", configPath, l.FileVersion, config.CurrentSchemaVersion)
		return nil
	}
	c.printf("%s is valid\n", configPath)
	return nil
}

// set changes one key while the host is stopped. host.data_dir must name a
// data directory whose database is bound to this instance.
func (c *configCmd) set(args []string, unset bool) error {
	name, n := "set", 2
	if unset {
		name, n = "unset", 1
	}
	c.flags(name)
	pos, rest, err := c.parse(args, -1)
	if err != nil {
		return err
	}
	// "--" lets a value start with a dash.
	if pos = append(pos, rest...); len(pos) != n {
		return usagef("want %d arguments, got %d", n, len(pos))
	}
	key := pos[0]
	if key == "host.instance_id" {
		return actionf("host.instance_id cannot be changed; give a copied data directory its own identity with `flats config rebind --yes`")
	}
	configPath, _, err := c.paths()
	if err != nil {
		return err
	}
	l, cfg, err := load(configPath)
	if err != nil {
		return err
	}
	// After the data was moved away, no host can run in the old directory.
	moving := key == "host.data_dir" && !unset
	if _, err := os.Stat(cfg.Host.DataDir); !moving || err == nil {
		lock, err := lockOffline(cfg.Host.DataDir)
		if err != nil {
			return err
		}
		defer lock.Close()
	}
	if moving {
		dir := pos[1]
		if !filepath.IsAbs(dir) {
			return actionf("host.data_dir must be an absolute path")
		}
		if dir != cfg.Host.DataDir {
			if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
				return actionf("%s is not a directory; move the data there first", dir)
			}
			other, err := lockOffline(dir)
			if err != nil {
				return err
			}
			defer other.Close()
			info, err := inspectDB(filepath.Join(dir, "flats.db"))
			if err != nil {
				return err
			}
			if info.Binding == nil || info.Binding.InstanceID != cfg.Host.InstanceID {
				return actionf("%s does not hold the database of instance %s; move this host's data there first", dir, cfg.Host.InstanceID)
			}
		}
	}
	_, err = config.NewWriter(configPath).Save(l.Hash, func(d *config.Document) error {
		if unset {
			return d.Unset(key)
		}
		return d.Set(key, pos[1])
	})
	if errors.Is(err, config.ErrConflict) {
		return actionf("%s changed while it was being updated; retry", configPath)
	}
	var fe *config.FieldError
	if errors.As(err, &fe) {
		return &ActionError{err}
	}
	if err != nil {
		return err
	}
	if unset {
		c.printf("unset %s in %s; it uses its default from the next start\n", key, configPath)
	} else {
		c.printf("set %s in %s; it applies from the next start\n", key, configPath)
	}
	return nil
}

// migrate converts a legacy data directory with the code `flats serve`
// runs in legacy mode, or reports what it would do with --dry-run.
func (c *configCmd) migrate(args []string) error {
	f := c.flags("migrate")
	dryRun := f.Bool("dry-run", false, "show the result without writing anything")
	yes := f.Bool("yes", false, "confirm a config format conversion that changes network or credentials")
	_, serveArgs, err := c.parse(args, 0)
	if err != nil {
		return err
	}
	o, err := ParseServeFlags(serveArgs)
	if err != nil {
		return usagef("legacy serve flags: %v", err)
	}
	if o.Set["config"] {
		return usagef("pass --config before --")
	}
	if o.Set["data"] {
		if c.dataFlag != "" && o.DataDir != mustAbs(c.dataFlag) {
			return usagef("--data %s and the serve flag --data %s differ", c.dataFlag, o.DataDir)
		}
		c.dataFlag = o.DataDir
	}
	configPath, dataDir, err := c.paths()
	if err != nil {
		return err
	}
	l, err := config.Load(configPath)
	if err == nil && l.NeedsConfirm {
		if *dryRun || !*yes {
			c.printf("%s would change from schema_version %d to %d:\n%s\n", configPath, l.FileVersion, config.CurrentSchemaVersion, l.Doc.Encode())
			if *dryRun {
				return nil
			}
			return actionf("the conversion can change network or credentials; rerun with --yes to apply it")
		}
		cfg, err := l.Doc.Effective(nil)
		if err != nil {
			return &ActionError{err}
		}
		lock, err := lockOffline(cfg.Host.DataDir)
		if err != nil {
			return err
		}
		_, err = config.NewWriter(configPath).Save(l.Hash, nil)
		lock.Close()
		if err != nil {
			return err
		}
		c.printf("converted %s to schema_version %d\n", configPath, config.CurrentSchemaVersion)
	} else if errors.Is(err, fs.ErrNotExist) {
		if _, serr := os.Stat(filepath.Join(dataDir, "flats.db")); serr != nil {
			return actionf("nothing to migrate: %s has no flats.db; create a new host with `flats config init`", dataDir)
		}
	}
	s := setup{configPath: configPath, dataDir: dataDir, legacy: true, flags: o, dryRun: *dryRun,
		logf: func(format string, args ...any) { c.printf(format+"\n", args...) }}
	p, err := prepare(c.ctx, s)
	if err != nil {
		if errors.Is(err, errDataDirInUse) {
			return actionf("%v: stop the running Flats host first, or restart it once without --config so it migrates itself", err)
		}
		return err
	}
	defer p.close()
	for _, n := range p.notes {
		c.printf("%s\n", n)
	}
	if *dryRun {
		if p.doc != nil {
			c.printf("%s:\n%s", configPath, p.doc.Encode())
		}
		c.printf("dry run: nothing was written\n")
		return nil
	}
	c.printf("%s is ready (instance %s)\n", configPath, p.cfg.Host.InstanceID)
	return nil
}

// rebind gives a copied data directory its own identity: a new instance id
// in config.json and in the database binding. Rerun it if it is interrupted.
func (c *configCmd) rebind(args []string) error {
	f := c.flags("rebind")
	yes := f.Bool("yes", false, "confirm the new identity")
	if err := c.parseFlags(args); err != nil {
		return err
	}
	if !*yes {
		return usagef("rebind gives this config and its data a new host identity; confirm with --yes")
	}
	configPath, _, err := c.paths()
	if err != nil {
		return err
	}
	l, cfg, err := load(configPath)
	if err != nil {
		return err
	}
	lock, err := lockOffline(cfg.Host.DataDir)
	if err != nil {
		return err
	}
	defer lock.Close()
	dbPath := filepath.Join(cfg.Host.DataDir, "flats.db")
	info, err := inspectDB(dbPath)
	if err != nil {
		return err
	}
	if info.Binding == nil {
		return actionf("%s is not bound to any config; use `flats config init` or `flats config migrate` instead", dbPath)
	}
	id := config.NewInstanceID()
	if _, err := config.NewWriter(configPath).Save(l.Hash, func(d *config.Document) error {
		return d.Set("host.instance_id", id)
	}); err != nil {
		return err
	}
	st, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.ReplaceHostBinding(c.ctx, store.HostBinding{InstanceID: id, ConfigPath: configPath, BoundAt: time.Now()}); err != nil {
		return err
	}
	c.printf("rebound %s and %s to the new instance %s\n", configPath, dbPath, id)
	return nil
}

func mustAbs(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return path
}

// EnsureConfig prepares the config that `flats install` runs the service
// with: it creates one from serveArgs for a new host, migrates a legacy
// data directory, or checks that serveArgs match an existing config. The
// service then runs `flats serve --config <configPath>`. Empty configPath
// and dataDir use the defaults of `flats config`.
func EnsureConfig(ctx context.Context, configPath, dataDir string, serveArgs []string, out io.Writer) (string, string, error) {
	o, err := ParseServeFlags(serveArgs)
	if err != nil {
		return "", "", usagef("serve flags: %v", err)
	}
	if o.Set["config"] {
		return "", "", usagef("pass --config to install, not as a serve flag")
	}
	c := &configCmd{configFlag: configPath, dataFlag: dataDir}
	if o.Set["data"] {
		if dataDir != "" && o.DataDir != mustAbs(dataDir) {
			return "", "", usagef("--data %s and the serve flag --data %s differ", dataDir, o.DataDir)
		}
		c.dataFlag = o.DataDir
	}
	if configPath, dataDir, err = c.paths(); err != nil {
		return "", "", err
	}
	// A configured, bound host whose flags match needs no lock: the service
	// may be running and keeps running until install restarts it.
	if l, err := config.Load(configPath); err == nil && !l.Migrated {
		if cfg, err := l.Doc.Effective(nil); err == nil {
			info, err := store.Inspect(filepath.Join(cfg.Host.DataDir, "flats.db"))
			if err == nil && info.Binding != nil && info.Binding.InstanceID == cfg.Host.InstanceID && len(flagConflicts(cfg, o)) == 0 {
				return configPath, cfg.Host.DataDir, nil
			}
		}
	}
	s := setup{configPath: configPath, dataDir: dataDir, legacy: true, flags: o,
		logf: func(format string, args ...any) { fmt.Fprintf(out, format+"\n", args...) }}
	p, err := prepare(ctx, s)
	if errors.Is(err, errDataDirInUse) {
		return "", "", actionf("%v: stop the running Flats host, then run install again", err)
	}
	if err != nil {
		return "", "", err
	}
	defer p.close()
	for _, n := range p.notes {
		fmt.Fprintln(out, n)
	}
	return configPath, p.cfg.Host.DataDir, nil
}

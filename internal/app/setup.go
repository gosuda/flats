package app

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gosuda/flats/internal/config"
	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/expose/portal"
	"github.com/gosuda/flats/internal/expose/provider"
	"github.com/gosuda/flats/internal/store"
)

// ExitConfig is the exit status of a failure the operator must fix
// (sysexits EX_CONFIG). Installed systemd units do not restart on it.
const ExitConfig = 78

// ActionError is a failure that a restart cannot fix: the configuration,
// the data directory or the flags need the operator. Commands exit with
// ExitConfig.
type ActionError struct{ Err error }

func (e *ActionError) Error() string { return e.Err.Error() }
func (e *ActionError) Unwrap() error { return e.Err }

// ExitCode implements the exit status hook of package cli.
func (e *ActionError) ExitCode() int { return ExitConfig }

func actionf(format string, args ...any) error {
	return &ActionError{fmt.Errorf(format, args...)}
}

// errDataDirInUse reports that another process holds the data lock.
var errDataDirInUse = errors.New("another Flats process holds the data directory lock")

// setup selects config.json and the data directory for one host start or
// one offline command.
type setup struct {
	configPath string
	// dataDir is where a new config.json points (legacy mode). An existing
	// config's host.data_dir always wins.
	dataDir string
	// legacy allows creating config.json from the flags, by fresh init or by
	// migrating a legacy data directory, and compares explicitly set flags
	// with an existing config.
	legacy bool
	flags  Options
	// overrides are per-run values of the host keys; never stored.
	overrides map[string]string
	// dryRun computes a migration without the data lock or any write.
	dryRun bool
	logf   func(string, ...any)
}

// prepared is a host whose config.json and database agree. The caller owns
// lock and st (both nil after a dry run).
type prepared struct {
	path    string
	dataDir string
	doc     *config.Document
	hash    string
	cfg     *config.Config // effective, with overrides
	lock    *os.File
	st      *store.Store
	notes   []string // what migration or init did
	// migrated is set when this run converted a legacy host.
	migrated bool
	// flagsChecked is set when explicitly set legacy flags matched an
	// existing config.
	flagsChecked bool
}

// interrupt lets tests stop a migration or init after a step: "db" (database
// migrated and the legacy choice applied), "config" (config.json written),
// "bind" (database bound). Production leaves it nil.
var interrupt func(step string) error

func stop(step string) error {
	if interrupt != nil {
		return interrupt(step)
	}
	return nil
}

// prepare resolves config.json and the database in the order of the storage
// design: read and validate the config in memory, take the data lock, check
// that the file did not change, inspect the database read-only, decide, and
// only then write. Every refusal leaves the files as they were.
func prepare(ctx context.Context, s setup) (_ *prepared, err error) {
	p := &prepared{path: s.configPath}
	l, err := config.Load(s.configPath)
	exists := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, &ActionError{fmt.Errorf("config %s: %w", s.configPath, err)}
	}
	dataDir := s.dataDir
	if !exists && !s.legacy {
		// Config mode bootstraps a missing config.json: the new host keeps its
		// data beside the file, as `flats config init` lays it out by default.
		dataDir = filepath.Dir(s.configPath)
	}
	if exists {
		if l.NeedsConfirm {
			return nil, actionf("config %s uses schema_version %d and its conversion needs confirmation; run `flats config migrate --config %s --yes`", s.configPath, l.FileVersion, s.configPath)
		}
		c, err := l.Doc.Effective(nil)
		if err != nil {
			return nil, &ActionError{err}
		}
		dataDir = c.Host.DataDir
		if fi, err := os.Stat(dataDir); err != nil || !fi.IsDir() {
			return nil, actionf("host.data_dir %s in %s is not a directory; fix the path, or move the data back", dataDir, s.configPath)
		}
		p.doc, p.hash = l.Doc, l.Hash
	}
	p.dataDir = dataDir
	dbPath := filepath.Join(dataDir, "flats.db")

	if s.dryRun {
		info, err := inspectDB(dbPath)
		if err != nil {
			return nil, err
		}
		return p, p.decide(ctx, s, l, info, dbPath)
	}
	if !exists {
		if err := os.MkdirAll(dataDir, 0o700); err != nil {
			return nil, err
		}
	}
	if p.lock, err = lockDataDir(dataDir); err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			p.close()
		}
	}()
	if err := sameConfig(s.configPath, exists, p.hash); err != nil {
		return nil, err
	}
	info, err := inspectDB(dbPath)
	if err != nil {
		return nil, err
	}
	if err := p.decide(ctx, s, l, info, dbPath); err != nil {
		return nil, err
	}
	if err := stop("bind"); err != nil {
		return nil, err
	}
	// Housekeeping: the host file is not read once the database is bound.
	if err := archiveProviderFile(dataDir, time.Now()); err != nil {
		s.logf("move %s to %s: %v; retrying on the next start", provider.FileName, filepath.Join(dataDir, "backups"), err)
	}
	if p.cfg, err = p.doc.Effective(s.overrides); err != nil {
		return nil, &ActionError{err}
	}
	return p, nil
}

func (p *prepared) close() {
	if p.st != nil {
		p.st.Close()
		p.st = nil
	}
	if p.lock != nil {
		p.lock.Close()
		p.lock = nil
	}
}

// decide applies the state table of the storage design. In a dry run it
// only fills p.doc and p.notes.
func (p *prepared) decide(ctx context.Context, s setup, l *config.Loaded, info store.Info, dbPath string) error {
	exists := l != nil
	switch {
	case exists && info.Binding != nil:
		c, _ := p.doc.Effective(nil)
		if info.Binding.InstanceID != c.Host.InstanceID {
			return actionf("%s is bound to host instance %s, but %s is instance %s: the config points at another host's data or at a copy; fix host.data_dir, or give a copy its own identity with `flats config rebind --config %s --yes`",
				dbPath, info.Binding.InstanceID, s.configPath, c.Host.InstanceID, s.configPath)
		}
		if s.legacy {
			if err := p.checkFlags(c, s); err != nil {
				return err
			}
		}
		if s.dryRun {
			p.notes = append(p.notes, fmt.Sprintf("%s is already bound to %s; nothing to migrate", dbPath, s.configPath))
			return nil
		}
		return p.open(l, dbPath)

	case exists && !s.legacy:
		if info.HasData {
			return actionf("%s is not bound to a config; convert the legacy data directory with `flats config migrate --data %s`", dbPath, p.dataDir)
		}
		c, _ := p.doc.Effective(nil)
		// Only a config beside its own data directory finishes an init here:
		// that is an interrupted bootstrap, or a config written by hand. A
		// config whose host.data_dir is elsewhere and empty may point at the
		// wrong directory or at an unmounted volume.
		if filepath.Clean(c.Host.DataDir) != filepath.Dir(s.configPath) {
			return actionf("%s has no host database yet, and host.data_dir is not the directory of %s: check that host.data_dir is the right directory and is mounted, then run `flats config init --config %s`", p.dataDir, s.configPath, s.configPath)
		}
		if found := flatsLeftovers(p.dataDir); len(found) > 0 {
			return leftoverErr(s.configPath, p.dataDir, found)
		}
		if s.dryRun {
			p.notes = append(p.notes, "would create "+dbPath+" for "+s.configPath)
			return nil
		}
		if err := p.open(l, dbPath); err != nil {
			return err
		}
		if err := p.bind(ctx, c.Host.InstanceID, s.configPath, false); err != nil {
			return err
		}
		p.notes = append(p.notes, fmt.Sprintf("created the host database in %s for %s (instance %s)", p.dataDir, s.configPath, c.Host.InstanceID))
		return nil

	case exists && info.HasData:
		// Interrupted between writing config.json and binding the database.
		c, _ := p.doc.Effective(nil)
		plan, err := planLegacy(p.dataDir, info, s.flags, c.Host.InstanceID)
		if err != nil {
			return err
		}
		if diff := docDiff(p.doc, plan.doc); diff != "" {
			return actionf("%s exists but %s is not bound to it, and the config differs from the migration of this data directory with these flags:\n%s\nrestore the original flags, or move the config away to migrate again", s.configPath, dbPath, diff)
		}
		p.notes = append(p.notes, plan.notes...)
		if s.dryRun {
			p.notes = append(p.notes, "would resume the interrupted migration and bind the database to "+s.configPath)
			return nil
		}
		return p.migrate(ctx, l, plan, dbPath, s.configPath, false)

	case exists:
		// Interrupted init: config.json without a bound database.
		c, _ := p.doc.Effective(nil)
		if err := p.checkFlags(c, s); err != nil {
			return err
		}
		if s.dryRun {
			p.notes = append(p.notes, "would create "+dbPath+" for "+s.configPath)
			return nil
		}
		if err := p.open(l, dbPath); err != nil {
			return err
		}
		return p.bind(ctx, c.Host.InstanceID, s.configPath, false)

	case info.Binding != nil:
		return actionf("%s is bound to the config %s (instance %s), but %s does not exist; run with --config %s",
			dbPath, info.Binding.ConfigPath, info.Binding.InstanceID, s.configPath, info.Binding.ConfigPath)

	case !s.legacy:
		return p.bootstrap(ctx, s, info, dbPath)

	default:
		// A legacy data directory, or a new one: config.json from the flags.
		plan, err := planLegacy(p.dataDir, info, s.flags, config.NewInstanceID())
		if err != nil {
			return err
		}
		p.doc, p.notes, p.migrated = plan.doc, plan.notes, plan.migrated
		if s.dryRun {
			return nil
		}
		return p.migrate(ctx, nil, plan, dbPath, s.configPath, true)
	}
}

// bootstrap starts a new host in config mode when config.json does not
// exist: it writes the default config (per-run overrides are not stored),
// creates the database and binds it, as `flats config init` does. A data
// directory that already holds Flats data is refused, never replaced.
func (p *prepared) bootstrap(ctx context.Context, s setup, info store.Info, dbPath string) error {
	if info.HasData {
		return actionf("%s does not exist, but %s holds a Flats host that is not bound to a config; convert it with `flats config migrate --data %s --config %s`", s.configPath, dbPath, p.dataDir, s.configPath)
	}
	if found := flatsLeftovers(p.dataDir); len(found) > 0 {
		return leftoverErr(s.configPath, p.dataDir, found)
	}
	doc, err := config.New(config.NewInstanceID(), p.dataDir)
	if err != nil {
		return &ActionError{err}
	}
	c, err := doc.Effective(nil)
	if err != nil {
		return &ActionError{err}
	}
	p.doc = doc
	if s.dryRun {
		p.notes = append(p.notes, "would initialize a new host in "+p.dataDir)
		return nil
	}
	if p.hash, err = config.Create(s.configPath, doc); err != nil {
		return err
	}
	if err := stop("config"); err != nil {
		return err
	}
	if err := p.open(nil, dbPath); err != nil {
		return err
	}
	if err := p.bind(ctx, c.Host.InstanceID, s.configPath, false); err != nil {
		return err
	}
	p.notes = append(p.notes, fmt.Sprintf("initialized a new host in %s (instance %s): %s did not exist, so Flats created it with the defaults and an empty database. If you expected existing data, stop the host and check the path or volume", p.dataDir, c.Host.InstanceID, s.configPath))
	return nil
}

// leftoverNames are entries only a Flats host creates in its data directory.
var leftoverNames = []string{"flats", "secret.key", "tsnet", "portal", "backups", "network-retirements", provider.FileName}

// flatsLeftovers lists the Flats entries in dir. Other entries, such as a
// new volume's lost+found, do not stop a bootstrap.
func flatsLeftovers(dir string) []string {
	var found []string
	for _, name := range leftoverNames {
		if _, err := os.Lstat(filepath.Join(dir, name)); err == nil {
			found = append(found, name)
		}
	}
	return found
}

func leftoverErr(configPath, dataDir string, found []string) error {
	return actionf("%s has no host database, but %s already holds Flats data (%s): restore flats.db and %s from the same backup, or start a new host in an empty directory",
		dataDir, dataDir, strings.Join(found, ", "), configPath)
}

// open persists a config schema migration, then opens (and migrates) the
// database.
func (p *prepared) open(l *config.Loaded, dbPath string) error {
	if l != nil && l.Migrated {
		h, err := config.NewWriter(p.path).Save(l.Hash, nil)
		if err != nil {
			return err
		}
		p.hash = h
	}
	st, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	p.st = st
	return nil
}

// migrate runs the steps of the legacy migration: back up and migrate the
// database and apply the legacy Private Tailscale choice, write config.json
// (unless it exists), then bind the database. The binding is the only
// completion record; every earlier step can run again.
func (p *prepared) migrate(ctx context.Context, l *config.Loaded, plan *legacyPlan, dbPath, configPath string, create bool) error {
	if err := p.open(l, dbPath); err != nil {
		return err
	}
	if err := prepareLegacyPrivateUpgrade(ctx, p.st, plan.flags, plan.grants); err != nil {
		return err
	}
	if err := stop("db"); err != nil {
		return err
	}
	if create {
		h, err := config.Create(configPath, plan.doc)
		if err != nil {
			return err
		}
		p.doc, p.hash = plan.doc, h
	}
	if err := stop("config"); err != nil {
		return err
	}
	c, _ := p.doc.Effective(nil)
	return p.bind(ctx, c.Host.InstanceID, configPath, plan.migrated)
}

func (p *prepared) bind(ctx context.Context, id, configPath string, migrated bool) error {
	now := time.Now()
	b := store.HostBinding{InstanceID: id, ConfigPath: configPath, BoundAt: now}
	if migrated {
		b.MigratedAt = &now
		p.migrated = true
	}
	if err := p.st.BindHost(ctx, b); err != nil {
		if errors.Is(err, store.ErrHostBindingMismatch) {
			return &ActionError{err}
		}
		return err
	}
	if migrated {
		p.notes = append(p.notes, fmt.Sprintf("migrated the legacy host configuration to %s (instance %s); the previous database is kept in %s. Reinstall the service with `flats install --config %s` so it runs from the config file",
			configPath, id, filepath.Join(p.dataDir, "backups"), configPath))
	}
	return nil
}

// checkFlags compares explicitly set legacy flags with an existing config.
// A difference is refused: flags never override config.json silently.
func (p *prepared) checkFlags(c *config.Config, s setup) error {
	conflicts := flagConflicts(c, s.flags)
	if len(conflicts) > 0 {
		return actionf("these flags differ from %s:\n  %s\nchange the config with `flats config set` while the host is stopped, or reinstall the service with `flats install --config %s` so it runs without legacy flags",
			s.configPath, strings.Join(conflicts, "\n  "), s.configPath)
	}
	p.flagsChecked = len(comparedFlags(s.flags)) > 0
	return nil
}

func inspectDB(path string) (store.Info, error) {
	info, err := store.Inspect(path)
	if errors.Is(err, store.ErrSchemaTooNew) || errors.Is(err, store.ErrNotFlats) {
		return info, &ActionError{fmt.Errorf("%s: %w", path, err)}
	}
	return info, err
}

// sameConfig checks, under the data lock, that config.json is still the
// file read before the lock was taken.
func sameConfig(path string, exists bool, hash string) error {
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist) && !exists:
		return nil
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return err
	case err == nil && exists && config.Hash(data) == hash:
		return nil
	}
	return actionf("%s changed while Flats was starting; retry", path)
}

// archiveProviderFile moves the legacy host permission file into backups/.
// config.json holds the grants once the database is bound.
func archiveProviderFile(dataDir string, now time.Time) error {
	src := filepath.Join(dataDir, provider.FileName)
	if _, err := os.Lstat(src); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err := stop("archive"); err != nil {
		return err
	}
	dir := filepath.Join(dataDir, "backups")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.Rename(src, filepath.Join(dir, "network-provider."+now.UTC().Format("20060102T150405.000000000Z")+".json"))
}

// legacyPlan is config.json computed from a legacy data directory and the
// flags of this run. Computing it writes nothing.
type legacyPlan struct {
	doc      *config.Document
	notes    []string
	migrated bool // the database held a legacy host
	flags    Options
	grants   provider.File // the host file before this run's flags
}

// hostFlags are the legacy flags of the host.* keys, in config key order.
var hostFlags = []struct {
	flag, key string
	value     func(Options) string
}{
	{"listen", "host.management_addr", func(o Options) string { return o.Listen }},
	{"local-addr", "host.local_addr", func(o Options) string { return o.LocalAddr }},
	{"console-host", "host.console_host", func(o Options) string { return o.ConsoleHost }},
	{"runtime", "host.server_runtime", func(o Options) string { return strconv.FormatBool(o.Runtime) }},
}

// legacyHostValues converts the explicitly set host flags to the config
// values that have the effect they had: a service name port becomes its
// number. A flag `flats serve` could not have started with is ignored and
// reported, and is then neither stored nor compared.
func legacyHostValues(o Options) (map[string]string, []string) {
	values, notes := map[string]string{}, []string(nil)
	scratch, _ := config.New(config.NewInstanceID(), string(filepath.Separator))
	for _, hf := range hostFlags {
		if !o.Set[hf.flag] {
			continue
		}
		v := hf.value(o)
		if host, port, err := net.SplitHostPort(v); err == nil && hf.flag != "console-host" {
			if _, err := strconv.ParseUint(port, 10, 16); err != nil && port != "" {
				if n, err := net.LookupPort("tcp", port); err == nil {
					notes = append(notes, fmt.Sprintf("--%s %s is stored as port %d", hf.flag, v, n))
					v = net.JoinHostPort(host, strconv.Itoa(n))
				}
			}
		}
		if err := scratch.Set(hf.key, v); err != nil {
			notes = append(notes, fmt.Sprintf("--%s %s was ignored: flats serve could not listen on it (%v)", hf.flag, v, err))
			continue
		}
		values[hf.key] = v
	}
	// Equal fixed addresses could never both listen. Ignore the explicit
	// local address, else the explicit management address.
	if err := scratch.Validate(); err != nil {
		for _, f := range []struct{ flag, key string }{{"local-addr", "host.local_addr"}, {"listen", "host.management_addr"}} {
			if v, ok := values[f.key]; ok {
				notes = append(notes, fmt.Sprintf("--%s %s was ignored: flats serve could not listen on both addresses", f.flag, v))
				delete(values, f.key)
				break
			}
		}
	}
	return values, notes
}

// flagRelays returns the relays of --relays, or none (and a note) when
// they are not valid relay URLs and so could not have been used.
func flagRelays(o Options) ([]string, string) {
	if len(o.Relays) == 0 {
		return nil, ""
	}
	r, err := portal.NormalizeRelays(o.Relays)
	if err != nil {
		return nil, fmt.Sprintf("--relays %s was ignored (%v); Portal could not have started with it", strings.Join(o.Relays, ","), err)
	}
	return r, ""
}

// inEffect maps a stored system setting outside the config range to the
// config value with the effect it had, with the reason. why is "" for a
// value in range.
func inEffect(setting string, n int64) (v int64, why string) {
	if n > config.MaxInt {
		return config.MaxInt, "above the largest value, which has the same effect"
	}
	switch {
	case n >= 1:
		return n, ""
	case setting == core.SetKeepVersions || setting == core.SetDiskQuotaBytes || setting == core.SetRedirectDays:
		if n < 0 {
			return 0, "a negative value worked like 0"
		}
		return n, ""
	case setting == core.SetRateLimit:
		return config.MaxInt, "a value below 1 turned rate limiting off; the largest rate never limits"
	case setting == core.SetPreviewTTL:
		return 1, "a value below 1 expired previews at once; the shortest stored time is 1 second"
	case setting == core.SetUploadMaxBytes:
		return 1, "a value below 1 refused every upload; the smallest stored limit is 1 byte"
	case setting == core.SetEventsKeep:
		return 1, "a value below 1 kept no events; the smallest stored count is 1"
	}
	return n, ""
}

// systemSettings are the legacy settings rows of the system.* keys.
var systemSettings = []string{core.SetUploadMaxBytes, core.SetKeepVersions, core.SetDiskQuotaBytes,
	core.SetPreviewTTL, core.SetRateLimit, core.SetRedirectDays, core.SetEventsKeep}

// planLegacy converts a legacy host to config.json so that every value is
// the one `flats serve` applied before config.json existed: explicit host
// flags, the host file plus this run's grants, the stored settings, and
// --relays over the stored relays. Stored values that serve ignored become
// the value it used instead, with a note.
func planLegacy(dataDir string, info store.Info, o Options, instanceID string) (*legacyPlan, error) {
	grants, _, err := provider.Read(dataDir)
	if err != nil {
		return nil, actionf("%s: %v", filepath.Join(dataDir, provider.FileName), err)
	}
	old, err := store.ReadLegacy(filepath.Join(dataDir, "flats.db"))
	if err != nil {
		return nil, err
	}
	doc, err := config.New(instanceID, dataDir)
	if err != nil {
		return nil, &ActionError{err}
	}
	plan := &legacyPlan{doc: doc, migrated: info.HasData, flags: o, grants: grants}
	note := func(format string, args ...any) { plan.notes = append(plan.notes, fmt.Sprintf(format, args...)) }
	set := func(key, value, from string) error {
		if err := doc.Set(key, value); err != nil {
			return actionf("%s cannot be stored in config.json: %v", from, err)
		}
		return nil
	}

	host, hostNotes := legacyHostValues(o)
	plan.notes = append(plan.notes, hostNotes...)
	for _, hf := range hostFlags {
		def, _ := config.Default(hf.key)
		if v, ok := host[hf.key]; ok && v != fmt.Sprint(def) {
			if err := set(hf.key, v, "--"+hf.flag+" "+v); err != nil {
				return nil, err
			}
		}
	}

	// Grants recorded in the host file plus those this run's flags record,
	// as hostGrants did. An explicit --portal=false kept the grant but never
	// started Portal, so Portal stays off.
	file := grants
	if o.Network == "tailscale" {
		file, _ = file.Grant(provider.Tailscale)
		file.PrivateBackend = "tailscale"
	} else if o.Set["network"] && o.Network == "local" {
		file.PrivateBackend = "local"
	}
	if o.Portal {
		file, _ = file.Grant(provider.Portal)
	}
	for _, raw := range o.Permit {
		id, err := provider.ParseID(raw)
		if err != nil {
			return nil, &ActionError{err}
		}
		file, _ = file.Grant(id)
	}
	var permitted []string
	for _, id := range file.Permitted {
		if id == provider.Portal && o.Set["portal"] && !o.Portal {
			note("--portal=false: portal is not permitted in config.json, as it was not started before; per-flat Portal permission stays in the database")
			continue
		}
		permitted = append(permitted, string(id))
	}
	if len(permitted) > 0 {
		if err := set("network.permitted", strings.Join(permitted, ","), "the network grants"); err != nil {
			return nil, err
		}
	}
	if file.PrivateBackend != "" {
		backend := "local"
		if o.Network == "tailscale" || (!o.Set["network"] && file.PrivateBackend == "tailscale" && file.Allows(provider.Tailscale)) {
			backend = "tailscale"
		}
		if err := set("network.private_backend", backend, "the private backend"); err != nil {
			return nil, err
		}
	}

	for _, k := range systemSettings {
		v, ok := old.Settings[k]
		if !ok {
			continue
		}
		key, _ := core.SettingConfigKey(k)
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			note("setting %s=%q was ignored (not an integer); %s keeps its default %s", k, v, key, core.Defaults[k])
			continue
		}
		if c, why := inEffect(k, n); why != "" {
			note("setting %s=%s: %s; %s is %d", k, v, why, key, c)
			n = c
		}
		if strconv.FormatInt(n, 10) == core.Defaults[k] {
			continue
		}
		if err := set(key, strconv.FormatInt(n, 10), fmt.Sprintf("setting %s=%s", k, v)); err != nil {
			return nil, err
		}
	}

	relays, flagNote := flagRelays(o)
	if flagNote != "" {
		note("%s", flagNote)
	}
	if len(relays) > 0 {
		if v := strings.TrimSpace(old.Settings[core.SetPortalRelays]); v != "" {
			note("--relays replaces the stored portal relays %q, as before", v)
		}
	} else if v := strings.TrimSpace(old.Settings[core.SetPortalRelays]); v != "" {
		r, err := portal.NormalizeRelays(strings.Split(v, ","))
		if err != nil {
			note("setting %s=%q was ignored (%v); portal.relays keeps the Portal default relays", core.SetPortalRelays, v, err)
		} else {
			relays = r
		}
	}
	if len(relays) > 0 {
		if err := set("portal.relays", strings.Join(relays, ","), "the portal relays"); err != nil {
			return nil, err
		}
	}
	if v, ok := old.Settings[core.SetPortalDiscover]; ok {
		on, err := strconv.ParseBool(strings.TrimSpace(v))
		switch {
		case err != nil:
			note("setting %s=%q was ignored (want true or false); portal.discovery stays true", core.SetPortalDiscover, v)
		case !on && len(relays) == 0:
			note("setting %s=false was ignored (no relays are configured); portal.discovery stays true", core.SetPortalDiscover)
		case !on:
			if err := set("portal.discovery", "false", "setting "+core.SetPortalDiscover+"=false"); err != nil {
				return nil, err
			}
		}
	}
	if v, ok := old.Settings[core.SetPortalMaxRelay]; ok {
		n, err := strconv.Atoi(strings.TrimSpace(v))
		switch {
		case err != nil || n < 0:
			note("setting %s=%q was ignored (want a non-negative integer); portal.max_active_relays keeps its default %d", core.SetPortalMaxRelay, v, portal.DefaultMaxActiveRelays)
		case n == 0:
			note("setting %s=0 selected the Portal default; portal.max_active_relays keeps its default %d", core.SetPortalMaxRelay, portal.DefaultMaxActiveRelays)
		case n == portal.DefaultMaxActiveRelays:
		case n > math.MaxInt32:
			// No relay set comes near this; the cap stays unreachable.
			note("setting %s=%s is stored as %d, the largest value", core.SetPortalMaxRelay, v, math.MaxInt32)
			if err := set("portal.max_active_relays", strconv.Itoa(math.MaxInt32), "setting "+core.SetPortalMaxRelay); err != nil {
				return nil, err
			}
		default:
			if err := set("portal.max_active_relays", strconv.Itoa(n), "setting "+core.SetPortalMaxRelay+"="+v); err != nil {
				return nil, err
			}
		}
	}

	if o.Set["operator-credential-file"] && o.OperatorCredentialFile != "" {
		if err := set("credentials.operator_file", o.OperatorCredentialFile, "--operator-credential-file"); err != nil {
			return nil, err
		}
	}
	if o.AuthKeyFile != "" {
		if err := set("credentials.tailscale_authkey_file", o.AuthKeyFile, "--authkey-file"); err != nil {
			return nil, err
		}
	}
	if err := doc.Validate(); err != nil {
		return nil, &ActionError{fmt.Errorf("the converted config is invalid: %w", err)}
	}
	// Decide the legacy Private Tailscale choice now, from the host file as
	// it was, so an undecidable upgrade writes nothing.
	if old.PrivateUpgradePending {
		if _, err := legacyPrivateChoice(o, grants); err != nil {
			return nil, err
		}
	}
	return plan, nil
}

// legacyPrivateChoice decides whether flats from before the provider
// lifecycle keep Private Tailscale. It needs an explicit choice if the old
// default was Tailscale, so an ambiguous upgrade neither silently removes
// tailnet access nor contacts a live provider. Neither choice grants Funnel
// or Portal.
func legacyPrivateChoice(o Options, grants provider.File) (func(*store.Store, context.Context) error, error) {
	if o.Set["network"] && o.Network == "local" {
		return (*store.Store).DeclineLegacyTailscale, nil
	}
	if (o.Network == "tailscale" && grants.Migration.HistoricalTSNet) || (grants.PrivateBackend == "tailscale" && grants.Allows(provider.Tailscale)) {
		return (*store.Store).PreserveLegacyTailscale, nil
	}
	if grants.Migration.HistoricalTSNet {
		return nil, actionf("legacy Tailscale flats require an explicit upgrade choice: use --network tailscale to preserve Private tailnet access, or --network local to stop using it; neither choice grants Funnel or Portal")
	}
	return (*store.Store).DeclineLegacyTailscale, nil
}

// prepareLegacyPrivateUpgrade records the legacy Private Tailscale choice
// for flats still waiting for it. grants is the host file before this run's
// flags were applied.
func prepareLegacyPrivateUpgrade(ctx context.Context, st *store.Store, o Options, grants provider.File) error {
	pending, err := st.LegacyPrivateUpgradePending(ctx)
	if err != nil || !pending {
		return err
	}
	apply, err := legacyPrivateChoice(o, grants)
	if err != nil {
		return err
	}
	return apply(st, ctx)
}

// comparedFlags are the explicitly set flags that have a config.json key.
func comparedFlags(o Options) []string {
	var out []string
	for _, name := range []string{"listen", "local-addr", "console-host", "runtime", "network", "portal", "permit",
		"relays", "authkey-file", "operator-credential-file"} {
		if o.Set[name] {
			out = append(out, name)
		}
	}
	return out
}

// flagConflicts lists the explicitly set legacy flags whose effect differs
// from c. Flags left at their defaults are not compared.
func flagConflicts(c *config.Config, o Options) []string {
	var out []string
	differ := func(flag, value, key string, have any) {
		out = append(out, fmt.Sprintf("--%s %s, but %s is %s", flag, value, key, show(have)))
	}
	host, _ := legacyHostValues(o)
	for _, hf := range hostFlags {
		have, _, _ := c.Lookup(hf.key)
		if v, ok := host[hf.key]; ok && v != fmt.Sprint(have) {
			differ(hf.flag, hf.value(o), hf.key, have)
		}
	}
	permitted := func(id provider.ID) bool { return slices.Contains(c.Network.Permitted, string(id)) }
	if o.Set["network"] {
		switch {
		case o.Network == "tailscale" && !permitted(provider.Tailscale):
			differ("network", o.Network, "network.permitted", c.Network.Permitted)
		case c.Network.PrivateBackend != o.Network:
			differ("network", o.Network, "network.private_backend", c.Network.PrivateBackend)
		}
	}
	if o.Set["portal"] && o.Portal != permitted(provider.Portal) {
		differ("portal", strconv.FormatBool(o.Portal), "network.permitted", c.Network.Permitted)
	}
	for _, raw := range o.Permit {
		if id, err := provider.ParseID(raw); err == nil && id != provider.Local && !permitted(id) {
			differ("permit", raw, "network.permitted", c.Network.Permitted)
		}
	}
	if r, _ := flagRelays(o); len(r) > 0 && !slices.Equal(r, c.Portal.Relays) {
		differ("relays", strings.Join(o.Relays, ","), "portal.relays", c.Portal.Relays)
	}
	if o.Set["authkey-file"] && o.AuthKeyFile != c.Credentials.TailscaleAuthKeyFile {
		differ("authkey-file", o.AuthKeyFile, "credentials.tailscale_authkey_file", c.Credentials.TailscaleAuthKeyFile)
	}
	if o.Set["operator-credential-file"] && o.OperatorCredentialFile != c.Credentials.OperatorFile {
		differ("operator-credential-file", o.OperatorCredentialFile, "credentials.operator_file", c.Credentials.OperatorFile)
	}
	return out
}

func show(v any) string {
	switch v := v.(type) {
	case nil:
		return "unset"
	case string:
		if v == "" {
			return "unset"
		}
		return strconv.Quote(v)
	case []string:
		return "[" + strings.Join(v, ", ") + "]"
	}
	return fmt.Sprint(v)
}

// docDiff lists the lines of b that are not in a (+) and of a that are not
// in b (-), ignoring host.instance_id.
func docDiff(a, b *config.Document) string {
	lines := func(d *config.Document) []string {
		var out []string
		for _, l := range strings.Split(string(d.Encode()), "\n") {
			if !strings.Contains(l, `"instance_id"`) {
				out = append(out, strings.TrimSuffix(strings.TrimSpace(l), ","))
			}
		}
		return out
	}
	la, lb := lines(a), lines(b)
	var diff []string
	for _, l := range la {
		if !slices.Contains(lb, l) {
			diff = append(diff, "  - "+l)
		}
	}
	for _, l := range lb {
		if !slices.Contains(la, l) {
			diff = append(diff, "  + "+l)
		}
	}
	return strings.Join(diff, "\n")
}

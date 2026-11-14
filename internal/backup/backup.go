package backup

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/notebook"
	"github.com/gosuda/flats/internal/store"
)

// Options select one backup.
type Options struct {
	DataDir          string // the data directory of the host
	ConfigPath       string // the configuration file, "" when the host has none
	Out              string // the new folder to write
	ExcludeSecretKey bool   // leave secret.key out (rule 24); with SecretKeyPath, do not read the key
	SecretKeyPath    string // host.secret_key_path, "" for the key at <data>/secret.key
	FlatsVersion     string // recorded in the manifest
	Now              func() time.Time

	// afterStep, when set, runs after each numbered step of the spec. Tests use it to
	// change the host between two steps.
	afterStep     func(step int) error
	afterSnapshot func(rel string)
	midCopy       func(src string)
	order         func(all []dbEntry) []dbEntry // see copier.order
	maxGroup      int                           // overrides maxTogether
	busyTimeout   time.Duration                 // overrides busyTimeout
}

// Result reports a finished backup.
type Result struct {
	Out       string
	Files     int
	Bytes     int64
	Retries   int // the retries of rules 4 to 7
	Attempts  int // the attempts at coherence of rule 16, at least 1
	SecretKey string
	KeyPath   string // the external key's path, when SecretKey is "external"
	Skipped   []Skip
	Unstable  []string
}

func (o *Options) hook(step int) error {
	if o.afterStep == nil {
		return nil
	}
	return o.afterStep(step)
}

// isUnder reports whether child is the directory parent or lies below it.
func isUnder(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// Run makes the copy that docs/spec/host-backup.md describes. It never writes
// anything at Out except the finished copy (rules 21 and 23).
func Run(ctx context.Context, o Options) (res *Result, err error) {
	if o.Now == nil {
		o.Now = time.Now
	}
	// Step 12, preflight.
	data, err := filepath.EvalSymlinks(o.DataDir)
	if err != nil {
		return nil, err
	}
	flatsDB := filepath.Join(data, nameFlatsDB)
	if _, err := os.Stat(flatsDB); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, &RefusedError{fmt.Sprintf("%s has no %s: it is not the data directory of a Flats host", data, nameFlatsDB)}
		}
		return nil, err
	}
	out, err := filepath.Abs(o.Out)
	if err != nil {
		return nil, err
	}
	if _, err := os.Lstat(out); err == nil {
		return nil, &RefusedError{fmt.Sprintf("%s exists; a backup never replaces anything", out)}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(out))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, &RefusedError{fmt.Sprintf("the folder of --out, %s, does not exist", filepath.Dir(out))}
		}
		return nil, err
	}
	if fi, err := os.Stat(parent); err != nil || !fi.IsDir() {
		return nil, &RefusedError{fmt.Sprintf("the folder of --out, %s, is not a folder", filepath.Dir(out))}
	}
	final := filepath.Join(parent, filepath.Base(out))
	if isUnder(data, final) {
		return nil, &RefusedError{fmt.Sprintf("--out %s lies inside the data directory %s", out, data)}
	}
	if isUnder(final, data) {
		return nil, &RefusedError{fmt.Sprintf("the data directory %s lies inside --out %s", data, out)}
	}
	// A configured key (docs/spec/secret-key-path.md, rules 16 and 17): the checks of rule 3, and a key that
	// can be read unless --exclude-secret-key says that the command does not read it. These refusals come
	// before the partial directory exists.
	keyFile := core.NewKeyFile(data, o.SecretKeyPath)
	if keyFile.Configured() {
		if err := keyFile.Check(); err != nil {
			return nil, &RefusedError{err.Error()}
		}
		if !o.ExcludeSecretKey {
			if _, err := keyFile.Read(); err != nil {
				return nil, &RefusedError{fmt.Sprintf("%v; --exclude-secret-key makes the backup skip the key", err)}
			}
		}
	}
	// Last, because it is the one refusal that opens a database, and that can create a -shm file (rule 11).
	if err := checkSchema(flatsDB); err != nil {
		return nil, err
	}
	// Once, here, before any goroutine: the snapshots of one attempt open connections together, and the driver's
	// table of collations is not safe against a registration that overlaps an open (a data race that CI found).
	// collation_test.go registers it in init() as well, because the tests run in parallel in one process. So the
	// race lane cannot catch a move of this call into a goroutine: check that by reading, whenever this code changes.
	if err := notebook.RegisterCollation(); err != nil {
		return nil, err
	}
	var token [4]byte
	if _, err := rand.Read(token[:]); err != nil {
		return nil, err
	}
	partial := final + ".partial-" + hex.EncodeToString(token[:])
	if err := os.Mkdir(partial, 0o700); err != nil {
		return nil, err
	}
	published := false
	defer func() {
		if !published {
			os.RemoveAll(partial)
		}
	}()
	// With a configured key the walk leaves out <data>/secret.key as well (rule 16): the key is not part of the data.
	c := newCopier(ctx, data, partial, o.ExcludeSecretKey || keyFile.Configured())
	c.afterSnap, c.midCopy, c.order = o.afterSnapshot, o.midCopy, o.order
	if o.maxGroup > 0 {
		c.maxGroup = o.maxGroup
	}
	if o.busyTimeout > 0 {
		c.busy = o.busyTimeout
	}
	if err := os.Mkdir(filepath.Join(partial, "data"), 0o700); err != nil {
		return nil, err
	}
	if err := o.hook(12); err != nil {
		return nil, err
	}

	// Step 13, survey. The key is read now and again after step 18 (rule 20).
	s1, err := takeSurvey(data)
	if err != nil {
		return nil, err
	}
	key0, err := readKey(keyFile, o.ExcludeSecretKey, false)
	if err != nil {
		return nil, err
	}
	if err := o.hook(13); err != nil {
		return nil, err
	}

	// Step 14, write-once files, first pass.
	if err := c.pass(classWriteOnce); err != nil {
		return nil, err
	}
	if err := o.hook(14); err != nil {
		return nil, err
	}

	// Steps 15 and 16, the databases and their coherence.
	attempts, err := c.databasesAndCoherence(o.hook)
	if err != nil {
		return nil, err
	}

	// Step 17, write-once files, second pass.
	if err := c.pass(classWriteOnce); err != nil {
		return nil, err
	}
	if err := o.hook(17); err != nil {
		return nil, err
	}

	// Step 18, guest files and host files, and a configuration file outside the data directory.
	if err := c.pass(classGuest, classHost); err != nil {
		return nil, err
	}
	configState, err := c.copyExternalConfig(o.ConfigPath)
	if err != nil {
		return nil, err
	}
	if err := o.hook(18); err != nil {
		return nil, err
	}

	// Step 19, survey again, and rule 20, the key.
	s2, err := takeSurvey(data)
	if err != nil {
		return nil, err
	}
	if d := s1.diff(s2); d != "" {
		return nil, &RerunError{fmt.Errorf("a flat was created, renamed, deleted or restored during the copy (%s)", d)}
	}
	key1, err := readKey(keyFile, o.ExcludeSecretKey, true)
	if err != nil {
		if keyFile.Configured() {
			return nil, &RerunError{fmt.Errorf("the secret key at %s cannot be read any more: %w", keyFile.Path(), err)}
		}
		return nil, err
	}
	if !bytes.Equal(key0, key1) {
		return nil, &RerunError{fmt.Errorf("the secret key at %s changed during the copy", keyFile.Path())}
	}
	if err := o.hook(19); err != nil {
		return nil, err
	}

	// Step 20, verification, with the manifest that the copier holds.
	m := c.manifest()
	m.FlatsVersion = o.FlatsVersion
	m.Created = o.Now().UTC().Format(time.RFC3339)
	m.Attempts = attempts
	m.Config = configState
	// The bytes of a configured key go to the check of step 20: it opens the sealed values of the copy with
	// them, and it scans every file of the copy for them (rule 16 and 17 of the spec of the key path).
	var externalKey []byte
	switch {
	case keyFile.Configured():
		m.SecretKey, externalKey = SecretKeyExternal, key0
	case o.ExcludeSecretKey:
		m.SecretKey = SecretKeyExcluded
	case key0 != nil:
		m.SecretKey = SecretKeyIncluded
	default:
		m.SecretKey = SecretKeyAbsent
	}
	if fails, _, err := verifyTree(ctx, partial, m, externalKey); err != nil {
		return nil, err
	} else if len(fails) > 0 {
		err := fmt.Errorf("the copy failed its own check: %s: %s", fails[0].Path, fails[0].Reason)
		for _, f := range fails {
			if !f.Fixed {
				return nil, &RerunError{err}
			}
		}
		return nil, err
	}
	if err := o.hook(20); err != nil {
		return nil, err
	}

	// Step 21, publish. Nothing after this point may remove the folder.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := m.write(partial); err != nil {
		return nil, err
	}
	if err := syncTree(partial); err != nil {
		return nil, err
	}
	if err := renameNoReplace(partial, final); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return nil, &RefusedError{fmt.Sprintf("%s appeared during the backup; a backup never replaces anything", out)}
		}
		return nil, err
	}
	published = true

	res = &Result{Out: final, Files: len(m.Files), Attempts: attempts, Retries: c.retries, SecretKey: m.SecretKey, Skipped: m.Skipped, Unstable: m.Unstable}
	if m.SecretKey == SecretKeyExternal {
		res.KeyPath = keyFile.Path()
	}
	for _, f := range m.Files {
		res.Bytes += f.Size
	}
	return res, nil
}

// checkSchema refuses a flats.db that is not the database of a host of this
// release. The check of the copy (rule 25) refuses the same databases, and doing
// it first saves the copy that would end in that refusal. A host of an older
// release migrates its flats.db when it starts.
func checkSchema(flatsDB string) error {
	info, err := store.Inspect(flatsDB)
	switch {
	case errors.Is(err, store.ErrSchemaTooNew):
		return &RefusedError{fmt.Sprintf("%s was written by a newer Flats than this one", flatsDB)}
	case errors.Is(err, store.ErrNotFlats):
		return &RefusedError{fmt.Sprintf("%s is not a Flats database: %v", flatsDB, err)}
	case err != nil:
		return fmt.Errorf("read the schema version of %s: %w", flatsDB, err)
	case info.Version != info.Latest:
		return &RefusedError{fmt.Sprintf("%s has schema version %d and this Flats expects %d. Start the host once with this Flats, which migrates it, or copy the stopped data directory by hand", flatsDB, info.Version, info.Latest)}
	}
	return nil
}

// readKey returns the bytes of the host key, or nil when the key is excluded or the default file is not
// there. A configured key must be readable, and it is not read at all when it is excluded. The second
// read of rule 22 (again) does not repeat the checks of rule 3.
func readKey(k core.KeyFile, excluded, again bool) ([]byte, error) {
	switch {
	case excluded || !k.Configured():
		return readKeyBytes(k.DataDir(), excluded)
	case again:
		return k.ReadAgain()
	}
	return k.Read()
}

// readKeyBytes returns the bytes of secret.key, following a link as the host
// does, or nil when the file is not there or the key is excluded.
func readKeyBytes(data string, excluded bool) ([]byte, error) {
	if excluded {
		return nil, nil
	}
	b, err := os.ReadFile(filepath.Join(data, nameSecretKey))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return b, err
}

// maxTogether is how many outbox databases one attempt snapshots together with
// flats.db. It bounds the files that the command holds open at once on a host
// with many flats; the check of rule 16 covers the other databases.
const maxTogether = 64

// databasesAndCoherence runs steps 15 and 16. It snapshots the databases of the
// flats that the check does not need to pair with flats.db, one by one. Then it
// snapshots flats.db together with the outbox databases of the flats that have an
// import cursor, and checks every outbox database against the cursors in the copy
// of flats.db (rule 16). When a database does not fit its cursor, it snapshots
// the group again, up to maxTries attempts in all.
func (c *copier) databasesAndCoherence(hook func(int) error) (int, error) {
	dbs, err := c.flatDatabases()
	if err != nil {
		return 0, err
	}
	live, err := liveCursors(c.ctx, c.src(nameFlatsDB))
	if err != nil {
		return 0, fmt.Errorf("read the import cursors of flats.db: %w", err)
	}
	byRel := make(map[string]dbEntry, len(dbs))
	for _, d := range dbs {
		byRel[d.rel] = d
	}
	var group []dbEntry
	grouped := map[string]bool{}
	for _, flat := range slices.Sorted(maps.Keys(live)) {
		d, ok := byRel[outboxPath(flat, live[flat])]
		if ok && len(group) < c.maxGroup {
			group = append(group, d)
			grouped[d.rel] = true
		}
	}
	for _, d := range dbs {
		if !grouped[d.rel] {
			if err := c.snapshotDB(d.rel, d.kind, nil); err != nil {
				return 0, err
			}
		}
	}
	if err := hook(15); err != nil {
		return 0, err
	}
	dataRoot := filepath.Join(c.root, "data")
	for attempt := 1; ; attempt++ {
		if err := c.snapshotTogether(group); err != nil {
			return 0, err
		}
		cursors, err := readCursors(c.ctx, filepath.Join(dataRoot, nameFlatsDB))
		if err != nil {
			return 0, fmt.Errorf("read the import cursors of the copy: %w", err)
		}
		bad, err := incoherentCursors(c.ctx, dataRoot, cursors)
		if err != nil {
			return 0, err
		}
		if len(bad) == 0 {
			if err := c.sealDatabases(); err != nil {
				return 0, err
			}
			return attempt, hook(16)
		}
		if attempt == maxTries {
			names := make([]string, len(bad))
			for i, b := range bad {
				names[i] = b.String()
			}
			sort.Strings(names)
			return 0, &RerunError{fmt.Errorf("the outbox of %s does not fit its import cursor in flats.db on %d attempts; the flat was written during the copy", strings.Join(names, ", "), maxTries)}
		}
		c.retries++
		for _, b := range bad { // a database that was not in the group joins it, first
			d, ok := byRel[outboxPath(b.flat, b.typ)]
			if ok && !slices.ContainsFunc(group, func(g dbEntry) bool { return g.rel == d.rel }) {
				group = slices.Insert(group, 0, d)
			}
		}
		if len(group) > c.maxGroup {
			group = group[:c.maxGroup]
		}
	}
}

// copyExternalConfig copies a configuration file that lies outside the data
// directory, and its config-history folder, to the top of the copy (rule 1). It
// returns the value of Manifest.Config.
func (c *copier) copyExternalConfig(configPath string) (string, error) {
	if configPath == "" {
		return ConfigAbsent, nil
	}
	abs, err := filepath.EvalSymlinks(configPath)
	if errors.Is(err, fs.ErrNotExist) {
		return ConfigAbsent, nil
	}
	if err != nil {
		return "", err
	}
	if isUnder(c.data, abs) {
		return ConfigInData, nil
	}
	if err := c.copyTo(abs, filepath.Join(c.root, nameConfig), nameConfig, nameConfig, classHost); err != nil {
		return "", err
	}
	histDir := filepath.Join(filepath.Dir(abs), nameConfigHist)
	entries, err := os.ReadDir(histDir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	for _, de := range entries {
		if !de.Type().IsRegular() {
			continue
		}
		rel := nameConfigHist + "/" + de.Name()
		if err := c.copyTo(filepath.Join(histDir, de.Name()), filepath.Join(c.root, nameConfigHist, de.Name()), rel, rel, classHost); err != nil {
			return "", err
		}
	}
	return ConfigIncluded, nil
}

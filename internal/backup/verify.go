package backup

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/notebook"
	"github.com/gosuda/flats/internal/sheet"
	"github.com/gosuda/flats/internal/store"
)

// Failure is one thing that a check of a copy found. Fixed is true when a second
// run of the backup cannot clear it: a key that does not open a sealed value is
// the host's state, not a change during the copy.
type Failure struct {
	Path   string
	Reason string
	Fixed  bool
}

// VerifyResult is the outcome of Verify.
type VerifyResult struct {
	Manifest *Manifest
	Failures []Failure
	Notes    []string
}

// Verify checks the copy in root (rules 22 to 29 of the spec). It reads root and
// writes nothing. A manifest format or a flats.db schema version that this
// binary does not know is a *RefusedError. Anything else that fails the copy is
// in Failures.
func Verify(ctx context.Context, root string) (*VerifyResult, error) {
	m, err := readManifest(root)
	if errors.Is(err, errNoManifest) {
		return &VerifyResult{Failures: []Failure{{Path: ManifestFile, Reason: "is missing"}}}, nil
	}
	if err != nil {
		return nil, err
	}
	fails, notes, err := verifyTree(ctx, root, m, nil)
	if err != nil {
		return nil, err
	}
	return &VerifyResult{Manifest: m, Failures: fails, Notes: notes}, nil
}

type verifier struct {
	root, data string
	m          *Manifest
	// extKey is the key of a copy whose key is outside it (secret_key "external"), when the caller has
	// the bytes: Run does, a standalone Verify does not. The check opens the sealed values with it, and
	// every file of the copy is scanned for it (docs/spec/secret-key-path.md, rules 16 and 17).
	extKey []byte
	fails  []Failure
	notes  []string
	// listed are the canonical paths of the manifest's files, walked every
	// entry of the copy that is not a folder, by the name its folder gives
	// it, and regular those that are regular files. A check looks a manifest
	// path up in these and opens a file only by a path the walk found, so no
	// system resolves a spelling to another file than the one the checks
	// classify (rule 26).
	listed, walked, regular map[string]bool
	// dbs is the state of each database the manifest lists, by its path:
	// true when it passed quick_check, and a sheet.db the sheet check before
	// it. The later readers of a database read none other (dbReadable): those
	// of a sheet.db hold for the schema of this release only.
	dbs map[string]bool
	// unread are the databases the later checks did not read because the
	// manifest does not list them as databases, and badFlats the names
	// flats.db gives flats that are no folder of a copy.
	unread, badFlats map[string]bool
}

func (v *verifier) fail(path, format string, args ...any) {
	v.fails = append(v.fails, Failure{Path: path, Reason: fmt.Sprintf(format, args...)})
}

// verifyTree runs the checks of rules 25 to 28 on the copy in root against the
// manifest m, which Run holds in memory before it writes manifest.json. extKey is the bytes of an
// external key, or nil.
func verifyTree(ctx context.Context, root string, m *Manifest, extKey []byte) ([]Failure, []string, error) {
	v := &verifier{root: root, data: filepath.Join(root, "data"), m: m, extKey: extKey,
		listed: map[string]bool{}, walked: map[string]bool{}, regular: map[string]bool{}, dbs: map[string]bool{}, unread: map[string]bool{}, badFlats: map[string]bool{}}
	flatsDB := filepath.Join(v.data, nameFlatsDB)
	flatsHere, sidecar := false, false
	if _, err := os.Stat(flatsDB); err == nil {
		if names := sidecarsOf(flatsDB); len(names) > 0 {
			// store.Inspect opens a database that has a -wal or -journal file without immutable=1,
			// which can create a -shm file. A copy of this command holds none of the three (rule 25).
			sidecar = true
			v.fail("data/flats.db", "has %s beside it, and a copy made by this command holds no such file", strings.Join(names, ", "))
		} else {
			flatsHere = true
		}
	}
	if flatsHere {
		info, err := store.Inspect(flatsDB)
		switch {
		case errors.Is(err, store.ErrSchemaTooNew):
			return nil, nil, &RefusedError{fmt.Sprintf("data/flats.db has a newer schema than this Flats knows; check the copy with Flats %s", m.FlatsVersion)}
		case err != nil:
			v.fail("data/flats.db", "is not a Flats database: %v", err)
			flatsHere = false
		case info.Version != info.Latest:
			return nil, nil, &RefusedError{fmt.Sprintf("data/flats.db has schema version %d and this Flats expects %d; check the copy with Flats %s", info.Version, info.Latest, m.FlatsVersion)}
		}
	}
	if err := v.checkFiles(ctx); err != nil {
		return nil, nil, err
	}
	flatsOK := false
	if flatsHere || sidecar {
		flatsOK = v.checkDatabases(ctx) && flatsHere
	}
	if flatsOK {
		v.checkParts(ctx)
		v.checkKey(ctx)
	}
	return v.fails, v.notes, ctx.Err()
}

// sidecarsOf names the -wal, -shm and -journal files that lie beside the database.
func sidecarsOf(db string) []string {
	var names []string
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Lstat(db + suffix); err == nil {
			names = append(names, filepath.Base(db)+suffix)
		}
	}
	return names
}

// checkFiles is the manifest check of rule 26: every listed file is there with
// its size and hash, and nothing else is. A path the copy does not hold in
// that spelling fails before anything looks it up.
func (v *verifier) checkFiles(ctx context.Context) error {
	listed := make(map[string]File, len(v.m.Files))
	for _, f := range v.m.Files {
		if err := checkManifestPath(f.Path); err != nil {
			v.fail(f.Path, "is not a path a copy holds: it %v", err)
			continue
		}
		listed[f.Path] = f
		v.listed[f.Path] = true
	}
	seen := map[string]bool{}
	err := filepath.WalkDir(v.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(v.root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == ManifestFile {
			return nil
		}
		v.walked[rel] = true
		f, ok := listed[rel]
		switch {
		case !ok:
			v.fail(rel, "is not listed in the manifest")
			return nil
		case !d.Type().IsRegular():
			v.fail(rel, "is not a regular file")
			return nil
		}
		seen[rel] = true
		v.regular[rel] = true
		got, holdsKey, err := hashScan(p, v.extKey)
		if err != nil {
			return err
		}
		if holdsKey {
			// A second run cannot clear this: a name for the key file got into the copy.
			v.fails = append(v.fails, Failure{Path: rel, Reason: "holds the bytes of the secret key, which the copy must not hold", Fixed: true})
		}
		switch {
		case got.Size != f.Size:
			v.fail(rel, "has %d bytes, the manifest says %d", got.Size, f.Size)
		case got.SHA256 != f.SHA256:
			v.fail(rel, "has another SHA-256 than the manifest")
		}
		return nil
	})
	if err != nil {
		return err
	}
	var missing []string
	for p := range listed {
		if !seen[p] {
			missing = append(missing, p)
		}
	}
	sort.Strings(missing)
	for _, p := range missing {
		if !v.walked[p] {
			v.fail(p, "is missing")
		}
	}
	return nil
}

// checkDatabases runs PRAGMA quick_check on each database of the manifest (rule
// 27). It reports whether flats.db passed, which the later checks need. A
// database whose path is not canonical, or that the layout does not make a
// database of its kind, exactly as written, fails before anything opens it.
// A sheet.db is checked by the sheet package first, as a sheet checks it
// before its own quick_check: quick_check reads a value of another class in a
// column of numbers whole. It notes in v.dbs which databases passed.
func (v *verifier) checkDatabases(ctx context.Context) bool {
	flatsOK := false
	refused := map[string]string{}
	for _, r := range v.m.Refused {
		if checkDatabasePath(Database{Path: r.Path, Kind: KindSheet}) == nil {
			refused[r.Path] = r.Reason
		} else {
			v.fail(r.Path, "is listed as a refused sheet database, and it is none")
		}
	}
	found := map[string]bool{}
	for _, d := range v.m.Databases {
		if err := checkDatabasePath(d); err != nil {
			v.fail(d.Path, "%v", err)
			continue
		}
		var passed bool
		if reason, ok := refused[d.Path]; ok && d.Kind == KindSheet {
			found[d.Path] = true
			v.checkRefused(ctx, d, reason)
		} else {
			passed = v.checkDatabase(ctx, d)
		}
		if passed && d.Kind == KindFlats {
			flatsOK = true
		}
		// A path the manifest lists twice is read only when every listing
		// passed.
		was, twice := v.dbs[d.Path]
		v.dbs[d.Path] = passed && (was || !twice)
	}
	for p := range refused {
		if !found[p] {
			v.fail(p, "is listed as a refused sheet database, and the manifest lists no such database")
		}
	}
	return flatsOK
}

// checkRefused checks a sheet.db that the backup copied as it is because the
// sheet check refused it (rule 4): the check must still refuse it, and nothing
// else reads it, not even quick_check, which can read a value whole.
func (v *verifier) checkRefused(ctx context.Context, d Database, reason string) {
	p, ok := v.copied(d)
	if !ok {
		return
	}
	if err := preflightSheet(ctx, p); err != nil {
		v.notes = append(v.notes, fmt.Sprintf("%s is copied as it is, unchecked: the sheet check refuses it (%v), and so does a host, which keeps the sheet closed", d.Path, err))
		return
	}
	if ctx.Err() == nil {
		v.fail(d.Path, "is listed as refused by the sheet check (%s), and it passes the check", reason)
	}
}

// copied returns the path to open the database d at, and whether the copy
// holds it under the spelling of the manifest: as a listed regular file the
// walk found. checkFiles failed a listed file that is missing or irregular.
func (v *verifier) copied(d Database) (string, bool) {
	if !v.listed[d.Path] {
		v.fail(d.Path, "is listed as a database and not as a file of the copy")
		return "", false
	}
	if !v.regular[d.Path] {
		return "", false
	}
	return filepath.Join(v.root, filepath.FromSlash(d.Path)), true
}

// checkDatabase checks one database of the manifest and reports whether it
// passed.
func (v *verifier) checkDatabase(ctx context.Context, d Database) bool {
	p, ok := v.copied(d)
	if !ok {
		return false
	}
	if d.Kind == KindNotebook {
		if err := notebook.RegisterCollation(); err != nil {
			v.fail(d.Path, "cannot be checked: %v", err)
			return false
		}
	}
	if d.Kind == KindSheet {
		if err := preflightSheet(ctx, p); err != nil {
			v.fail(d.Path, "failed the sheet check before quick_check: %v", err)
			return false
		}
	}
	msgs, err := quickCheck(ctx, p)
	switch {
	case err != nil:
		v.fail(d.Path, "does not open: %v", err)
	case len(msgs) > 0:
		v.fail(d.Path, "failed quick_check: %s", strings.Join(msgs, "; "))
	default:
		return true
	}
	return false
}

// dbReadable reports whether the later checks may read the database at rel,
// below the data directory: only one that passed checkDatabases. A database
// the copy holds that the manifest does not list as one fails the check of
// the copy.
func (v *verifier) dbReadable(rel string) bool {
	p := manifestPath(rel)
	passed, listed := v.dbs[p]
	if passed {
		return true
	}
	if v.walked[p] && !listed && !v.unread[p] {
		v.unread[p] = true
		v.fail(p, "is not listed as a %s database in the manifest, so it was not checked", classify(rel).kind)
	}
	return false
}

// preflightSheet runs sheet.Preflight on a copied sheet.db. A test replaces
// it to measure the check without it.
var preflightSheet = func(ctx context.Context, path string) error {
	db, err := openCopy(path)
	if err != nil {
		return err
	}
	defer db.Close()
	return sheet.Preflight(ctx, db)
}

func quickCheck(ctx context.Context, path string) ([]string, error) {
	db, err := openCopy(path)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, `PRAGMA quick_check`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var bad []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		if s != "ok" {
			bad = append(bad, s)
		}
	}
	return bad, rows.Err()
}

// checkParts checks that the parts of the copy fit together (rules 16 and 27):
// every outbox database is coherent with flats.db, and every blob that a flat
// needs is in the copy.
func (v *verifier) checkParts(ctx context.Context) {
	flatsDB := filepath.Join(v.data, nameFlatsDB)
	cursors, err := readCursors(ctx, flatsDB)
	if err != nil {
		v.fail("data/flats.db", "cannot read the import cursors: %v", err)
		return
	}
	// A rejected database fails the check already, and the coherence check
	// reads it no further.
	cursors = slices.DeleteFunc(cursors, func(c cursor) bool {
		rel := outboxPath(c.flat, c.typ)
		return rel != "" && (!v.flatName(c.flat) || !v.dbReadable(rel))
	})
	bad, err := incoherentCursors(ctx, v.data, cursors)
	if err != nil {
		v.fail("data/flats.db", "cannot check the outbox databases: %v", err)
	}
	for _, b := range bad {
		v.fail(manifestPath(outboxPath(b.flat, b.typ)), "%s", b.reason())
	}
	st, err := store.OpenImmutable(flatsDB)
	if err != nil {
		v.fail("data/flats.db", "cannot be read: %v", err)
		return
	}
	defer st.Close()
	db, err := openCopy(flatsDB)
	if err != nil {
		v.fail("data/flats.db", "cannot be read: %v", err)
		return
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, `SELECT flat, type FROM content_state ORDER BY flat`)
	if err != nil {
		v.fail("data/flats.db", "cannot list the flats with revisions: %v", err)
		return
	}
	type flatType struct{ flat, typ string }
	var flats []flatType
	for rows.Next() {
		var f flatType
		if err := rows.Scan(&f.flat, &f.typ); err != nil {
			rows.Close()
			v.fail("data/flats.db", "cannot list the flats with revisions: %v", err)
			return
		}
		flats = append(flats, f)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		v.fail("data/flats.db", "cannot list the flats with revisions: %v", err)
		return
	}
	for _, f := range flats {
		if !v.flatName(f.flat) {
			continue
		}
		hashes, err := st.ReferencedHashes(ctx, f.flat)
		if err != nil {
			v.fail("data/flats.db", "cannot read the hashes of %s: %v", f.flat, err)
			continue
		}
		v.needBlobs(f.flat, f.typ, hashes)
		switch f.typ {
		case typeBoard, typeNotebook:
			v.needBlobs(f.flat, f.typ, v.assetHashes(ctx, f.flat, f.typ))
		case typeSheet:
			v.needBlobs(f.flat, f.typ, v.sheetHashes(ctx, f.flat))
		}
	}
}

// flatName reports whether flat, a name that flats.db of the copy gives a
// flat, is one folder of the layout of a copy, so that the paths the checks
// build from it name that flat's folder and no other. Another fails the
// check of the copy.
func (v *verifier) flatName(flat string) bool {
	if strings.Contains(flat, "/") || flat == nameLifecycle || checkManifestPath(manifestPath(nameFlats+"/"+flat)) != nil {
		if !v.badFlats[flat] {
			v.badFlats[flat] = true
			v.fail("data/flats.db", "names the flat %q, which is no folder of a copy", flat)
		}
		return false
	}
	return true
}

// blobRoot is the folder below a flat that holds its blobs.
func blobRoot(typ string) string {
	if typ == "design" {
		return "design"
	}
	return "revisions"
}

func (v *verifier) needBlobs(flat, typ string, hashes map[string]bool) {
	sorted := make([]string, 0, len(hashes))
	for h := range hashes {
		sorted = append(sorted, h)
	}
	sort.Strings(sorted)
	for _, h := range sorted {
		if !blobHash(h) {
			v.fail(manifestPath(join(nameFlats, flat)), "has the malformed blob hash %q", h)
			continue
		}
		rel := join(nameFlats, flat, blobRoot(typ), "blobs", h[:2], h)
		if !v.walked[manifestPath(rel)] {
			v.fail(manifestPath(rel), "is missing, and the database of flat %s needs it", flat)
		}
	}
}

// blobHash reports whether h is a SHA-256 in lowercase hex, the only form
// that names a blob.
func blobHash(h string) bool {
	if len(h) != sha256.Size*2 {
		return false
	}
	for i := 0; i < len(h); i++ {
		if c := h[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// assetHashes reads the hash of every row of the assets table of a flat's board
// or notebook database in the copy.
func (v *verifier) assetHashes(ctx context.Context, flat, typ string) map[string]bool {
	rel := outboxPath(flat, typ)
	path := filepath.Join(v.data, filepath.FromSlash(rel))
	if !v.dbReadable(rel) {
		return nil
	}
	if typ == typeNotebook {
		if err := notebook.RegisterCollation(); err != nil {
			v.fail(manifestPath(rel), "cannot be read: %v", err)
			return nil
		}
	}
	db, err := openCopy(path)
	if err != nil {
		v.fail(manifestPath(rel), "cannot be read: %v", err)
		return nil
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, `SELECT hash FROM assets`)
	if err != nil {
		v.fail(manifestPath(rel), "cannot read its assets: %v", err)
		return nil
	}
	out := map[string]bool{}
	for rows.Next() {
		var h sql.NullString
		if err := rows.Scan(&h); err != nil {
			rows.Close()
			v.fail(manifestPath(rel), "cannot read its assets: %v", err)
			return nil
		}
		if h.Valid {
			out[h.String] = true
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		v.fail(manifestPath(rel), "cannot read its assets: %v", err)
	}
	return out
}

// sheetHashes reads the hashes that a flat's sheet.db in the copy names and that the copy must hold as
// blobs: the files of the newest revision and of every pending outbox entry, which is what the host's blob
// sweep keeps for a sheet. A file whose bytes sheet.db still holds in its snapshots waits for its import
// and has no blob yet, so it is not in the result. The sheet package reads them as the sheet does: each
// list and hash only when it is of a class and a length the writer stores, and a list of no more files
// than a revision names.
func (v *verifier) sheetHashes(ctx context.Context, flat string) map[string]bool {
	rel := outboxPath(flat, typeSheet)
	path := filepath.Join(v.data, filepath.FromSlash(rel))
	if !v.dbReadable(rel) {
		return nil
	}
	db, err := openCopy(path)
	if err != nil {
		v.fail(manifestPath(rel), "cannot be read: %v", err)
		return nil
	}
	defer db.Close()
	out, err := sheet.BlobHashes(ctx, db)
	if err != nil {
		v.fail(manifestPath(rel), "cannot read its file lists: %v", err)
		return nil
	}
	return out
}

// checkKey is the key check of rule 28: the key in the copy opens every sealed
// value of flats.db. It skips the check when the copy has no key.
func (v *verifier) checkKey(ctx context.Context) {
	var key []byte
	keyName := "data/secret.key"
	switch {
	case v.m.SecretKey == SecretKeyIncluded:
		var err error
		if key, err = core.NewKeyFile(v.data, "").Read(); err != nil {
			v.fail("data/secret.key", "is not a usable key: %v", err)
			return
		}
	case v.m.SecretKey == SecretKeyExternal && v.extKey != nil:
		key, keyName = v.extKey, "the key at host.secret_key_path"
	case v.m.SecretKey == SecretKeyExternal:
		v.notes = append(v.notes, "the key is outside the copy, so the sealed values were not checked against a key")
		return
	default:
		v.notes = append(v.notes, "the copy holds no secret.key, so the sealed values were not checked against a key")
		return
	}
	st, err := store.OpenImmutable(filepath.Join(v.data, nameFlatsDB))
	if err != nil {
		v.fail("data/flats.db", "cannot be read: %v", err)
		return
	}
	defer st.Close()
	check, err := core.CheckKey(ctx, st, key)
	if err != nil {
		v.fail("data/flats.db", "cannot read the sealed values: %v", err)
		return
	}
	for _, n := range check.Unopened {
		v.fails = append(v.fails, Failure{Path: "data/flats.db", Reason: fmt.Sprintf("%s does not open with %s", n, keyName), Fixed: true})
	}
}

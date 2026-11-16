package backup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/notebook"
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
}

func (v *verifier) fail(path, format string, args ...any) {
	v.fails = append(v.fails, Failure{Path: path, Reason: fmt.Sprintf(format, args...)})
}

// verifyTree runs the checks of rules 25 to 28 on the copy in root against the
// manifest m, which Run holds in memory before it writes manifest.json. extKey is the bytes of an
// external key, or nil.
func verifyTree(ctx context.Context, root string, m *Manifest, extKey []byte) ([]Failure, []string, error) {
	v := &verifier{root: root, data: filepath.Join(root, "data"), m: m, extKey: extKey}
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
// its size and hash, and nothing else is.
func (v *verifier) checkFiles(ctx context.Context) error {
	listed := make(map[string]File, len(v.m.Files))
	for _, f := range v.m.Files {
		listed[f.Path] = f
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
		if _, err := os.Lstat(filepath.Join(v.root, filepath.FromSlash(p))); err != nil {
			v.fail(p, "is missing")
		}
	}
	return nil
}

// checkDatabases runs PRAGMA quick_check on each database of the manifest (rule
// 27). It reports whether flats.db passed, which the later checks need.
func (v *verifier) checkDatabases(ctx context.Context) bool {
	flatsOK := false
	for _, d := range v.m.Databases {
		p := filepath.Join(v.root, filepath.FromSlash(d.Path))
		if _, err := os.Stat(p); err != nil {
			continue // checkFiles reported it
		}
		if d.Kind == KindNotebook {
			if err := notebook.RegisterCollation(); err != nil {
				v.fail(d.Path, "cannot be checked: %v", err)
				continue
			}
		}
		msgs, err := quickCheck(ctx, p)
		switch {
		case err != nil:
			v.fail(d.Path, "does not open: %v", err)
		case len(msgs) > 0:
			v.fail(d.Path, "failed quick_check: %s", strings.Join(msgs, "; "))
		case d.Kind == KindFlats:
			flatsOK = true
		}
	}
	return flatsOK
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
		hashes, err := st.ReferencedHashes(ctx, f.flat)
		if err != nil {
			v.fail("data/flats.db", "cannot read the hashes of %s: %v", f.flat, err)
			continue
		}
		v.needBlobs(f.flat, f.typ, hashes)
		switch f.typ {
		case typeBoard, typeNotebook:
			v.needBlobs(f.flat, f.typ, v.assetHashes(ctx, f.flat, f.typ))
		}
	}
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
		if len(h) < 3 {
			v.fail(manifestPath(join(nameFlats, flat)), "has the malformed blob hash %q", h)
			continue
		}
		rel := join(nameFlats, flat, blobRoot(typ), "blobs", h[:2], h)
		if _, err := os.Lstat(filepath.Join(v.data, filepath.FromSlash(rel))); err != nil {
			v.fail(manifestPath(rel), "is missing, and the database of flat %s needs it", flat)
		}
	}
}

// assetHashes reads the hash of every row of the assets table of a flat's board
// or notebook database in the copy.
func (v *verifier) assetHashes(ctx context.Context, flat, typ string) map[string]bool {
	rel := outboxPath(flat, typ)
	path := filepath.Join(v.data, filepath.FromSlash(rel))
	if _, err := os.Stat(path); err != nil {
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

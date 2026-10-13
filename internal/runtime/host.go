package runtime

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"modernc.org/sqlite"
)

// dataStore backs env.DB and env.FILES for one worker.
type dataStore struct {
	dir string

	dbOnce sync.Once
	db     *sql.DB
	dbErr  error

	filesWriteMu sync.Mutex // serializes quota checks, mutations and accounting
	filesMu      sync.Mutex
	files        *os.Root
	filesUsed    atomic.Int64 // -1 until computed
	filesEntries int64        // -1 until computed; protected by filesWriteMu
}

func newDataStore(dir string) *dataStore {
	d := &dataStore{dir: dir}
	d.filesUsed.Store(-1)
	d.filesEntries = -1
	return d
}

func (d *dataStore) Close() {
	if d.db != nil {
		d.db.Close()
	}
	d.filesMu.Lock()
	if d.files != nil {
		d.files.Close()
	}
	d.filesMu.Unlock()
}

// sqliteLimitAttached is SQLITE_LIMIT_ATTACHED: 0 forbids ATTACH (and VACUUM
// INTO), which would otherwise let SQL read or write arbitrary host files.
const sqliteLimitAttached = 7

// sqliteLimitLength is SQLITE_LIMIT_LENGTH, the largest string or blob
// (default 1e9: randomblob(1e9) would allocate a gigabyte).
const (
	sqliteLimitLength = 0
	maxSQLValue       = 32 << 20
)

// sqliteHeapLimit caps SQLite's own allocations (page caches, sorts, temp
// tables) in the worker. PRAGMA hard_heap_limit can only lower it.
const sqliteHeapLimit = 256 << 20

func (d *dataStore) openDB() (*sql.DB, error) {
	d.dbOnce.Do(func() {
		if err := os.MkdirAll(d.dir, 0o700); err != nil {
			d.dbErr = err
			return
		}
		p := filepath.Join(d.dir, "db.sqlite")
		if strings.ContainsAny(p, "?#") {
			d.dbErr = fmt.Errorf("data directory %q contains ? or #", d.dir)
			return
		}
		// _defensive: SQLITE_DBCONFIG_DEFENSIVE (no writable_schema, no
		// sqlite_dbpage writes, no journal_mode=OFF); trusted_schema=0;
		// temp_store=2 keeps temporary tables and sorts in (capped) memory
		// instead of temp files outside the data directory. DELETE journals
		// bound rollback disk use to the page-capped database, unlike WAL,
		// which can grow without bound under a long-lived reader.
		d.db, d.dbErr = sql.Open("sqlite", p+"?_defensive=1&_pragma=busy_timeout(5000)&_pragma=journal_mode(DELETE)&_pragma=foreign_keys(1)"+
			"&_pragma=trusted_schema(0)&_pragma=temp_store(2)"+fmt.Sprintf("&_pragma=hard_heap_limit(%d)", sqliteHeapLimit))
	})
	return d.db, d.dbErr
}

// conn returns a new connection with the sandbox limits applied. Each JS
// runtime owns one, so transactions of concurrent requests do not mix.
func (d *dataStore) conn(ctx context.Context) (*sql.Conn, error) {
	db, err := d.openDB()
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	c, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	if _, err := sqlite.Limit(c, sqliteLimitAttached, 0); err != nil {
		c.Close()
		return nil, fmt.Errorf("open database: %w", err)
	}
	if _, err := sqlite.Limit(c, sqliteLimitLength, maxSQLValue); err != nil {
		c.Close()
		return nil, fmt.Errorf("open database: %w", err)
	}
	if err := limitDBPages(ctx, c); err != nil {
		c.Close()
		return nil, fmt.Errorf("open database: %w", err)
	}
	return c, nil
}

// The page budget is set on every connection because max_page_count is a
// connection-local SQLite limit. Existing databases above the budget fail
// closed instead of silently raising it. Page size is fixed for guest SQL.
func limitDBPages(ctx context.Context, c *sql.Conn) error {
	var pageSize int64
	if err := c.QueryRowContext(ctx, "PRAGMA main.page_size").Scan(&pageSize); err != nil {
		return err
	}
	if pageSize <= 0 || pageSize > MaxDBBytes {
		return fmt.Errorf("invalid SQLite page size %d", pageSize)
	}
	maxPages := int64(MaxDBBytes) / pageSize
	var applied int64
	if err := c.QueryRowContext(ctx, fmt.Sprintf("PRAGMA main.max_page_count = %d", maxPages)).Scan(&applied); err != nil {
		return err
	}
	if applied > maxPages {
		return fmt.Errorf("SQLite database exceeds %d MiB limit", MaxDBBytes>>20)
	}
	return nil
}

// checkSQL denies pragmas that can escape storage or page limits. SQLite
// pragma names are single identifiers, even when quoted; case-folding and
// substring matching also reject multi-statement attempts to change them.
func checkSQL(query string) error {
	q := strings.ToLower(query)
	for _, p := range []string{"temp_store_directory", "data_store_directory", "max_page_count", "page_size", "journal_mode"} {
		if strings.Contains(q, p) {
			return fmt.Errorf("PRAGMA %s is not allowed", p)
		}
	}
	// Keep temp storage in the capped SQLite heap, not in unbounded disk
	// files. Preserve the read-only introspection used by the runtime API.
	if strings.Contains(q, "temp_store") {
		trimmed := strings.TrimSpace(q)
		if trimmed != "pragma temp_store" && trimmed != "pragma temp_store;" {
			return errors.New("PRAGMA temp_store changes are not allowed")
		}
	}
	return nil
}

// sqlArgs converts JSON parameters to SQLite values.
func sqlArgs(raw json.RawMessage) ([]any, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var in []any
	if err := dec.Decode(&in); err != nil {
		return nil, fmt.Errorf("parameters: %w", err)
	}
	out := make([]any, len(in))
	for i, v := range in {
		switch x := v.(type) {
		case nil, string:
			out[i] = x
		case bool:
			if x {
				out[i] = int64(1)
			} else {
				out[i] = int64(0)
			}
		case json.Number:
			if n, err := x.Int64(); err == nil {
				out[i] = n
			} else if f, err := x.Float64(); err == nil {
				out[i] = f
			} else {
				return nil, fmt.Errorf("parameter %d: bad number %s", i+1, x)
			}
		default:
			b, _ := json.Marshal(x) // objects and arrays are stored as JSON text
			out[i] = string(b)
		}
	}
	return out, nil
}

func dbQuery(ctx context.Context, c *sql.Conn, query string, raw json.RawMessage) (string, error) {
	if err := checkSQL(query); err != nil {
		return "", err
	}
	args, err := sqlArgs(raw)
	if err != nil {
		return "", err
	}
	rows, err := c.QueryContext(ctx, query, args...)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	buf.WriteByte('[')
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	n := 0
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return "", err
		}
		if n > 0 {
			buf.WriteByte(',')
		}
		n++
		buf.WriteByte('{')
		for i, c := range cols {
			if i > 0 {
				buf.WriteByte(',')
			}
			k, _ := json.Marshal(c)
			buf.Write(k)
			buf.WriteByte(':')
			v := vals[i]
			switch x := v.(type) {
			case []byte:
				v = string(x)
			case time.Time:
				v = x.Format(time.RFC3339Nano)
			}
			b, err := json.Marshal(v)
			if err != nil {
				b = []byte("null")
			}
			buf.Write(b)
		}
		buf.WriteByte('}')
		if buf.Len() > MaxQueryResult {
			return "", fmt.Errorf("query result is larger than %d MiB; add a LIMIT", MaxQueryResult>>20)
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	buf.WriteByte(']')
	return buf.String(), nil
}

func dbExec(ctx context.Context, c *sql.Conn, query string, raw json.RawMessage) (string, error) {
	if err := checkSQL(query); err != nil {
		return "", err
	}
	args, err := sqlArgs(raw)
	if err != nil {
		return "", err
	}
	res, err := c.ExecContext(ctx, query, args...)
	if err != nil {
		return "", err
	}
	changes, _ := res.RowsAffected()
	last, _ := res.LastInsertId()
	b, _ := json.Marshal(map[string]int64{"changes": changes, "last_insert_id": last})
	return string(b), nil
}

// --- FILES ---

var errBadKey = errors.New("invalid key")

// checkKey validates a FILES key: a relative slash-separated path without
// empty, "." or ".." segments, backslashes or control characters.
func checkKey(k string) error {
	bad := func(why string) error { return fmt.Errorf("%w %q: %s", errBadKey, k, why) }
	switch {
	case k == "":
		return bad("empty")
	case len(k) > 512:
		return bad("longer than 512 bytes")
	case !utf8.ValidString(k):
		return bad("not UTF-8")
	case strings.HasPrefix(k, "/"):
		return bad("must not start with /")
	}
	for _, r := range k {
		if r < 0x20 || r == 0x7f || r == '\\' {
			return bad("control character or backslash")
		}
	}
	for _, seg := range strings.Split(k, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return bad(`empty, "." or ".." path segment`)
		}
		if strings.HasPrefix(seg, tmpPrefix) {
			return bad("reserved name")
		}
	}
	return nil
}

const tmpPrefix = ".flats-tmp-"

func (d *dataStore) filesRoot(create bool) (*os.Root, error) {
	d.filesMu.Lock()
	defer d.filesMu.Unlock()
	if d.files != nil {
		return d.files, nil
	}
	dir := filepath.Join(d.dir, "files")
	if create {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	r, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	d.files = r
	return r, nil
}

func (d *dataStore) fileGet(key string) (*string, error) {
	if err := checkKey(key); err != nil {
		return nil, err
	}
	r, err := d.filesRoot(false)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	b, err := r.ReadFile(key)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		var pe *fs.PathError
		if errors.As(err, &pe) && strings.Contains(err.Error(), "is a directory") {
			return nil, nil
		}
		return nil, err
	}
	s := string(b)
	return &s, nil
}

// usage caches FILES bytes and inode-consuming objects. The caller holds
// filesWriteMu, so the snapshot and all later mutations are serialized.
func (d *dataStore) usage(r *os.Root) (int64, int64, error) {
	if bytes := d.filesUsed.Load(); bytes >= 0 && d.filesEntries >= 0 {
		return bytes, d.filesEntries, nil
	}
	var bytes, entries int64
	err := fs.WalkDir(r.FS(), ".", func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p != "." {
			entries++ // count files, directories, symlinks and special files
		}
		if e.Type().IsRegular() {
			info, err := e.Info()
			if err != nil {
				return err
			}
			bytes += info.Size()
		}
		return nil
	})
	if err != nil {
		return 0, 0, fmt.Errorf("measure FILES usage: %w", err)
	}
	d.filesUsed.Store(bytes)
	d.filesEntries = entries
	return bytes, entries, nil
}

// missingDirs counts directories that MkdirAll would create. Checking every
// prefix also detects incompatible paths before attempting any mutation.
func missingDirs(r *os.Root, dir string) (int64, error) {
	if dir == "." {
		return 0, nil
	}
	var missing int64
	prefix := ""
	for _, segment := range strings.Split(dir, "/") {
		prefix = path.Join(prefix, segment)
		info, err := r.Stat(prefix)
		if errors.Is(err, fs.ErrNotExist) {
			missing++
			continue
		}
		if err != nil {
			return 0, err
		}
		if !info.IsDir() {
			return 0, fmt.Errorf("%q is not a directory", prefix)
		}
	}
	return missing, nil
}

func (d *dataStore) invalidateUsage() {
	d.filesUsed.Store(-1)
	d.filesEntries = -1
}

func (d *dataStore) filePut(key, data string) error {
	d.filesWriteMu.Lock()
	defer d.filesWriteMu.Unlock()
	if err := checkKey(key); err != nil {
		return err
	}
	if len(data) > MaxFileValue {
		return fmt.Errorf("value for %q is %d bytes; the limit is %d MiB", key, len(data), MaxFileValue>>20)
	}
	r, err := d.filesRoot(true)
	if err != nil {
		return err
	}
	var old int64
	exists := false
	if info, err := r.Stat(key); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("put %q: target is not a regular file", key)
		}
		old, exists = info.Size(), true
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("put %q: %w", key, err)
	}
	bytes, entries, err := d.usage(r)
	if err != nil {
		return err
	}
	if bytes-old+int64(len(data)) > MaxFilesTotal {
		return fmt.Errorf("FILES storage is full (limit %d MiB)", MaxFilesTotal>>20)
	}
	dir := path.Dir(key)
	newDirs, err := missingDirs(r, dir)
	if err != nil {
		return fmt.Errorf("put %q: %w", key, err)
	}
	// A put needs one temporary file inode even when replacing a key.
	// For a new key that inode becomes the final file after rename.
	if entries+newDirs+1 > MaxFilesObjects {
		return fmt.Errorf("FILES object quota is full (limit %d files and directories)", MaxFilesObjects)
	}
	if dir != "." {
		if err := r.MkdirAll(dir, 0o700); err != nil {
			d.invalidateUsage() // MkdirAll may have created some parents
			return fmt.Errorf("put %q: %w", key, err)
		}
	}
	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		d.invalidateUsage()
		return fmt.Errorf("put %q: random temporary name: %w", key, err)
	}
	tmp := path.Join(dir, tmpPrefix+hex.EncodeToString(rnd[:]))
	if err := r.WriteFile(tmp, []byte(data), 0o600); err != nil {
		r.Remove(tmp)
		d.invalidateUsage() // cleanup can fail, leaving an inode or bytes
		return fmt.Errorf("put %q: %w", key, err)
	}
	if err := r.Rename(tmp, key); err != nil {
		r.Remove(tmp)
		d.invalidateUsage()
		return fmt.Errorf("put %q: %w", key, err)
	}
	d.filesUsed.Add(int64(len(data)) - old)
	d.filesEntries += newDirs
	if !exists {
		d.filesEntries++
	}
	return nil
}

func (d *dataStore) fileDelete(key string) (bool, error) {
	d.filesWriteMu.Lock()
	defer d.filesWriteMu.Unlock()
	if err := checkKey(key); err != nil {
		return false, err
	}
	r, err := d.filesRoot(false)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	info, err := r.Stat(key)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, nil
	}
	if err := r.Remove(key); err != nil {
		return false, err
	}
	if d.filesUsed.Load() >= 0 {
		d.filesUsed.Add(-info.Size())
	}
	if d.filesEntries >= 0 {
		d.filesEntries--
	}
	// Remove now-empty parent directories (best effort).
	for dir := path.Dir(key); dir != "."; dir = path.Dir(dir) {
		if r.Remove(dir) != nil {
			break
		}
		if d.filesEntries >= 0 {
			d.filesEntries--
		}
	}
	return true, nil
}

const maxListKeys = 10000

func (d *dataStore) fileList(prefix string) ([]string, error) {
	if len(prefix) > 512 || strings.ContainsAny(prefix, "\\\x00") {
		return nil, fmt.Errorf("%w prefix %q", errBadKey, prefix)
	}
	r, err := d.filesRoot(false)
	if errors.Is(err, fs.ErrNotExist) {
		return []string{}, nil
	} else if err != nil {
		return nil, err
	}
	keys := []string{}
	err = fs.WalkDir(r.FS(), ".", func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if strings.HasPrefix(e.Name(), tmpPrefix) {
			return nil
		}
		if e.IsDir() {
			// Skip directories that cannot contain a matching key.
			if p != "." && !strings.HasPrefix(p+"/", prefix) && !strings.HasPrefix(prefix, p+"/") {
				return fs.SkipDir
			}
			return nil
		}
		if e.Type().IsRegular() && strings.HasPrefix(p, prefix) {
			if len(keys) >= maxListKeys {
				return fs.SkipAll
			}
			keys = append(keys, p)
		}
		return nil
	})
	sort.Strings(keys)
	return keys, err
}

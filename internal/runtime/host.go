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

	wal string // path of the write-ahead log, set by openDB

	filesWriteMu sync.Mutex // serializes quota checks, mutations and accounting
	filesMu      sync.Mutex
	files        *os.Root
	filesUsed    atomic.Int64 // bytes; -1 until computed
	filesEntries atomic.Int64 // files and directories; -1 until computed

	// Tests lower these; zero means MaxDBBytes and MaxFilesObjects.
	maxDBBytes      int64
	maxFilesObjects int64
}

func newDataStore(dir string) *dataStore {
	d := &dataStore{dir: dir}
	d.invalidateUsage()
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
		// instead of temp files outside the data directory.
		// journal_size_limit shrinks a write-ahead log that a large
		// transaction grew once it has been checkpointed and restarted.
		d.db, d.dbErr = sql.Open("sqlite", p+"?_defensive=1&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"+
			"&_pragma=trusted_schema(0)&_pragma=temp_store(2)"+fmt.Sprintf("&_pragma=hard_heap_limit(%d)&_pragma=journal_size_limit(%d)", sqliteHeapLimit, walSizeLimit))
		d.wal = p + "-wal"
	})
	return d.db, d.dbErr
}

// walSizeLimit is the size SQLite truncates a restarted write-ahead log to.
const walSizeLimit = 16 << 20

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
	if err := d.limitDBSize(ctx, c); err != nil {
		c.Close()
		return nil, fmt.Errorf("open database: %w", err)
	}
	return c, nil
}

// limitDBSize applies the per-flat database budget to one connection.
//
// The main file is capped with max_page_count, a connection-local limit
// that must be set on every connection. SQLite never lowers it below the
// current size, so a database that is already larger stays readable and
// writable within its existing pages but cannot grow.
//
// In WAL mode the write-ahead log is a separate file. A reader that keeps
// an old snapshot open stops checkpoints from restarting the log, so
// sustained overlapping reads with writes could grow it without bound. The
// commit hook refuses (rolls back) any commit while the log is over the
// budget; dbError then tries a truncating checkpoint. The log is therefore
// at most the budget plus the one transaction being refused, and that
// transaction is itself limited by max_page_count.
func (d *dataStore) limitDBSize(ctx context.Context, c *sql.Conn) error {
	limit := d.dbLimit()
	var pageSize int64
	if err := c.QueryRowContext(ctx, "PRAGMA main.page_size").Scan(&pageSize); err != nil {
		return err
	}
	if pageSize <= 0 || pageSize > limit {
		return fmt.Errorf("invalid SQLite page size %d", pageSize)
	}
	var applied int64
	if err := c.QueryRowContext(ctx, fmt.Sprintf("PRAGMA main.max_page_count = %d", limit/pageSize)).Scan(&applied); err != nil {
		return err
	}
	wal := d.wal
	return c.Raw(func(dc any) error {
		h, ok := dc.(interface{ RegisterCommitHook(sqlite.CommitHookFn) })
		if !ok {
			return fmt.Errorf("SQLite driver %T has no commit hook", dc)
		}
		h.RegisterCommitHook(func() int32 {
			if info, err := os.Stat(wal); err == nil && info.Size() > limit {
				return 1 // SQLite turns the COMMIT into a ROLLBACK
			}
			return 0
		})
		return nil
	})
}

func (d *dataStore) dbLimit() int64 {
	if d.maxDBBytes > 0 {
		return d.maxDBBytes
	}
	return MaxDBBytes
}

// SQLite result codes the runtime explains to guest code.
const (
	sqliteFull       = 13
	sqliteConstraint = 19
	sqliteCommitHook = sqliteConstraint | 2<<8 // SQLITE_CONSTRAINT_COMMITHOOK
)

// dbError explains storage-limit failures. A commit refused by the
// write-ahead-log budget also triggers a truncating checkpoint, which waits
// at most a second for readers to finish; if it succeeds, a retry commits.
func dbError(ctx context.Context, c *sql.Conn, err error) error {
	var se *sqlite.Error
	if !errors.As(err, &se) {
		return err
	}
	switch se.Code() {
	case sqliteCommitHook:
		var busy int64
		if c.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&busy) == nil {
			c.ExecContext(ctx, "PRAGMA busy_timeout = 1000")
			c.ExecContext(ctx, "PRAGMA main.wal_checkpoint(TRUNCATE)")
			c.ExecContext(ctx, fmt.Sprintf("PRAGMA busy_timeout = %d", busy))
		}
		return fmt.Errorf("database write-ahead log is over its %d MiB limit while other reads are active; the transaction was rolled back, retry later: %w", MaxDBBytes>>20, err)
	case sqliteFull:
		return fmt.Errorf("database is full: it is limited to %d MiB, or the host disk is full: %w", MaxDBBytes>>20, err)
	}
	return err
}

// Pragmas guest SQL must not use at all (they point SQLite at host
// directories) and pragmas it may read but not set (they would lift the
// storage limits or move temporary data out of the capped heap).
var (
	deniedPragmas   = map[string]bool{"temp_store_directory": true, "data_store_directory": true}
	readOnlyPragmas = map[string]bool{"max_page_count": true, "page_size": true, "journal_mode": true, "temp_store": true}
)

// checkSQL refuses denied pragmas. It finds PRAGMA statements with a
// tokenizer that skips comments and unquotes identifiers and strings, so
// table, column and value names such as page_size are not mistaken for
// pragmas while every spelling of a pragma name is caught. SQLite also
// accepts a string literal as a pragma or schema name ('page_size').
// The table-valued pragma_NAME functions take no value argument and cannot
// set anything.
func checkSQL(query string) error {
	toks := sqlTokens(query)
	punct := func(i int, p string) bool { return i < len(toks) && toks[i].kind == sqlPunct && toks[i].text == p }
	for i, t := range toks {
		if t.kind != sqlWord || !strings.EqualFold(t.text, "pragma") {
			continue
		}
		j := i + 1
		if punct(j+1, ".") {
			j += 2 // schema-qualified name
		}
		if j >= len(toks) || toks[j].kind == sqlPunct {
			continue
		}
		name := strings.ToLower(toks[j].text)
		set := punct(j+1, "=") || punct(j+1, "(")
		switch {
		case deniedPragmas[name]:
			return fmt.Errorf("PRAGMA %s is not allowed", name)
		case readOnlyPragmas[name] && set:
			return fmt.Errorf("PRAGMA %s can be read but not changed", name)
		}
	}
	return nil
}

type sqlToken struct {
	kind int
	text string // unquoted for identifiers and strings
}

const (
	sqlWord   = iota // keyword or bare identifier
	sqlIdent         // "identifier", `identifier` or [identifier]
	sqlString        // 'string'
	sqlPunct         // any other single byte
)

// sqlTokens splits SQL into words, quoted identifiers, strings and
// punctuation, dropping whitespace and comments. It follows SQLite's
// tokenizer (tokenize.c) for everything that separates or hides a word.
func sqlTokens(q string) []sqlToken {
	var toks []sqlToken
	for i := 0; i < len(q); {
		c := q[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v':
			i++
		case strings.HasPrefix(q[i:], "\uFEFF"):
			// SQLite skips a UTF-8 byte order mark where a token starts.
			// Inside a word it is an identifier byte, as below.
			i += len("\uFEFF")
		case strings.HasPrefix(q[i:], "--"):
			if n := strings.IndexByte(q[i:], '\n'); n >= 0 {
				i += n + 1
			} else {
				i = len(q)
			}
		case strings.HasPrefix(q[i:], "/*"):
			if n := strings.Index(q[i+2:], "*/"); n >= 0 {
				i += n + 4
			} else {
				i = len(q)
			}
		case c == '\'':
			var text string
			text, i = sqlQuoted(q, i, c)
			toks = append(toks, sqlToken{kind: sqlString, text: text})
		case c == '"' || c == '`':
			var text string
			text, i = sqlQuoted(q, i, c)
			toks = append(toks, sqlToken{kind: sqlIdent, text: text})
		case c == '[':
			end := strings.IndexByte(q[i:], ']')
			if end < 0 {
				end = len(q) - i
			}
			toks = append(toks, sqlToken{kind: sqlIdent, text: q[i+1 : i+end]})
			i += end + 1
		case sqlWordByte(c):
			j := i
			for j < len(q) && sqlWordByte(q[j]) {
				j++
			}
			toks = append(toks, sqlToken{kind: sqlWord, text: q[i:j]})
			i = j
		default:
			toks = append(toks, sqlToken{kind: sqlPunct, text: q[i : i+1]})
			i++
		}
	}
	return toks
}

// sqlQuoted reads a token quoted by quote starting at q[i], where a doubled
// quote stands for one. It returns the unquoted text and the next index.
func sqlQuoted(q string, i int, quote byte) (string, int) {
	var b strings.Builder
	for i++; i < len(q); i++ {
		if q[i] == quote {
			if i+1 < len(q) && q[i+1] == quote {
				b.WriteByte(quote)
				i++
				continue
			}
			return b.String(), i + 1
		}
		b.WriteByte(q[i])
	}
	return b.String(), len(q)
}

func sqlWordByte(c byte) bool {
	return c == '_' || c == '$' || c >= 0x80 || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
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
		return "", dbError(ctx, c, err)
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
		rows.Close()
		return "", dbError(ctx, c, err)
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
		return "", dbError(ctx, c, err)
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
	if bytes, entries := d.filesUsed.Load(), d.filesEntries.Load(); bytes >= 0 && entries >= 0 {
		return bytes, entries, nil
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
	d.filesEntries.Store(entries)
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
	d.filesEntries.Store(-1)
}

func (d *dataStore) filesObjectLimit() int64 {
	if d.maxFilesObjects > 0 {
		return d.maxFilesObjects
	}
	return MaxFilesObjects
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
	// Only new objects count: overwriting an existing key keeps working at
	// the limit (and above it, for stores that predate the limit). The
	// temporary file of an overwrite exists only until the rename.
	added := newDirs
	if !exists {
		added++
	}
	if limit := d.filesObjectLimit(); added > 0 && entries+added > limit {
		return fmt.Errorf("FILES object quota is full (limit %d files and directories)", limit)
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
	d.filesEntries.Add(added)
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
	if d.filesEntries.Load() >= 0 {
		d.filesEntries.Add(-1)
	}
	// Remove now-empty parent directories (best effort).
	for dir := path.Dir(key); dir != "."; dir = path.Dir(dir) {
		if r.Remove(dir) != nil {
			break
		}
		if d.filesEntries.Load() >= 0 {
			d.filesEntries.Add(-1)
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

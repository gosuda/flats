package sheet

import (
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"unsafe"

	"modernc.org/libc"
	sqlite3 "modernc.org/sqlite/lib"
)

// fullDisk is a scratch file system that tests can fill. While full is set,
// every write to a file under its directory that would make the file
// larger fails, as on a file system without a free block, which still
// overwrites the blocks a file has. SQLite's own VFS turns ENOSPC into
// SQLITE_FULL, and so does this one.
type fullDisk struct {
	dir  string
	full atomic.Bool
	// refused counts the writes that failed, and journal those of them to
	// a rollback journal.
	refused, journal atomic.Int64
}

// fullFS is a SQLite VFS in front of the default one. It passes every call
// on, and for the files it opens under the directory of a fullDisk it
// takes the writes first. What SQLite reads of it lives in SQLite's own
// memory, as SQLite's structures do.
var fullFS struct {
	once sync.Once
	err  int32
	base uintptr

	mu      sync.Mutex
	disks   []*fullDisk
	files   map[uintptr]*fullFile
	methods map[uintptr]uintptr // the methods of a file by those of the default VFS
}

type fullFile struct {
	disk    *fullDisk
	journal bool
	methods *sqlite3.Tsqlite3_io_methods // the default VFS's
}

const fullVFSName = "flats-test-full"

// newFullDisk returns a scratch root for Options.ScratchDir and the
// fullDisk over it. The collected-stamp store opens its file through the
// VFS once blankStore.vfs names it.
func newFullDisk(t testing.TB) (string, *fullDisk) {
	t.Helper()
	fullFS.once.Do(registerFullVFS)
	if fullFS.err != sqlite3.SQLITE_OK {
		t.Fatalf("registering the VFS: %d", fullFS.err)
	}
	root := t.TempDir()
	// SQLite names files by their path with symbolic links resolved.
	dir, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	d := &fullDisk{dir: dir + string(filepath.Separator)}
	fullFS.mu.Lock()
	fullFS.disks = append(fullFS.disks, d)
	fullFS.mu.Unlock()
	t.Cleanup(func() {
		fullFS.mu.Lock()
		defer fullFS.mu.Unlock()
		for i, other := range fullFS.disks {
			if other == d {
				fullFS.disks = append(fullFS.disks[:i], fullFS.disks[i+1:]...)
				break
			}
		}
	})
	return root, d
}

// at reads memory of SQLite at an address. SQLite passes addresses as
// uintptr; reading the variable as a pointer keeps vet and checkptr from
// taking the address for arithmetic.
func at[T any](p uintptr) *T { return *(**T)(unsafe.Pointer(&p)) }

// alloc allocates memory of SQLite for a T, with what malloc of the
// platform takes as a size.
func alloc[T any, N ~uint32 | ~uint64](malloc func(*libc.TLS, N) uintptr, tls *libc.TLS) uintptr {
	var v T
	return malloc(tls, N(unsafe.Sizeof(v)))
}

// funcAddr is the address SQLite calls a Go function by, and asFunc the
// function at such an address: SQLite's structures hold Go function values
// as addresses.
func funcAddr[F any](f F) uintptr { return *(*uintptr)(unsafe.Pointer(&f)) }

func asFunc[F any](p uintptr) F { return *(*F)(unsafe.Pointer(&p)) }

type (
	openFunc     = func(*libc.TLS, uintptr, uintptr, uintptr, int32, uintptr) int32
	writeFunc    = func(*libc.TLS, uintptr, uintptr, int32, int64) int32
	fileSizeFunc = func(*libc.TLS, uintptr, uintptr) int32
	closeFunc    = func(*libc.TLS, uintptr) int32
)

func registerFullVFS() {
	tls := libc.NewTLS()
	defer tls.Close()
	fullFS.base = sqlite3.Xsqlite3_vfs_find(tls, 0)
	if fullFS.base == 0 {
		fullFS.err = sqlite3.SQLITE_ERROR
		return
	}
	fullFS.files = map[uintptr]*fullFile{}
	fullFS.methods = map[uintptr]uintptr{}
	name, err := libc.CString(fullVFSName)
	p := alloc[sqlite3.Tsqlite3_vfs](libc.Xmalloc, tls)
	if err != nil || p == 0 {
		fullFS.err = sqlite3.SQLITE_NOMEM
		return
	}
	vfs := at[sqlite3.Tsqlite3_vfs](p)
	*vfs = *at[sqlite3.Tsqlite3_vfs](fullFS.base)
	vfs.FpNext, vfs.FzName, vfs.FxOpen = 0, name, funcAddr[openFunc](fullOpen)
	fullFS.err = sqlite3.Xsqlite3_vfs_register(tls, p, 0)
}

func fullOpen(tls *libc.TLS, _ uintptr, zName, pFile uintptr, flags int32, pOutFlags uintptr) int32 {
	base := at[sqlite3.Tsqlite3_vfs](fullFS.base)
	rc := asFunc[openFunc](base.FxOpen)(tls, fullFS.base, zName, pFile, flags, pOutFlags)
	file := at[sqlite3.Tsqlite3_file](pFile)
	if rc != sqlite3.SQLITE_OK || file.FpMethods == 0 || zName == 0 {
		return rc
	}
	name := libc.GoString(zName)
	fullFS.mu.Lock()
	defer fullFS.mu.Unlock()
	for _, d := range fullFS.disks {
		if !strings.HasPrefix(name, d.dir) {
			continue
		}
		wrapped := fullFS.methods[file.FpMethods]
		if wrapped == 0 {
			if wrapped = alloc[sqlite3.Tsqlite3_io_methods](libc.Xmalloc, tls); wrapped == 0 {
				return sqlite3.SQLITE_NOMEM
			}
			m := at[sqlite3.Tsqlite3_io_methods](wrapped)
			*m = *at[sqlite3.Tsqlite3_io_methods](file.FpMethods)
			m.FxWrite, m.FxClose = funcAddr[writeFunc](fullWrite), funcAddr[closeFunc](fullClose)
			fullFS.methods[file.FpMethods] = wrapped
		}
		fullFS.files[pFile] = &fullFile{disk: d, journal: flags&sqlite3.SQLITE_OPEN_MAIN_JOURNAL != 0,
			methods: at[sqlite3.Tsqlite3_io_methods](file.FpMethods)}
		file.FpMethods = wrapped
		break
	}
	return rc
}

func fullFileOf(pFile uintptr) *fullFile {
	fullFS.mu.Lock()
	defer fullFS.mu.Unlock()
	return fullFS.files[pFile]
}

func fullWrite(tls *libc.TLS, pFile, zBuf uintptr, iAmt int32, iOfst int64) int32 {
	f := fullFileOf(pFile)
	if f.disk.full.Load() {
		size := tls.Alloc(8)
		defer tls.Free(8)
		if rc := asFunc[fileSizeFunc](f.methods.FxFileSize)(tls, pFile, size); rc != sqlite3.SQLITE_OK {
			return rc
		}
		if iOfst+int64(iAmt) > *at[int64](size) {
			f.disk.refused.Add(1)
			if f.journal {
				f.disk.journal.Add(1)
			}
			return sqlite3.SQLITE_FULL
		}
	}
	return asFunc[writeFunc](f.methods.FxWrite)(tls, pFile, zBuf, iAmt, iOfst)
}

func fullClose(tls *libc.TLS, pFile uintptr) int32 {
	f := fullFileOf(pFile)
	fullFS.mu.Lock()
	delete(fullFS.files, pFile)
	fullFS.mu.Unlock()
	return asFunc[closeFunc](f.methods.FxClose)(tls, pFile)
}

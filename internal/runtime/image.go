package runtime

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io/fs"
	"os"
	"path/filepath"
	goruntime "runtime"
	"sync"
	"syscall"

	"github.com/gosuda/flats/internal/buildinfo"
	"github.com/gosuda/flats/internal/jsengine"
)

// Document images (built-in docs app only; workerSpec.ImagePath).
//
// A document runtime is captured right after the prelude and the docs
// module loaded and before __flats_init gave it this worker's generation
// and env (prelude.js), so the image holds only the docs app's code and the
// document's content. It is kept in memory for later runtimes of this
// process and written next to the document's runtime directory after the
// worker reported ready; later workers restore it instead of evaluating the
// ~900 KB bundle. Every failure falls back to loading the module as before.
//
// FLATS_NO_IMAGES=1 disables images (forced fallback). FLATS_FORCE_IMAGES=1
// makes a document runtime that cannot use or create an image a startup
// failure (forced images, for test suites).

func imagesEnabled() bool { return os.Getenv("FLATS_NO_IMAGES") != "1" }

func imagesForced() bool { return os.Getenv("FLATS_FORCE_IMAGES") == "1" }

// documentImage is this process's state for one image path. Lifecycle: load
// hands out the in-memory or file image (a corrupt file is deleted); a miss
// captures a fresh image, which stays in data until the worker reported
// ready (isReady) and write publishes it to path; refuse latches refused and
// the process loads documents as before.
type documentImage struct {
	path string

	mu      sync.Mutex
	img     *jsengine.Image
	binding string // the binding key img was built with
	data    []byte // serialized img waiting for the worker to report ready
	isReady bool   // the worker reported ready
	refused bool   // capture failed in this process: load documents as before
	logged  bool   // a failure was logged
}

var documentImages = struct {
	mu sync.Mutex
	m  map[string]*documentImage
}{m: map[string]*documentImage{}}

func documentImageFor(path string) *documentImage {
	documentImages.mu.Lock()
	defer documentImages.mu.Unlock()
	d := documentImages.m[path]
	if d == nil {
		d = &documentImage{path: path}
		documentImages.m[path] = d
	}
	return d
}

// newDocumentVM is newJSVM for a document runtime (TECH section 3).
func newDocumentVM(w *worker, ws wsSink) (*jsVM, error) {
	forced := imagesForced()
	d := documentImageFor(w.spec.ImagePath)
	binding, err := imageBinding(w)
	if err != nil {
		d.logOnce(w, "document image unavailable: "+err.Error())
		if forced {
			return nil, fmt.Errorf("document image: %w", err)
		}
		return newPlainVM(w, ws)
	}
	if d.isRefused() {
		if forced {
			return nil, errors.New("document image: capture was refused")
		}
		return newPlainVM(w, ws)
	}

	// 1. Restore the image this process holds, else the saved file.
	if img, fromFile := d.load(binding); img != nil {
		vm, err := restoreDocumentVM(w, ws, img, binding)
		if err == nil {
			if fromFile {
				d.keep(img, binding)
				// Once per worker; tests observe forced and disabled modes by it.
				if w.log != nil {
					w.log.Log("info", "document runtime restored from image")
				}
			}
			return vm, nil
		}
		d.discard(img, fromFile)
		if forced {
			return nil, fmt.Errorf("document image: restore: %w", err)
		}
	}

	// 2. Load as before, without __flats_init, and capture.
	vm := allocJSVM(w, ws)
	spec := vm.engineSpec()
	spec.ImageBinding = binding
	spec.ImagePath = w.spec.ImagePath
	if err := vm.newEngine(spec); err != nil {
		return nil, err
	}
	if err := vm.startup(vm.load); err != nil {
		vm.close()
		return nil, err
	}
	if err := d.capture(w, vm, binding); err != nil {
		d.refuse(w, err)
		if forced {
			vm.close()
			return nil, fmt.Errorf("document image: %w", err)
		}
	}

	// 3. Give the runtime its own generation and env.
	if err := vm.startup(vm.initRuntime); err != nil {
		vm.close()
		return nil, err
	}
	return vm, nil
}

func restoreDocumentVM(w *worker, ws wsSink, img *jsengine.Image, binding string) (*jsVM, error) {
	vm := allocJSVM(w, ws)
	spec := vm.engineSpec()
	spec.Prelude = ""
	spec.ImageBinding = binding
	spec.ImagePath = w.spec.ImagePath
	spec.Image = img
	if err := vm.newEngine(spec); err != nil {
		return nil, err
	}
	if err := vm.startup(vm.initRuntime); err != nil {
		vm.close()
		return nil, err
	}
	return vm, nil
}

// load returns the image this process holds for binding, else the file's.
// An unreadable or corrupt file is deleted.
func (d *documentImage) load(binding string) (img *jsengine.Image, fromFile bool) {
	d.mu.Lock()
	if d.img != nil && d.binding == binding {
		img = d.img
	}
	d.mu.Unlock()
	if img != nil {
		return img, false
	}
	b, err := os.ReadFile(d.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false
	}
	if err == nil {
		var decoded jsengine.Image
		decoded, err = jsengine.UnmarshalImage(b)
		if err == nil {
			img = &decoded
		}
	}
	if err != nil {
		_ = os.Remove(d.path)
		return nil, false
	}
	return img, true
}

// keep holds an image restored from the file for later runtimes.
func (d *documentImage) keep(img *jsengine.Image, binding string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.img, d.binding = img, binding
}

// discard forgets an image that failed to restore, deleting its file.
func (d *documentImage) discard(img *jsengine.Image, fromFile bool) {
	if fromFile {
		_ = os.Remove(d.path)
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.img == img {
		d.img, d.binding, d.data = nil, "", nil
	}
}

// capture takes an image of vm, keeps it, and writes it now if the worker
// already reported ready, else when it does.
func (d *documentImage) capture(w *worker, vm *jsVM, binding string) error {
	data, err := vm.eng.Snapshot()
	if err != nil {
		return err
	}
	img, err := jsengine.UnmarshalImage(data)
	if err != nil {
		return err
	}
	d.mu.Lock()
	d.img, d.binding = &img, binding
	d.data = data
	write := d.isReady
	if write {
		d.data = nil
	}
	d.mu.Unlock()
	if write {
		d.write(w, data)
	}
	return nil
}

func (d *documentImage) isRefused() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.refused
}

func (d *documentImage) refuse(w *worker, err error) {
	d.mu.Lock()
	d.refused = true
	d.mu.Unlock()
	d.logOnce(w, "document image unavailable: "+err.Error())
}

// ready runs after the worker reported ready: it writes a pending image.
func (d *documentImage) ready(w *worker) {
	d.mu.Lock()
	d.isReady = true
	data := d.data
	d.data = nil
	d.mu.Unlock()
	if data != nil {
		d.write(w, data)
	}
}

// write saves data at the image path through a temporary file in the same
// directory. It never creates directories: once the document's state
// directory is gone, the write fails.
func (d *documentImage) write(w *worker, data []byte) {
	err := func() error {
		f, err := os.CreateTemp(filepath.Dir(d.path), "image-*.tmp")
		if err != nil {
			return err
		}
		tmp := f.Name()
		_, err = f.Write(data)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err == nil {
			err = os.Chmod(tmp, 0o600)
		}
		if err == nil {
			err = os.Rename(tmp, d.path)
		}
		if err != nil {
			_ = os.Remove(tmp)
		}
		return err
	}()
	if err != nil {
		d.logOnce(w, "document image not saved: "+pathlessError(err))
	}
}

func (d *documentImage) logOnce(w *worker, msg string) {
	d.mu.Lock()
	logged := d.logged
	d.logged = true
	d.mu.Unlock()
	if !logged && w.log != nil {
		w.log.Log("warn", msg)
	}
}

// pathlessError keeps host paths out of the document's log.
func pathlessError(err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Op + ": " + pe.Err.Error()
	}
	var le *os.LinkError
	if errors.As(err, &le) {
		return le.Op + ": " + le.Err.Error()
	}
	return err.Error()
}

// imageBinding is the runtime package's part of the image key: the prelude,
// what newJSVM evaluates in which order, the running build, and the version
// files.
func imageBinding(w *worker) (string, error) {
	fp := w.versionFP
	if fp == "" {
		return "", errors.New("version files have no fingerprint")
	}
	prelude := sha256.Sum256([]byte(preludeJS))
	bi := buildinfo.Get()
	h := sha256.New()
	for _, s := range []string{
		"flats document image binding v1",
		hex.EncodeToString(prelude[:]),
		jsBindingOrder,
		w.spec.Entry,
		bi.Version, bi.Commit, fmt.Sprint(bi.Dirty), bi.Go,
		executableIdentity(),
		fp,
	} {
		h.Write(binary.LittleEndian.AppendUint64(nil, uint64(len(s))))
		h.Write([]byte(s))
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// executableIdentity identifies the running binary by size, modification
// time and inode, read once (at worker start: startPrewarm, or the first
// document runtime) so a binary replaced later never has its images
// attributed to this one. Linux stats /proc/self/exe, the running inode.
var executableIdentity = sync.OnceValue(func() string {
	var fi os.FileInfo
	var err error
	if goruntime.GOOS == "linux" {
		fi, err = os.Stat("/proc/self/exe")
	}
	if fi == nil {
		var exe string
		if exe, err = os.Executable(); err == nil {
			fi, err = os.Stat(exe)
		}
	}
	if err != nil {
		return "unknown: " + err.Error()
	}
	var ino uint64
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		ino = uint64(st.Ino)
	}
	return fmt.Sprintf("%d %d %d", fi.Size(), fi.ModTime().UnixNano(), ino)
})

// fingerprintFile adds the regular file p of the version directory dir to
// the version fingerprint, which covers every regular file by relative path,
// size, modification time and mode, in the sorted order of
// checkVersionDir's walk.
func fingerprintFile(h hash.Hash, dir, p string, e fs.DirEntry) error {
	fi, err := e.Info()
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(dir, p)
	if err != nil {
		return err
	}
	fmt.Fprintf(h, "%d:%s %d %d %o\n", len(rel), rel, fi.Size(), fi.ModTime().UnixNano(), fi.Mode())
	return nil
}

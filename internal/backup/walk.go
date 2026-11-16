package backup

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// irregular is the set of mode bits that make an entry something other than a
// regular file or a directory: a link, a junction, a pipe, a socket, a device.
const irregular = fs.ModeSymlink | fs.ModeIrregular | fs.ModeDevice | fs.ModeNamedPipe | fs.ModeSocket | fs.ModeCharDevice

// visit is one entry that the walk hands on. fi is nil when the entry was gone
// by the time its details were read.
type visit struct {
	rel string
	fi  fs.FileInfo
	cls classification
}

func (v visit) isDir() bool { return v.fi != nil && v.fi.IsDir() }

// kindOf names the kind of an irregular entry for the manifest.
func kindOf(m fs.FileMode) string {
	switch {
	case m&fs.ModeSymlink != 0:
		return "symbolic link"
	case m&fs.ModeNamedPipe != 0:
		return "named pipe"
	case m&fs.ModeSocket != 0:
		return "socket"
	case m&(fs.ModeDevice|fs.ModeCharDevice) != 0:
		return "device"
	}
	return "link or junction"
}

// followsLink reports whether rel is a file that the host follows when it is a
// link, and that the copy therefore takes by its target's bytes (rule 9).
func followsLink(rel string) bool { return rel == nameSecretKey || rel == nameRecovery }

// walk visits the data directory depth first, in name order. It leaves out and
// lists the excluded entries, skips and lists the entries that are not regular
// files or directories, and calls fn for every other entry, directories before
// their contents. A directory that is gone is not an error: rule 19 finds a
// flat that disappeared.
func (c *copier) walk(fn func(visit) error) error { return c.walkDir("", fn) }

func (c *copier) walkDir(rel string, fn func(visit) error) error {
	if err := c.ctx.Err(); err != nil {
		return err
	}
	entries, err := os.ReadDir(c.src(rel))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	incomplete := c.incompleteSnapshots(rel, entries)
	for _, de := range entries {
		crel := de.Name()
		if rel != "" {
			crel = join(rel, de.Name())
		}
		cls := classify(crel)
		if incomplete[de.Name()] || cls.class == classExcluded || (c.excludeKey && crel == nameSecretKey) {
			c.exclude(crel)
			continue
		}
		fi, err := de.Info()
		if errors.Is(err, fs.ErrNotExist) {
			if err := fn(visit{rel: crel, cls: cls}); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if fi.Mode()&irregular != 0 {
			if !followsLink(crel) || fi.Mode()&fs.ModeSymlink == 0 {
				c.skip(crel, kindOf(fi.Mode()))
				continue
			}
			target, err := os.Stat(c.src(crel))
			if err != nil || !target.Mode().IsRegular() {
				c.skip(crel, "link that does not lead to a regular file")
				continue
			}
			fi = target
		}
		if err := fn(visit{rel: crel, fi: fi, cls: cls}); err != nil {
			return err
		}
		if fi.IsDir() {
			if err := c.walkDir(crel, fn); err != nil {
				return err
			}
		}
	}
	return nil
}

// incompleteSnapshots names the entries of a flat's snapshots directory that
// belong to a snapshot with a directory and no completion marker. It returns
// nil for any other directory (rule 14).
func (c *copier) incompleteSnapshots(rel string, entries []fs.DirEntry) map[string]bool {
	seg := strings.Split(rel, "/")
	if len(seg) != 3 || seg[0] != nameFlats || seg[2] != "snapshots" {
		return nil
	}
	var out map[string]bool
	for _, de := range entries {
		name := de.Name()
		if !strings.HasSuffix(name, ".sqlite.data") || !de.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(c.src(rel), name, nameSnapshotMark)); err == nil {
			continue
		}
		if out == nil {
			out = map[string]bool{}
		}
		out[name] = true
		out[strings.TrimSuffix(name, ".data")] = true
	}
	return out
}

// pass copies every file of the given classes and creates the directories on
// the way. A file that an earlier pass already took is not copied again.
func (c *copier) pass(classes ...class) error {
	want := map[class]bool{}
	for _, cl := range classes {
		want[cl] = true
	}
	return c.walk(func(v visit) error {
		if v.isDir() {
			return c.mkdir(v.rel)
		}
		if !want[v.cls.class] {
			return nil
		}
		if _, done := c.files[manifestPath(v.rel)]; done {
			return nil
		}
		return c.copyFile(v.rel, v.cls.class)
	})
}

// databases lists the databases of the flats, in order, without flats.db.
type dbEntry struct {
	rel  string
	kind string
}

func (c *copier) flatDatabases() ([]dbEntry, error) {
	var out []dbEntry
	err := c.walk(func(v visit) error {
		if v.cls.class == classDatabase && v.cls.kind != KindFlats && !v.isDir() {
			out = append(out, dbEntry{v.rel, v.cls.kind})
		}
		return nil
	})
	return out, err
}

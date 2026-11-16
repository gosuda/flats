package backup

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// ident is the identity of a directory: the file identity that the system
// gives, and its creation time where the system has one (rule 13).
type ident struct {
	fi       fs.FileInfo
	birth    time.Time
	hasBirth bool
}

func identOf(path string) (ident, bool, error) {
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return ident{}, false, nil
	}
	if err != nil {
		return ident{}, false, err
	}
	id := ident{fi: fi}
	id.birth, id.hasBirth = birthTime(path, fi)
	return id, true, nil
}

func (a ident) same(b ident) bool {
	if !os.SameFile(a.fi, b.fi) {
		return false
	}
	return !(a.hasBirth && b.hasBirth) || a.birth.Equal(b.birth)
}

type journalInfo struct {
	size  int64
	mtime time.Time
}

// survey is what steps 13 and 19 list: the flat directories with three of their
// subdirectories, the entries of flats/.lifecycle and each restore journal.
type survey struct {
	dirs      map[string]ident
	lifecycle []string
	journals  map[string]journalInfo
}

// subdirs are the subdirectories of a flat that the survey identifies.
var surveyedSubdirs = []string{"data", "board", "notebook"}

func takeSurvey(data string) (*survey, error) {
	s := &survey{dirs: map[string]ident{}, journals: map[string]journalInfo{}}
	flats := filepath.Join(data, nameFlats)
	entries, err := os.ReadDir(flats)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	for _, de := range entries {
		name := de.Name()
		if name == nameLifecycle {
			inner, err := os.ReadDir(filepath.Join(flats, name))
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				return nil, err
			}
			for _, e := range inner {
				s.lifecycle = append(s.lifecycle, e.Name())
			}
			sort.Strings(s.lifecycle)
			continue
		}
		if !de.IsDir() {
			continue
		}
		dir := join(nameFlats, name)
		id, ok, err := identOf(filepath.Join(data, filepath.FromSlash(dir)))
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		s.dirs[dir] = id
		for _, sub := range surveyedSubdirs {
			sp := join(dir, sub)
			sid, ok, err := identOf(filepath.Join(data, filepath.FromSlash(sp)))
			if err != nil {
				return nil, err
			}
			if ok {
				s.dirs[sp] = sid
			}
		}
		jp := join(dir, nameJournal)
		if fi, err := os.Lstat(filepath.Join(data, filepath.FromSlash(jp))); err == nil {
			s.journals[jp] = journalInfo{fi.Size(), fi.ModTime()}
		} else if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
	}
	return s, nil
}

// isFlatDir reports whether a surveyed path is a flat directory and not one
// of its subdirectories.
func isFlatDir(p string) bool {
	return len(p) > len(nameFlats)+1 && filepath.ToSlash(filepath.Dir(p)) == nameFlats
}

// diff describes the first difference between two surveys that rule 19 counts
// as a create, rename, delete or data restore, or "" when there is none.
func (a *survey) diff(b *survey) string {
	paths := make([]string, 0, len(a.dirs))
	for p := range a.dirs {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		bid, ok := b.dirs[p]
		switch {
		case !ok:
			return fmt.Sprintf("%s is gone", p)
		case !a.dirs[p].same(bid):
			return fmt.Sprintf("%s was replaced", p)
		}
	}
	for p := range b.dirs {
		if _, ok := a.dirs[p]; !ok && isFlatDir(p) {
			return fmt.Sprintf("%s appeared", p)
		}
	}
	if fmt.Sprint(a.lifecycle) != fmt.Sprint(b.lifecycle) {
		return "flats/.lifecycle changed"
	}
	for p, ja := range a.journals {
		if jb, ok := b.journals[p]; !ok || jb != ja {
			return fmt.Sprintf("%s changed", p)
		}
	}
	for p := range b.journals {
		if _, ok := a.journals[p]; !ok {
			return fmt.Sprintf("%s appeared", p)
		}
	}
	return ""
}

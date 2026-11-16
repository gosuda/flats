package backup

import (
	"path"
	"strings"
)

// class is the way the copy treats a path (rule 2 of the spec).
type class int

const (
	classExcluded  class = iota // left out, listed in the manifest
	classWriteOnce              // blobs, versions, draft revisions, snapshots
	classDatabase               // a SQLite database, copied as a snapshot
	classGuest                  // a flat's own files, copied once
	classHost                   // every other file, copied until stable
)

// classification is the verdict of classify for one path.
type classification struct {
	class class
	kind  string // for classDatabase, the Kind of the database
}

// The names of the data directory that the layout is built from. The tests
// create every kind of flat on a real host and check that these names match.
const (
	nameFlatsDB      = "flats.db"
	nameFlats        = "flats"
	nameLifecycle    = ".lifecycle"
	nameSecretKey    = "secret.key"
	nameRecovery     = "secret.key.recovery"
	nameRecoveryID   = "secret.key.recovery.id"
	nameConfigHist   = "config-history"
	nameConfig       = "config.json"
	nameLock         = "flats.lock"
	nameDataDB       = "db.sqlite"
	nameBoardDB      = "board.db"
	nameNotebookDB   = "notebook.db"
	nameJournal      = "restore-journal.json"
	nameSnapshotMark = ".flats-snapshot-complete"
)

// topExcluded lists the top-level entries that rule 3 leaves out.
var topExcluded = map[string]bool{
	"logs": true, "cache": true, "run": true, "runtime": true, "backups": true, nameLock: true,
}

// flatExcluded lists the entries of a flat's directory that rule 3 leaves out.
var flatExcluded = map[string]bool{
	"health-check": true, "restore-trial": true, "data.restore": true, "data.restore-old": true,
}

// isDBSidecar reports whether name is the -wal, -shm or -journal file of a
// database named base.
func isDBSidecar(name, base string) bool {
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if name == base+suffix {
			return true
		}
	}
	return false
}

// classify places rel, a slash-separated path below the data directory, in a
// class. A directory takes the class of its path; a class of Excluded means that
// the walk does not enter it.
func classify(rel string) classification {
	seg := strings.Split(rel, "/")
	top := seg[0]
	switch {
	case topExcluded[top]:
		return classification{class: classExcluded}
	case top == nameFlatsDB:
		return classification{class: classDatabase, kind: KindFlats}
	case isDBSidecar(top, nameFlatsDB):
		return classification{class: classExcluded}
	case top != nameFlats:
		return classification{class: classHost}
	}
	if len(seg) < 3 || seg[1] == nameLifecycle {
		return classification{class: classHost}
	}
	rest := seg[2:]
	if flatExcluded[rest[0]] {
		return classification{class: classExcluded}
	}
	switch rest[0] {
	case "revisions", "design":
		switch {
		case len(rest) == 1:
			return classification{class: classHost}
		case rest[1] == "blobs":
			return classification{class: classWriteOnce}
		case rest[1] == "tmp":
			return classification{class: classExcluded}
		}
		return classification{class: classHost}
	case "versions", "draft-revs", "snapshots":
		return classification{class: classWriteOnce}
	case "data":
		return dbOrElse(rest[1:], nameDataDB, KindData, classGuest)
	case "board":
		return dbOrElse(rest[1:], nameBoardDB, KindBoard, classHost)
	case "notebook":
		return dbOrElse(rest[1:], nameNotebookDB, KindNotebook, classHost)
	case "previews":
		// previews/<host>/db.sqlite is a database; the rest of a preview is guest data.
		switch {
		case len(rest) == 3:
			return dbOrElse(rest[2:], nameDataDB, KindPreview, classGuest)
		case len(rest) > 3:
			return classification{class: classGuest}
		}
		return classification{class: classHost}
	}
	return classification{class: classHost}
}

// dbOrElse classifies the part of a path below a directory that holds one
// database: the database, its sidecars, or anything else.
func dbOrElse(below []string, db, kind string, other class) classification {
	if len(below) == 0 {
		return classification{class: other}
	}
	if len(below) == 1 {
		switch {
		case below[0] == db:
			return classification{class: classDatabase, kind: kind}
		case isDBSidecar(below[0], db):
			return classification{class: classExcluded}
		}
	}
	return classification{class: other}
}

// join builds a slash-separated relative path.
func join(parts ...string) string { return path.Join(parts...) }

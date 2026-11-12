// Package sqliteuri builds the SQLite URI of a database file.
package sqliteuri

import (
	"net/url"
	"path/filepath"
	"strings"
)

// File returns the file: URI of the absolute path. A Windows path gets the
// form SQLite documents, file:///C:/dir/name.db.
func File(path string) string {
	path = filepath.ToSlash(path)
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	u := url.URL{Scheme: "file", Path: path}
	return u.String()
}

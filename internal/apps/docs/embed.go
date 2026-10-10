// Package docs embeds the built-in collaborative document editor that runs
// "docs" content-type flats on the Flats JavaScript runtime.
//
// The app is an ordinary server-flat module tree (dist/). Core materializes it
// next to a generated content.js module (see docs/internal/docs-app.md) and starts
// it with the runtime manager; the app never sees the host file system.
package docs

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"sort"
	"sync"
)

//go:embed dist
var dist embed.FS

// Entry is the server module, relative to FS().
const Entry = "server.js"

// ContentModule is the generated module core writes next to the app files.
// App files must never use this name.
const ContentModule = "content.js"

// FS returns the app files (the contents of dist/).
func FS() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err)
	}
	return sub
}

var (
	hashOnce sync.Once
	hash     string
)

// Hash is a stable SHA-256 over the sorted app file paths and contents. It
// changes whenever the embedded app changes, so materialized copies can be
// keyed by it.
func Hash() string {
	hashOnce.Do(func() {
		var paths []string
		f := FS()
		_ = fs.WalkDir(f, ".", func(p string, d fs.DirEntry, err error) error {
			if err == nil && d.Type().IsRegular() {
				paths = append(paths, p)
			}
			return err
		})
		sort.Strings(paths)
		h := sha256.New()
		for _, p := range paths {
			b, err := fs.ReadFile(f, p)
			if err != nil {
				panic(err)
			}
			sum := sha256.Sum256(b)
			h.Write([]byte(p))
			h.Write([]byte{0})
			h.Write([]byte(hex.EncodeToString(sum[:])))
			h.Write([]byte{'\n'})
		}
		hash = hex.EncodeToString(h.Sum(nil))
	})
	return hash
}

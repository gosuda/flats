package board

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"mime"
	"path"
	"strings"
	"sync"
)

// appPrefix is the URL path of the board app's files.
const appPrefix = "/_board/app/"

// appFile is one embedded app file with its content version.
type appFile struct {
	data    []byte
	ctype   string
	version string // hex prefix of the content's sha256
	etag    string
}

// appBundle is the board app as served: its files and the shell that names
// them by version (11.7). The shell links boot.js, app.css and app.js with
// ?v=<version> and carries an import map that sends every module import to
// its versioned URL, so a browser can keep versioned files without asking
// again, and a new binary changes the URLs of exactly the files that
// changed. A version the binary does not have is not served (boot.js).
type appBundle struct {
	files     map[string]*appFile // by path below appPrefix
	shell     []byte
	shellETag string
	shellCSP  string
}

var app = sync.OnceValues(loadApp)

func loadApp() (*appBundle, error) {
	b := &appBundle{files: map[string]*appFile{}}
	err := fs.WalkDir(webFS, "web", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || p == "web/index.html" {
			return err
		}
		data, err := fs.ReadFile(webFS, p)
		if err != nil {
			return err
		}
		name := strings.TrimPrefix(p, "web/")
		ctype := mime.TypeByExtension(path.Ext(name))
		if strings.HasSuffix(name, ".js") {
			ctype = "text/javascript; charset=utf-8"
		}
		sum := sha256.Sum256(data)
		v := hex.EncodeToString(sum[:8])
		b.files[name] = &appFile{data: data, ctype: ctype, version: v, etag: `"` + v + `"`}
		return nil
	})
	if err != nil {
		return nil, err
	}
	shell, err := fs.ReadFile(webFS, "web/index.html")
	if err != nil {
		return nil, err
	}
	imports := map[string]string{}
	for name := range b.files {
		if strings.HasSuffix(name, ".js") && name != "boot.js" {
			imports[appPrefix+name] = b.url(name)
		}
	}
	// json.Marshal sorts the keys, so the map and its hash are stable.
	m, err := json.Marshal(map[string]any{"imports": imports})
	if err != nil {
		return nil, err
	}
	importMap := `<script type="importmap">` + string(m) + `</script>`
	sum := sha256.Sum256(m)
	b.shellCSP = strings.Replace(shellCSP, "script-src 'self'", "script-src 'self' 'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'", 1)
	for _, ref := range []struct{ attr, name string }{{"src", "boot.js"}, {"href", "app.css"}, {"src", "app.js"}} {
		old := fmt.Sprintf(`%s="%s%s"`, ref.attr, appPrefix, ref.name)
		if !bytes.Contains(shell, []byte(old)) {
			return nil, fmt.Errorf("the board shell does not link %s", ref.name)
		}
		shell = bytes.Replace(shell, []byte(old), []byte(fmt.Sprintf(`%s="%s"`, ref.attr, b.url(ref.name))), 1)
	}
	// The import map must come before the first module script.
	script := []byte(`<script type="module"`)
	if !bytes.Contains(shell, script) {
		return nil, fmt.Errorf("the board shell has no module script")
	}
	shell = bytes.Replace(shell, script, append([]byte(importMap+"\n"), script...), 1)
	b.shell = shell
	ss := sha256.Sum256(shell)
	b.shellETag = `"` + hex.EncodeToString(ss[:8]) + `"`
	return b, nil
}

// url is the versioned URL of an app file.
func (b *appBundle) url(name string) string {
	return appPrefix + name + "?v=" + b.files[name].version
}

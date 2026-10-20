package core

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/store"
)

// testBlobPath is blobPath for a hash the test knows is valid.
func testBlobPath(t *testing.T, root, hash string) string {
	t.Helper()
	p, ok := blobPath(root, hash)
	if !ok {
		t.Fatalf("blob hash %q refused", hash)
	}
	return p
}

// escapingHash is 64 bytes long, like a SHA-256 in hex, but joined as a
// blob path it names <parent of root>/eee...: outside the blob directory.
var escapingHash = "../" + strings.Repeat("e", 61)

// plantEscapeTarget creates the file that escapingHash would have named
// under root before hashes were validated, and returns its path.
func plantEscapeTarget(t *testing.T, root string) string {
	t.Helper()
	target := filepath.Join(root, "blobs", escapingHash[:2], escapingHash)
	if strings.HasPrefix(target, filepath.Join(root, "blobs")+string(filepath.Separator)) {
		t.Fatalf("%s is inside the blob directory", target)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	return target
}

func TestValidBlobHash(t *testing.T) {
	good := hashBytes([]byte("text"))
	if len(escapingHash) != len(good) {
		t.Fatal("escapingHash must have the length of a real hash")
	}
	if !validBlobHash(good) {
		t.Fatalf("%s refused", good)
	}
	for _, h := range []string{
		"",
		good[:63],
		good + "0",
		strings.ToUpper(good),
		"g" + good[1:],
		escapingHash,
		strings.Repeat("a", 31) + "/" + strings.Repeat("a", 32),
		strings.Repeat("a", 31) + `\` + strings.Repeat("a", 32),
		strings.Repeat("a", 31) + "." + strings.Repeat("a", 32),
		strings.Repeat("a", 31) + "\x00" + strings.Repeat("a", 32),
		strings.Repeat("a", 62) + "é",
	} {
		if validBlobHash(h) {
			t.Errorf("%q accepted", h)
		}
		if p, ok := blobPath(t.TempDir(), h); ok || p != "" {
			t.Errorf("blobPath(%q) = %q, %v", h, p, ok)
		}
	}
}

// readBlob refuses a hash that is not a SHA-256 before it touches the
// filesystem, even when the path the hash would name exists.
func TestReadBlobRefusesEscapingHash(t *testing.T) {
	root := filepath.Join(t.TempDir(), "store")
	plantEscapeTarget(t, root)
	b, err := readBlob(root, escapingHash)
	if !errors.Is(err, store.ErrNotFound) || b != nil {
		t.Fatalf("readBlob = %q, %v", b, err)
	}
}

// A docs worker that reports a revision document with a hash naming a file
// outside the blob directory gets the entry refused: the host neither
// trusts the existing file as the stored text nor fetches the text, and
// records and acknowledges nothing.
func TestDocsImportRefusesEscapingHash(t *testing.T) {
	s, _ := newTestService(t)
	rt := &contentRuntime{}
	s.cfg.Runtime = rt
	s.cfg.DocsApp = testDocsApp()
	rt.outbox = &fakeOutbox{}
	ctx := context.Background()
	v := saveDocs(t, s, "notes")
	plantEscapeTarget(t, s.docsStoreDir("notes"))
	before, err := s.st.ListRevisions(ctx, "notes", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	rt.outbox = &fakeOutbox{
		texts:   map[string]string{escapingHash: "outside"},
		entries: []docsEntry{{Seq: 1, Kind: "person", Source: v.Hash, Documents: []store.RevisionFile{{Path: "index.md", Hash: escapingHash, Size: 7}}}},
	}
	unlock := s.lock("notes")
	res, err := s.importDocsLocked(ctx, "notes")
	unlock()
	if !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "invalid revision entry") {
		t.Fatalf("import = %v", err)
	}
	if len(res.revisions) != 0 || res.stored[escapingHash] {
		t.Fatalf("import recorded the entry: %+v", res)
	}
	if rt.outbox.textReads != 0 || len(rt.outbox.acks) != 0 {
		t.Fatalf("import fetched %d texts, acknowledged %v", rt.outbox.textReads, rt.outbox.acks)
	}
	after, err := s.st.ListRevisions(ctx, "notes", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("revisions %d -> %d", len(before), len(after))
	}
}

// blobsElement matches a string naming the blob directory as a path element.
var blobsElement = regexp.MustCompile(`(^|[/\\])blobs($|[/\\])`)

// Only blobPath builds paths beneath a blob directory, and it touches no
// filesystem, so a hash that blobPath refuses can never reach the
// filesystem. Elsewhere a literal naming the blob directory may only appear
// in filepath.WalkDir(filepath.Join(root, "blobs"), ...), with root a store
// directory, as sweepBlobs walks it.
func TestOnlyBlobPathBuildsBlobPaths(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		allowed := map[*ast.BasicLit]bool{}
		ast.Inspect(f, func(n ast.Node) bool {
			if fn, ok := n.(*ast.FuncDecl); ok && fn.Name.Name == "blobPath" && fn.Recv == nil {
				return false
			}
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			if !isFilepathCall(call, "WalkDir") {
				return true
			}
			if join, ok := call.Args[0].(*ast.CallExpr); ok && isFilepathCall(join, "Join") && len(join.Args) == 2 && isStoreRoot(join.Args[0]) {
				if lit, ok := join.Args[1].(*ast.BasicLit); ok {
					allowed[lit] = true
				}
			}
			return true
		})
		for _, d := range f.Decls {
			if fn, ok := d.(*ast.FuncDecl); ok && fn.Recv == nil && (fn.Name.Name == "blobPath" || fn.Name.Name == "validBlobHash") {
				checkPure(t, fset, fn)
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if fn, ok := n.(*ast.FuncDecl); ok && fn.Name.Name == "blobPath" && fn.Recv == nil {
				return false
			}
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING || allowed[lit] {
				return true
			}
			if v, err := strconv.Unquote(lit.Value); err == nil && blobsElement.MatchString(v) {
				t.Errorf("%s: %s builds a blob path outside blobPath", fset.Position(lit.Pos()), lit.Value)
			}
			return true
		})
	}
}

// isStoreRoot reports whether e names a blob store's root: the variable root
// or a Service method returning a directory (s.storeDir(...) and the like),
// never an expression that could carry a hash.
func isStoreRoot(e ast.Expr) bool {
	if id, ok := e.(*ast.Ident); ok {
		return id.Name == "root"
	}
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || !strings.HasSuffix(sel.Sel.Name, "Dir") {
		return false
	}
	x, ok := sel.X.(*ast.Ident)
	return ok && x.Name == "s"
}

func isFilepathCall(call *ast.CallExpr, name string) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != name {
		return false
	}
	x, ok := sel.X.(*ast.Ident)
	return ok && x.Name == "filepath"
}

// checkPure fails the test if fn calls anything but validBlobHash,
// filepath.Join and len: blobPath must not touch the filesystem before it
// validates, nor after.
func checkPure(t *testing.T, fset *token.FileSet, fn *ast.FuncDecl) {
	t.Helper()
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if id, ok := call.Fun.(*ast.Ident); ok && (id.Name == "validBlobHash" || id.Name == "len") {
			return true
		}
		if isFilepathCall(call, "Join") {
			return true
		}
		t.Errorf("%s: %s calls %s", fset.Position(call.Pos()), fn.Name.Name, types.ExprString(call.Fun))
		return true
	})
}

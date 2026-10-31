package jsengine

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

type qjsBoundaryPackage struct {
	ImportPath   string
	Standard     bool
	Module       *qjsBoundaryModule
	Dir          string
	CgoFiles     []string
	GoFiles      []string
	TestGoFiles  []string
	XTestGoFiles []string
	Imports      []string
	TestImports  []string
	XTestImports []string
	DepOnly      bool
}

type qjsBoundaryModule struct {
	Main bool
}

func TestQjsImportBoundary(t *testing.T) {
	violations, err := findQjsBoundaryViolations(t.Context())
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	if len(violations) > 0 {
		t.Fatalf("only internal/jsengine may import internal/qjs, found %d offenders:\n%s", len(violations), strings.Join(violations, "\n"))
	}
}

func findQjsBoundaryViolations(ctx context.Context) ([]string, error) {
	repoRoot := "."
	for range 64 {
		if _, err := os.Stat(filepath.Join(repoRoot, "go.mod")); err == nil {
			break
		}
		repoRoot = filepath.Join(repoRoot, "..")
	}
	goMod, err := os.ReadFile(filepath.Join(repoRoot, "go.mod"))
	if err != nil {
		return nil, err
	}
	modPath := ""
	for line := range strings.Lines(string(goMod)) {
		if rest, ok := strings.CutPrefix(line, "module "); ok {
			modPath = strings.TrimSpace(rest)
			break
		}
	}
	if modPath == "" {
		return nil, errors.New("no module line in go.mod")
	}
	target := modPath + "/internal/qjs"

	cmd := exec.CommandContext(ctx, "go", "list", "-deps", "-json", "./...")
	cmd.Dir = repoRoot
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("go list: %v: %s", err, strings.TrimSpace(stderr.String()))
	}

	seen := map[string]bool{}
	var violations []string
	dec := jsontext.NewDecoder(&stdout)
	for {
		var pkg qjsBoundaryPackage
		err := json.UnmarshalDecode(dec, &pkg)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if pkg.ImportPath == "" || pkg.Standard || pkg.DepOnly {
			continue
		}
		if pkg.Module == nil || !pkg.Module.Main || strings.Contains(pkg.ImportPath, "/vendor/") {
			continue
		}
		for _, names := range [][]string{pkg.CgoFiles, pkg.GoFiles, pkg.TestGoFiles, pkg.XTestGoFiles} {
			for _, name := range names {
				trackFile(pkg.Dir, name)
			}
		}
		if pkg.ImportPath == modPath+"/internal/jsengine" || pkg.ImportPath == modPath+"/internal/qjs" {
			continue
		}
		if slices.Contains(pkg.Imports, target) ||
			slices.Contains(pkg.TestImports, target) ||
			slices.Contains(pkg.XTestImports, target) {
			if !seen[pkg.ImportPath] {
				seen[pkg.ImportPath] = true
				violations = append(violations, pkg.ImportPath)
			}
		}
	}
	return violations, nil
}

func trackFile(dir, name string) {
	if dir == "" || name == "" {
		return
	}
	f, err := os.Open(filepath.Join(dir, name))
	if err == nil {
		f.Close()
	}
}

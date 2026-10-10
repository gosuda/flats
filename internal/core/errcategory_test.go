package core

import (
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strconv"
	"testing"

	"github.com/gosuda/flats/internal/store"
)

// ErrorCategories must list exactly the values ErrorCategory returns, so
// every category has refusal documentation.
func TestErrorCategoriesListEveryCategory(t *testing.T) {
	errs := []error{
		ErrStaleApproval, ErrPublicStopUnconfirmed, ErrProviderNotPermitted, ErrProviderUnavailable,
		ErrRuntimeUnavailable, ErrUnavailable, ErrProviderInUse, ErrNotDeployed, ErrUnchangedContent,
		ErrProviderNotReady, ErrConfigOverridden, ErrConfigChanged, &DeployError{Cause: ErrInvalid},
		&DeployError{}, ErrConflict, ErrForbidden, ErrNotDocs, ErrDocumentNotFound, store.ErrNotFound, ErrInvalid,
	}
	var got []string
	for _, err := range errs {
		got = append(got, ErrorCategory(err))
	}
	if !slices.Equal(got, ErrorCategories) {
		t.Fatalf("categories = %v, want %v", got, ErrorCategories)
	}
}

// Every string ErrorCategory can return must be listed, so a new case cannot
// skip ErrorCategories (and its refusal page) even without a sentinel above.
func TestErrorCategoriesMatchSource(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "lifecycle.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var returned []string
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "ErrorCategory" {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if r, ok := n.(*ast.ReturnStmt); ok && len(r.Results) == 1 {
				if lit, ok := r.Results[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
					if s, _ := strconv.Unquote(lit.Value); s != "" {
						returned = append(returned, s)
					}
				}
			}
			return true
		})
	}
	listed := slices.Clone(ErrorCategories)
	slices.Sort(returned)
	slices.Sort(listed)
	if len(returned) == 0 || !slices.Equal(returned, listed) {
		t.Fatalf("ErrorCategory returns %v, ErrorCategories lists %v", returned, listed)
	}
}

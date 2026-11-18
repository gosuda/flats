package runtimeref

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/sheet"
)

// Who receives a refusal the sheet code builds. An agent meets the ones
// marked forAgents through get_sheet, update_sheet and the revision tools,
// and topic.sheet names each of them. The others never leave the browser
// routes of a sheet, so the guide leaves them out.
//
// The three session-only conflicts are built where only a person's session
// gets: stale_formula for formula text typed against an older screen,
// session_expired when a socket or a History request names a session, and
// receipt_expired for the outcome of a numbered action. undo_scratch is
// not among them. An undo receives it, and so does any write, an agent's
// included, that gives a cleared cell a record again while the store of
// collected stamps cannot be read.
const (
	forAgents  = "an agent tool returns it"
	forSession = "only a person's editing session receives it: an action, an undo or its receipt"
	forBrowser = "only the sheet's own browser routes answer with it: the page, the socket and the History panel"
)

// sheetRefusals lists every refusal the sheet code builds, as
// "category/reason" or "sheet_capacity/limit", apart from the invalid ones:
// those name the field that is wrong and need no list.
var sheetRefusals = map[string]string{
	"conflict/guard":             forAgents,
	"conflict/op_id_reuse":       forAgents,
	"conflict/read_expired":      forAgents,
	"conflict/rolled_back":       forAgents,
	"conflict/state_required":    forAgents,
	"conflict/state_too_old":     forAgents,
	"conflict/state_unknown":     forAgents,
	"conflict/structure_changed": forAgents,
	"conflict/tab_changed":       forAgents,
	"conflict/receipt_expired":   forSession,
	"conflict/session_expired":   forSession,
	"conflict/stale_formula":     forSession,

	"sheet_capacity/action_changes":     forAgents,
	"sheet_capacity/call_bytes":         forAgents,
	"sheet_capacity/call_cells":         forAgents,
	"sheet_capacity/cells":              forAgents,
	"sheet_capacity/columns":            forAgents,
	"sheet_capacity/disk_quota":         forAgents,
	"sheet_capacity/formula_depth":      forAgents,
	"sheet_capacity/formula_length":     forAgents,
	"sheet_capacity/formula_reads":      forAgents,
	"sheet_capacity/formulas":           forAgents,
	"sheet_capacity/recalculation_time": forAgents,
	"sheet_capacity/rows":               forAgents,
	"sheet_capacity/sheet_reads":        forAgents,
	"sheet_capacity/sheet_text":         forAgents,
	"sheet_capacity/tabs":               forAgents,
	"sheet_capacity/text_length":        forAgents,

	"unavailable/calculating":         forAgents,
	"unavailable/capture_budget":      forAgents,
	"unavailable/commit":              forAgents,
	"unavailable/ids_exhausted":       forAgents,
	"unavailable/load":                forAgents,
	"unavailable/measurement":         forAgents,
	"unavailable/outbox_backpressure": forAgents,
	"unavailable/revision":            forAgents,
	"unavailable/sheet_unavailable":   forAgents,
	"unavailable/stopped":             forAgents,
	"unavailable/storage":             forAgents,
	"unavailable/undo_scratch":        forAgents,
	"unavailable/writer_backpressure": forAgents,
	"unavailable/app_missing":         forBrowser,
	"not_found/route":                 forBrowser,
	"unavailable/cancelled":           forBrowser,
	"unavailable/connections":         forBrowser,
	"unavailable/history_backend":     forBrowser,
	"unavailable/sessions":            forBrowser,

	"not_found/tab": forAgents,
	"forbidden/":    forBrowser,
}

// unseenSheetRefusals lists the places where the sheet code sets the
// category, the reason or the limit of a refusal to a value that
// scanSheetRefusals cannot follow to a text in the source, each with why
// no reason of sheetRefusals is missing because of it. A new place of that
// kind fails the test until it is written so that the scan can follow it,
// or is listed here.
var unseenSheetRefusals = map[string]string{
	"internal/core/sheet.go:toSheetError": "it turns a host error that is no sheet refusal into one of the host's categories, with the error text as its reason: the text is not one of a list, and refusal.<category> explains each category",
}

// sheetRefusalFiles are the sources that build sheet refusals: the engine
// and the host code that answers sheet calls.
var sheetRefusalFiles = []string{
	"../internal/sheet/*.go", "../internal/sheet/formula/*.go", "../internal/core/sheet*.go",
	"../internal/api/sheet.go", "../internal/mcpx/sheet.go", "../internal/mcpx/design.go", "../internal/cli/sheet.go",
}

// refusalScan reads refusals out of Go source by its syntax.
//
// It finds every composite literal of sheet.WriteError and of
// formula.LimitError, whatever the order of its fields, also one that
// leaves its type out inside a slice, an array or a map literal of
// refusals, and follows the value of Code, Reason, Limit and Name to the
// texts it can have: a string literal, a constant, a local variable through
// every assignment to it, and a parameter through every call of its
// function, a closure bound to a name included, across any number of
// functions. A name inside a function stands for its innermost declaration
// in scope where it is used, by Go's scope rules. A field copied from
// another refusal (err.Reason) builds no new refusal and is passed over. It
// is a copy only when that declaration of err gives it a refusal type: a
// parameter, a result, a receiver or a var with a written type. A field of
// another value (cfg.Reason), also of a name declared with := or outside
// the function, is a value the scan cannot follow.
//
// It does not run the code and has no types. A value it cannot follow (the
// result of a call, a field of a table, an index) is reported in unseen and
// must be listed in unseenSheetRefusals. It would miss a refusal built
// without a literal of the two types, one inside a literal of a named
// slice or map type, a field set by assignment after the literal, and a
// call through a function value or an interface. TestSheetRefusalScan
// holds the literals the text names and the assignments to zero misses in
// the scanned sources, and shows on a sample what the scan follows and what
// it reports.
type refusalScan struct {
	fset     *token.FileSet
	built    map[string]string // "category/reason" to where it is built
	unseen   map[string]string // "file:function" to the expression
	literals int               // literals whose text names a refusal type
	elided   int               // refusal literals inside them without a type
	pkgs     map[string]*refusalPkg
	scopes   map[ast.Node]*declScope // the declarations of each function declaration
}

type refusalPkg struct {
	name   string
	consts map[string]string
	files  []*ast.File
}

// refusalFunc is a function declaration or a closure bound to a name, with
// the declaration it stands in.
type refusalFunc struct {
	pkg  *refusalPkg
	decl *ast.FuncDecl
	lit  *ast.FuncLit
	name string
}

// forwarded is a parameter whose value becomes the reason or limit of a
// refusal of the categories.
type forwarded struct {
	fn         *refusalFunc
	param      int
	categories []string
}

func scanSheetRefusals(t *testing.T, sources map[string]string) *refusalScan {
	t.Helper()
	s := &refusalScan{fset: token.NewFileSet(), built: map[string]string{}, unseen: map[string]string{}, pkgs: map[string]*refusalPkg{}, scopes: map[ast.Node]*declScope{}}
	paths := make([]string, 0, len(sources))
	for path := range sources {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	for _, path := range paths {
		file, err := parser.ParseFile(s.fset, path, sources[path], 0)
		if err != nil {
			t.Fatal(err)
		}
		dir := filepath.Dir(path)
		pkg := s.pkgs[dir]
		if pkg == nil {
			pkg = &refusalPkg{name: file.Name.Name, consts: map[string]string{}}
			s.pkgs[dir] = pkg
		}
		pkg.files = append(pkg.files, file)
		ast.Inspect(file, func(n ast.Node) bool {
			if spec, ok := n.(*ast.ValueSpec); ok && len(spec.Names) == len(spec.Values) {
				for i, name := range spec.Names {
					if text, ok := stringLiteral(spec.Values[i]); ok && name.Obj != nil && name.Obj.Kind == ast.Con {
						pkg.consts[name.Name] = text
					}
				}
			}
			return true
		})
	}
	var queue []forwarded
	for _, pkg := range s.pkgs {
		for _, file := range pkg.files {
			s.walk(pkg, file, nil, func(stack []*refusalFunc, n ast.Node) {
				lit, ok := n.(*ast.CompositeLit)
				if !ok {
					return
				}
				if kind := refusalType(pkg, lit.Type); kind != "" {
					s.literals++
					queue = append(queue, s.literal(pkg, stack, lit, kind)...)
					return
				}
				// A slice, an array or a map of refusals names the type
				// once, and its elements may leave it out.
				kind, depth := refusalElem(pkg, lit.Type)
				if kind == "" {
					return
				}
				s.literals++
				for _, el := range elidedLiterals(lit, depth) {
					s.elided++
					queue = append(queue, s.literal(pkg, stack, el, kind)...)
				}
			})
		}
	}
	done := map[string]bool{}
	for len(queue) > 0 {
		f := queue[0]
		queue = queue[1:]
		key := fmt.Sprintf("%p/%d/%v", f.fn.node(), f.param, f.categories)
		if done[key] {
			continue
		}
		done[key] = true
		for _, pkg := range s.pkgs {
			for _, file := range pkg.files {
				s.walk(pkg, file, nil, func(stack []*refusalFunc, n ast.Node) {
					call, ok := n.(*ast.CallExpr)
					if !ok || !f.fn.calledBy(pkg, stack, call) {
						return
					}
					if f.param >= len(call.Args) {
						s.miss(stack, call)
						return
					}
					queue = append(queue, s.record(pkg, stack, call.Args[f.param], f.categories)...)
				})
			}
		}
	}
	return s
}

func (f *refusalFunc) node() ast.Node {
	if f.lit != nil {
		return f.lit
	}
	return f.decl
}

// calledBy reports whether the call is one of f: by its name in its own
// package, as a method or with the package's name from another, and for a
// closure by the name it is bound to inside its declaration.
func (f *refusalFunc) calledBy(pkg *refusalPkg, stack []*refusalFunc, call *ast.CallExpr) bool {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		if fun.Name != f.name || pkg != f.pkg {
			return false
		}
		return f.lit == nil || (len(stack) > 0 && stack[0].decl == f.decl)
	case *ast.SelectorExpr:
		if fun.Sel.Name != f.name || f.lit != nil {
			return false
		}
		if x, ok := fun.X.(*ast.Ident); ok && pkg != f.pkg {
			return x.Name == f.pkg.name
		}
		return pkg == f.pkg && f.decl.Recv != nil
	}
	return false
}

// walk calls visit for every node of the file with the functions around
// it, the declaration first.
func (s *refusalScan) walk(pkg *refusalPkg, n ast.Node, stack []*refusalFunc, visit func([]*refusalFunc, ast.Node)) {
	ast.Inspect(n, func(child ast.Node) bool {
		if child == nil || child == n {
			return true
		}
		var fn *refusalFunc
		switch c := child.(type) {
		case *ast.FuncDecl:
			fn = &refusalFunc{pkg: pkg, decl: c, name: c.Name.Name}
		case *ast.FuncLit:
			fn = &refusalFunc{pkg: pkg, lit: c}
			if len(stack) > 0 {
				fn.decl, fn.name = stack[0].decl, boundName(stack[0].decl, c)
			}
		}
		visit(stack, child)
		if fn == nil {
			return true
		}
		s.walk(pkg, child, append(slices.Clone(stack), fn), visit)
		return false
	})
}

// boundName is the name a closure is assigned to in its declaration.
func boundName(decl *ast.FuncDecl, lit *ast.FuncLit) string {
	name := ""
	ast.Inspect(decl, func(n ast.Node) bool {
		if assign, ok := n.(*ast.AssignStmt); ok && len(assign.Lhs) == len(assign.Rhs) {
			for i, rhs := range assign.Rhs {
				if id, ok := assign.Lhs[i].(*ast.Ident); ok && rhs == ast.Expr(lit) {
					name = id.Name
				}
			}
		}
		return name == ""
	})
	return name
}

// refusalType reports whether a type expression names sheet.WriteError
// ("write") or formula.LimitError ("limit").
func refusalType(pkg *refusalPkg, expr ast.Expr) string {
	name, from := "", pkg.name
	switch e := expr.(type) {
	case *ast.Ident:
		name = e.Name
	case *ast.SelectorExpr:
		if x, ok := e.X.(*ast.Ident); ok {
			name, from = e.Sel.Name, x.Name
		}
	}
	switch {
	case name == "WriteError" && from == "sheet":
		return "write"
	case name == "LimitError" && from == "formula":
		return "limit"
	}
	return ""
}

// refusalElem reports whether a type expression is a slice, an array or a
// map whose elements are refusals, through depth levels of such types, and
// which of the two types they are.
func refusalElem(pkg *refusalPkg, expr ast.Expr) (kind string, depth int) {
	for {
		switch e := expr.(type) {
		case *ast.ArrayType:
			expr = e.Elt
		case *ast.MapType:
			expr = e.Value
		default:
			if depth == 0 {
				return "", 0
			}
			if star, ok := expr.(*ast.StarExpr); ok {
				expr = star.X
			}
			if kind := refusalType(pkg, expr); kind != "" {
				return kind, depth
			}
			return "", 0
		}
		depth++
	}
}

// elidedLiterals returns the literals without a type of their own that are
// elements of lit depth levels down.
func elidedLiterals(lit *ast.CompositeLit, depth int) []*ast.CompositeLit {
	var out []*ast.CompositeLit
	for _, elt := range lit.Elts {
		if kv, ok := elt.(*ast.KeyValueExpr); ok {
			elt = kv.Value
		}
		el, ok := elt.(*ast.CompositeLit)
		if !ok || el.Type != nil {
			continue
		}
		if depth == 1 {
			out = append(out, el)
		} else {
			out = append(out, elidedLiterals(el, depth-1)...)
		}
	}
	return out
}

// refusalValue reports whether the innermost declaration of a name in
// scope where it is used gives it a refusal type: a parameter, a result, a
// receiver or a var with a written type. A name declared with := has no
// written type and is no refusal here.
func (s *refusalScan) refusalValue(pkg *refusalPkg, stack []*refusalFunc, id *ast.Ident) bool {
	d := s.declOf(stack, id)
	if d == nil || d.typ == nil {
		return false
	}
	typ := d.typ
	if star, ok := typ.(*ast.StarExpr); ok {
		typ = star.X
	}
	return refusalType(pkg, typ) != ""
}

// declaration is a name declared inside a function declaration.
type declaration struct {
	scope ast.Node  // the function, block, clause or statement it belongs to
	from  token.Pos // where its scope begins
	typ   ast.Expr  // its written type, or nil
	fn    ast.Node  // for a parameter, a result or a receiver, its function
	param int       // for a parameter, its index; otherwise -1
}

// declScope holds the declarations of one function declaration, by name,
// and the identifiers that declare them.
type declScope struct {
	byName  map[string][]*declaration
	byIdent map[*ast.Ident]*declaration
}

// declOf returns the declaration an identifier inside the functions of
// the stack declares or refers to, or nil when it is declared outside them.
func (s *refusalScan) declOf(stack []*refusalFunc, id *ast.Ident) *declaration {
	if len(stack) == 0 {
		return nil
	}
	root := stack[0].node()
	ds := s.scopes[root]
	if ds == nil {
		ds = collectDecls(root)
		s.scopes[root] = ds
	}
	if d, ok := ds.byIdent[id]; ok {
		return d
	}
	// The innermost scope around the use where the name is declared and
	// visible decides. Scopes nest, so the smallest one is the innermost.
	var best *declaration
	for _, d := range ds.byName[id.Name] {
		if id.Pos() < d.from || id.Pos() >= d.scope.End() {
			continue
		}
		if best == nil || d.scope.End()-d.scope.Pos() < best.scope.End()-best.scope.Pos() {
			best = d
		}
	}
	return best
}

// collectDecls finds the declarations in a function declaration with the
// scopes Go gives them.
func collectDecls(root ast.Node) *declScope {
	ds := &declScope{byName: map[string][]*declaration{}, byIdent: map[*ast.Ident]*declaration{}}
	add := func(id *ast.Ident, d declaration) {
		if id == nil || id.Name == "_" {
			return
		}
		// := names an existing variable of the same scope again.
		for _, old := range ds.byName[id.Name] {
			if old.scope == d.scope {
				ds.byIdent[id] = old
				return
			}
		}
		ds.byName[id.Name] = append(ds.byName[id.Name], &d)
		ds.byIdent[id] = &d
	}
	fields := func(fn ast.Node, list *ast.FieldList, params bool) {
		if list == nil {
			return
		}
		index := 0
		for _, field := range list.List {
			if len(field.Names) == 0 {
				index++
			}
			for _, id := range field.Names {
				param := -1
				if params {
					param = index
				}
				add(id, declaration{scope: fn, from: fn.Pos(), typ: field.Type, fn: fn, param: param})
				index++
			}
		}
	}
	var parents []ast.Node
	scope := func() ast.Node {
		for i := len(parents) - 1; i >= 0; i-- {
			switch parents[i].(type) {
			case *ast.BlockStmt, *ast.CaseClause, *ast.CommClause, *ast.IfStmt, *ast.ForStmt, *ast.RangeStmt,
				*ast.SwitchStmt, *ast.TypeSwitchStmt, *ast.FuncLit, *ast.FuncDecl:
				return parents[i]
			}
		}
		return root
	}
	ast.Inspect(root, func(n ast.Node) bool {
		if n == nil {
			parents = parents[:len(parents)-1]
			return true
		}
		switch x := n.(type) {
		case *ast.FuncDecl:
			fields(x, x.Recv, false)
			fields(x, x.Type.Params, true)
			fields(x, x.Type.Results, false)
		case *ast.FuncLit:
			fields(x, x.Type.Params, true)
			fields(x, x.Type.Results, false)
		case *ast.AssignStmt:
			ts, inSwitch := parents[len(parents)-1].(*ast.TypeSwitchStmt)
			if x.Tok == token.DEFINE && !(inSwitch && ts.Assign == ast.Stmt(x)) {
				in := scope()
				for _, lhs := range x.Lhs {
					id, _ := lhs.(*ast.Ident)
					add(id, declaration{scope: in, from: x.End(), param: -1})
				}
			}
		case *ast.GenDecl:
			in := scope()
			for _, spec := range x.Specs {
				switch sp := spec.(type) {
				case *ast.ValueSpec:
					var typ ast.Expr
					if x.Tok == token.VAR {
						typ = sp.Type
					}
					for _, id := range sp.Names {
						add(id, declaration{scope: in, from: sp.End(), typ: typ, param: -1})
					}
				case *ast.TypeSpec:
					add(sp.Name, declaration{scope: in, from: sp.Name.Pos(), param: -1})
				}
			}
		case *ast.RangeStmt:
			if x.Tok == token.DEFINE {
				for _, e := range []ast.Expr{x.Key, x.Value} {
					id, _ := e.(*ast.Ident)
					add(id, declaration{scope: x, from: x.Body.Pos(), param: -1})
				}
			}
		case *ast.TypeSwitchStmt:
			// switch v := x.(type) declares v in each clause, with the
			// clause's type when it names exactly one.
			if assign, ok := x.Assign.(*ast.AssignStmt); ok && len(assign.Lhs) == 1 {
				if id, ok := assign.Lhs[0].(*ast.Ident); ok && id.Name != "_" {
					// The name itself refers to none of them.
					ds.byIdent[id] = &declaration{scope: x, from: x.End(), param: -1}
					for _, stmt := range x.Body.List {
						clause := stmt.(*ast.CaseClause)
						d := &declaration{scope: clause, from: clause.Colon, param: -1}
						if len(clause.List) == 1 {
							d.typ = clause.List[0]
						}
						ds.byName[id.Name] = append(ds.byName[id.Name], d)
					}
				}
			}
		}
		parents = append(parents, n)
		return true
	})
	return ds
}

func stringLiteral(expr ast.Expr) (string, bool) {
	lit, ok := expr.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	text, err := strconv.Unquote(lit.Value)
	return text, err == nil
}

// literal records the refusals of one literal, and returns the parameters
// its reason or limit comes from.
func (s *refusalScan) literal(pkg *refusalPkg, stack []*refusalFunc, lit *ast.CompositeLit, kind string) []forwarded {
	fields := map[string]ast.Expr{}
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			s.miss(stack, lit)
			return nil
		}
		if key, ok := kv.Key.(*ast.Ident); ok {
			fields[key.Name] = kv.Value
		}
	}
	if kind == "limit" {
		return s.record(pkg, stack, fields["Name"], []string{"sheet_capacity"})
	}
	code := s.resolve(pkg, stack, fields["Code"])
	reasons := []ast.Expr{fields["Reason"], fields["Limit"]}
	if code.copied {
		// The literal repeats another refusal, unless it gives it a
		// reason of its own.
		for _, reason := range reasons {
			if reason != nil && !s.resolve(pkg, stack, reason).copied {
				s.miss(stack, lit)
			}
		}
		return nil
	}
	if code.fn != nil || code.unknown || len(code.texts) == 0 {
		s.miss(stack, lit)
		return nil
	}
	categories := slices.DeleteFunc(code.texts, func(c string) bool { return c == "invalid" })
	if len(categories) == 0 {
		return nil
	}
	if reasons[0] == nil && reasons[1] == nil {
		for _, category := range categories {
			s.built[category+"/"] = s.where(lit)
		}
		return nil
	}
	var out []forwarded
	for _, reason := range reasons {
		if reason != nil {
			out = append(out, s.record(pkg, stack, reason, categories)...)
		}
	}
	return out
}

// record adds the refusals of the categories whose reason or limit is the
// expression.
func (s *refusalScan) record(pkg *refusalPkg, stack []*refusalFunc, expr ast.Expr, categories []string) []forwarded {
	v := s.resolve(pkg, stack, expr)
	switch {
	case v.copied:
		return nil
	case v.unknown || expr == nil:
		s.miss(stack, expr)
		return nil
	case v.fn != nil:
		return []forwarded{{v.fn, v.param, categories}}
	}
	for _, category := range categories {
		for _, text := range v.texts {
			s.built[category+"/"+text] = s.where(expr)
		}
	}
	return nil
}

// resolved is what an expression can be: texts, the parameter of a
// function, a field copied from another refusal, or unknown.
type resolved struct {
	texts   []string
	fn      *refusalFunc
	param   int
	copied  bool
	unknown bool
}

func (s *refusalScan) resolve(pkg *refusalPkg, stack []*refusalFunc, expr ast.Expr) resolved {
	switch e := expr.(type) {
	case nil:
		return resolved{}
	case *ast.ParenExpr:
		return s.resolve(pkg, stack, e.X)
	case *ast.BasicLit:
		if text, ok := stringLiteral(e); ok {
			return resolved{texts: []string{text}}
		}
	case *ast.SelectorExpr:
		x, ok := e.X.(*ast.Ident)
		if ok && slices.Contains([]string{"Code", "Reason", "Limit", "Name"}, e.Sel.Name) && s.refusalValue(pkg, stack, x) {
			return resolved{copied: true}
		}
	case *ast.Ident:
		if d := s.declOf(stack, e); d != nil {
			if d.param < 0 {
				return s.assigned(pkg, stack, d)
			}
			for _, fn := range stack {
				if fn.node() == d.fn && fn.name != "" {
					return resolved{fn: fn, param: d.param}
				}
			}
			return resolved{unknown: true}
		}
		if text, ok := pkg.consts[e.Name]; ok {
			return resolved{texts: []string{text}}
		}
	}
	return resolved{unknown: true}
}

// assigned resolves a local variable through every assignment to it in
// its scope. A variable nothing assigns, or assigns in a way the scan does
// not read, is unknown.
func (s *refusalScan) assigned(pkg *refusalPkg, stack []*refusalFunc, d *declaration) resolved {
	var v resolved
	found := false
	add := func(lhs []ast.Expr, rhs []ast.Expr, plain bool) {
		for i, target := range lhs {
			id, ok := target.(*ast.Ident)
			if !ok || s.declOf(stack, id) != d {
				continue
			}
			found = true
			if !plain || len(lhs) != len(rhs) {
				v.unknown = true
				continue
			}
			got := s.resolve(pkg, stack, rhs[i])
			v.unknown = v.unknown || got.unknown || got.copied || got.fn != nil
			v.texts = append(v.texts, got.texts...)
		}
	}
	ast.Inspect(d.scope, func(n ast.Node) bool {
		switch st := n.(type) {
		case *ast.AssignStmt:
			add(st.Lhs, st.Rhs, st.Tok == token.ASSIGN || st.Tok == token.DEFINE)
		case *ast.ValueSpec:
			lhs := make([]ast.Expr, len(st.Names))
			for i, id := range st.Names {
				lhs[i] = id
			}
			if len(st.Values) > 0 {
				add(lhs, st.Values, true)
			}
		case *ast.RangeStmt:
			add([]ast.Expr{st.Key, st.Value}, nil, false)
		}
		return true
	})
	if !found {
		return resolved{unknown: true}
	}
	return v
}

func (s *refusalScan) where(n ast.Node) string {
	pos := s.fset.Position(n.Pos())
	return fmt.Sprintf("%s:%d", strings.TrimPrefix(filepath.ToSlash(pos.Filename), "../"), pos.Line)
}

// miss notes a place whose refusal the scan cannot read.
func (s *refusalScan) miss(stack []*refusalFunc, n ast.Node) {
	name := "package"
	if len(stack) > 0 && stack[0].decl != nil {
		name = stack[0].decl.Name.Name
	}
	file := ""
	if n != nil {
		file, _, _ = strings.Cut(s.where(n), ":")
	} else if len(stack) > 0 {
		file, _, _ = strings.Cut(s.where(stack[0].node()), ":")
	}
	s.unseen[file+":"+name] = "a value the scan cannot follow"
}

// sheetRefusalSources reads the sources that build sheet refusals.
func sheetRefusalSources(t *testing.T) map[string]string {
	t.Helper()
	sources := map[string]string{}
	for _, pattern := range sheetRefusalFiles {
		found, err := filepath.Glob(pattern)
		if err != nil || len(found) == 0 {
			t.Fatalf("no source matches %s: %v", pattern, err)
		}
		for _, path := range found {
			if strings.HasSuffix(path, "_test.go") {
				continue
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			sources[path] = string(data)
		}
	}
	return sources
}

// TestSheetRefusalScan checks the scan itself: that it follows a reason
// through the forms the sheet code uses and reports what it cannot follow,
// and that the sources hold nothing it would pass over without a report.
func TestSheetRefusalScan(t *testing.T) {
	sample := scanSheetRefusals(t, map[string]string{
		"sheet/a.go": `package sheet
const busy = "writer_busy"
type WriteError struct{ Code, Reason, Limit string; Retry bool }
func unavailable(reason string) *WriteError { return &WriteError{Retry: true, Reason: reason, Code: "unavailable"} }
func capacity(limit string, max int) *WriteError { return &WriteError{Code: "sheet_capacity", Limit: limit} }
func grow(rows bool) *WriteError {
	limit, name := 702, "columns"
	if rows {
		limit, name = 100000, "rows"
	}
	return capacity(name, limit)
}
func count(limit string) *WriteError { return capacity(limit, 1) }
func tabs() *WriteError { return count("tabs") }
func (w *WriteError) again() *WriteError { return &WriteError{Code: w.Code, Reason: w.Reason} }
func run(err error, list []string) []*WriteError {
	refuse := func(reason string) *WriteError { return unavailable(reason) }
	var why string
	why = "stopped"
	return []*WriteError{refuse(why), refuse(busy), unavailable(err.Error()), {Code: "conflict"},
		&WriteError{Code: "invalid", Reason: "tab"}, &WriteError{Code: "conflict", Reason: list[0]}}
}
type config struct{ Reason string }
func configured(cfg config) *WriteError { return &WriteError{Code: "unavailable", Reason: cfg.Reason} }
func keep(err error) *WriteError {
	var problem *WriteError
	return &WriteError{Code: problem.Code, Reason: problem.Reason}
}
var table = map[string][]*WriteError{"a": {{Code: "unavailable", Reason: "listed"}}}
func shadowed(err *WriteError, cfg config) *WriteError {
	if cfg.Reason != "" {
		err := cfg
		return &WriteError{Code: "unavailable", Reason: err.Reason}
	}
	return err
}
func nested(ok bool, cfg config) *WriteError {
	if ok {
		var problem *WriteError
		_ = problem
	}
	problem := cfg
	return &WriteError{Code: "unavailable", Reason: problem.Reason}
}
var outside config
func elsewhere(ok bool) *WriteError {
	if ok {
		var outside *WriteError
		_ = outside
	}
	return &WriteError{Code: "unavailable", Reason: outside.Reason}
}
func inner(cfg config) *WriteError {
	{
		var cfg *WriteError
		return &WriteError{Code: cfg.Code, Reason: cfg.Reason}
	}
}
func local(reason string) *WriteError {
	{
		reason := "inner"
		return &WriteError{Code: "unavailable", Reason: reason}
	}
}
func callLocal() *WriteError { return local("outer") }
func later(ok bool) *WriteError {
	why := "first"
	if ok {
		why := "other"
		_ = why
	}
	return &WriteError{Code: "conflict", Reason: why}
}
`,
		"core/b.go": `package core
import "example/sheet"
func measure() error { return &sheet.WriteError{Code: "unavailable", Reason: "measurement"} }
func late() error { return sheet.Unavailable("late") }
func batch() []*sheet.WriteError { return []*sheet.WriteError{{Code: "unavailable", Reason: "batched"}} }
`,
		"sheet/c.go": `package sheet
func Unavailable(reason string) *WriteError { return unavailable(reason) }
`,
	})
	// The elided literals of a list, a nested list and a map of lists are
	// read, the one without a reason as its category alone.
	//
	// A name stands for its innermost declaration in scope: the local
	// reason of local, not its parameter, so "outer" is no reason, and the
	// why of later that is in scope, not the one of the block.
	wantBuilt := []string{"conflict/", "conflict/first", "sheet_capacity/columns", "sheet_capacity/rows", "sheet_capacity/tabs", "unavailable/batched",
		"unavailable/inner", "unavailable/late", "unavailable/listed", "unavailable/measurement", "unavailable/stopped", "unavailable/writer_busy"}
	if got := slices.Sorted(maps.Keys(sample.built)); !slices.Equal(got, wantBuilt) {
		t.Errorf("the scan reads %v out of the sample, want %v", got, wantBuilt)
	}
	if sample.elided != 3 {
		t.Errorf("the scan read %d literals without a type of their own in the sample, want 3", sample.elided)
	}
	// The result of a call and an item of a list are not followed, and a
	// field of a value that is no refusal is no copy: cfg.Reason may be
	// any text. A field of a refusal declared by var is a copy. The
	// declaration in scope decides: a local err := cfg hides the refusal
	// parameter err (shadowed), and a refusal declared in another block
	// counts for nothing (nested, elsewhere), while one in the block of the
	// use hides a parameter that is no refusal (inner).
	wantUnseen := []string{"sheet/a.go:configured", "sheet/a.go:elsewhere", "sheet/a.go:nested", "sheet/a.go:run", "sheet/a.go:shadowed"}
	if got := slices.Sorted(maps.Keys(sample.unseen)); !slices.Equal(got, wantUnseen) {
		t.Errorf("the scan reports %v as places it cannot follow, want %v", got, wantUnseen)
	}
	// The text of a list of refusals names the type once, as the list
	// literal the scan counts.
	if written, _ := refusalCensus(map[string]string{"core/b.go": `package core
func batch() []*sheet.WriteError { return []*sheet.WriteError{{Code: "unavailable", Reason: "batched"}, {Code: "conflict"}} }
`}); written != 1 {
		t.Errorf("the census counts %d literals in one list of refusals, want 1", written)
	}

	sources := sheetRefusalSources(t)
	real := scanSheetRefusals(t, sources)
	written, assigned := refusalCensus(sources)
	for path, found := range assigned {
		// api/sheet.go sets the limit of a read query, which is no refusal.
		if !(strings.HasSuffix(path, "api/sheet.go") && slices.Equal(found, []string{".Limit = "})) {
			t.Errorf("%s sets a field of a refusal by assignment (%v), where the scan does not look: write it in the literal", path, found)
		}
	}
	if written != real.literals {
		t.Errorf("the sources write WriteError{ or LimitError{ %d times and the scan read %d literals: a literal has a form the scan does not see, such as one of a named list type, or a comment or a string holds that text", written, real.literals)
	}
	// No other source builds a sheet refusal.
	scanned := map[string]bool{}
	for path := range sources {
		scanned[filepath.Clean(path)] = true
	}
	for _, root := range []string{"../internal", "../cmd"} {
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || scanned[filepath.Clean(path)] {
				return err
			}
			data, err := os.ReadFile(path)
			if err == nil && strings.Contains(string(data), "sheet.WriteError{") {
				t.Errorf("%s builds a sheet refusal and is not among sheetRefusalFiles", path)
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

// refusalCensus counts by text the literals whose text names a refusal
// type, a list or a map of refusals included, and finds the assignments to
// a field of a refusal, by path. A literal names its type as the package of
// the type does, or with the package in front.
func refusalCensus(sources map[string]string) (written int, assigned map[string][]string) {
	own := map[string]*regexp.Regexp{
		"sheet":   regexp.MustCompile(`(?:^|[^.\w])WriteError\{|\bformula\.LimitError\{`),
		"formula": regexp.MustCompile(`(?:^|[^.\w])LimitError\{`),
	}
	other := regexp.MustCompile(`\b(?:sheet\.WriteError|formula\.LimitError)\{`)
	set := regexp.MustCompile(`\.(?:Code|Reason|Limit) = `)
	assigned = map[string][]string{}
	for path, src := range sources {
		typed := own[regexp.MustCompile(`(?m)^package (\w+)`).FindStringSubmatch(src)[1]]
		if typed == nil {
			typed = other
		}
		written += len(typed.FindAllString(src, -1))
		if found := set.FindAllString(src, -1); len(found) != 0 {
			assigned[path] = found
		}
	}
	return written, assigned
}

// TestSheetTopicCoversAgentRefusals keeps the guide's list of refusals in
// step with the code: every refusal the sheet code builds is either named
// by topic.sheet, so an agent that receives it knows what to do, or is one
// that no agent tool returns.
//
// The refusals come from scanSheetRefusals, which reads the source and
// does not run it. It finds a reason wherever it is written as a text and
// reaches the refusal through literals, constants, variables and
// parameters (refusalScan says how). What it cannot follow fails this test
// unless unseenSheetRefusals accounts for it, and TestSheetRefusalScan
// checks that the literals whose text names a refusal type are the ones it
// reads. Whether a listed refusal can reach an
// agent is a judgment recorded in sheetRefusals, not something the scan
// finds, and the invalid refusals are not listed at all.
func TestSheetTopicCoversAgentRefusals(t *testing.T) {
	md, _ := Topic("sheet")
	_, refusals, ok := strings.Cut(md, "\n### Refusals\n")
	if !ok {
		t.Fatal("topic.sheet has no Refusals section")
	}
	scan := scanSheetRefusals(t, sheetRefusalSources(t))
	built := scan.built
	for key, path := range built {
		if sheetRefusals[key] == "" {
			t.Errorf("%s builds the refusal %s: decide whether an agent tool can return it, list it in sheetRefusals, and name it in topic.sheet if one can", path, key)
		}
	}
	for place := range scan.unseen {
		if unseenSheetRefusals[place] == "" {
			t.Errorf("%s builds a refusal whose category, reason or limit the scan cannot follow: write it as a text, a constant, a variable or a parameter, or say in unseenSheetRefusals why no reason is missed", place)
		}
	}
	for place := range unseenSheetRefusals {
		if scan.unseen[place] == "" {
			t.Errorf("%s builds no refusal the scan cannot follow any more: remove it from unseenSheetRefusals", place)
		}
	}
	for key, who := range sheetRefusals {
		if built[key] == "" {
			t.Errorf("no sheet code builds the refusal %s any more: remove it from sheetRefusals and from topic.sheet", key)
			continue
		}
		category, reason, _ := strings.Cut(key, "/")
		named := strings.Contains(md, "`"+reason+"`")
		switch {
		case who == forAgents && !named:
			t.Errorf("topic.sheet does not name %s of %s, which an agent tool returns", reason, category)
		case who == forAgents && !strings.Contains(refusals, "`"+category+"`"):
			t.Errorf("the Refusals section of topic.sheet does not explain %s", category)
		case who != forAgents && reason != "" && named:
			t.Errorf("topic.sheet names %s of %s, and %s", reason, category, who)
		}
	}
	// The section says what to do for each category an agent tool returns,
	// and the generic refusal texts exist for them.
	for _, category := range []string{"invalid", "not_sheet", "not_found", "sheet_capacity", "conflict", "unavailable"} {
		if !strings.Contains(refusals, "- `"+category+"`") {
			t.Errorf("the Refusals section of topic.sheet has no item for %s", category)
		}
		if !slices.Contains(RefusalCategories(), category) {
			t.Errorf("refusal.%s does not exist", category)
		}
	}
	notFound, _ := Refusal("not_found")
	if !strings.Contains(notFound, "`get_sheet`") || !strings.Contains(notFound, "`tab`") {
		t.Error("refusal.not_found does not explain a sheet read that names a missing tab")
	}
	// The hint of an unavailable sheet call leads to refusal.unavailable,
	// which sends the agent on to the reasons in the topic.
	if unavailable, _ := Refusal("unavailable"); !strings.Contains(unavailable, "`topic.sheet`") {
		t.Error("refusal.unavailable does not point to the sheet reasons in topic.sheet")
	}
}

// An invalid call names what is wrong the way the guide says: reason is
// the field or the part of the call, and detail says what is wrong and
// counts the operation from 1 when one is at fault.
func TestSheetTopicInvalidRefusalNamesTheField(t *testing.T) {
	md, _ := Topic("sheet")
	_, refusals, _ := strings.Cut(md, "\n### Refusals\n")
	item := regexp.MustCompile("(?s)- `invalid`.*?\n- ").FindString(refusals)
	for _, want := range []string{"`reason` names the field or the part of the call", "`detail` says what is wrong", "counted from 1"} {
		if !strings.Contains(strings.Join(strings.Fields(item), " "), want) {
			t.Errorf("the invalid item of topic.sheet does not say %q: %s", want, item)
		}
	}
	for _, c := range []struct{ call, reason, detail, names string }{
		{`{"state":"s","ops":[{"op":"add_tab","name":"A"},{"op":"write","tab":"A","at":"A1","rows":[[1]],"colour":"red"}]}`, "", "", "colour"},
		{`{"state":"s","ops":[{"op":"paint","tab":"A"}]}`, "op", "operation 1 (paint)", ""},
		{`{"state":"s","ops":[{"op":"widths","tab":"A","cols":"B:D","width":5}]}`, "width", "operation 1 (widths)", "40"},
		{`{"state":"s","ops":[{"op":"write","tab":"A","at":"A1","rows":[[1]]},{"op":"sort","tab":"A","rows":"2:9","by":"7"}]}`, "by", "operation 2 (sort)", ""},
		{`{"state":"s","op_id":"not an id","ops":[]}`, "op_id", "", ""},
	} {
		_, err := sheet.DecodeUpdate([]byte(c.call))
		var refusal *sheet.WriteError
		if !errors.As(err, &refusal) || refusal.Code != "invalid" {
			t.Errorf("%s: %v, want an invalid refusal", c.call, err)
			continue
		}
		if refusal.Reason == "" || (c.reason != "" && refusal.Reason != c.reason) {
			t.Errorf("%s: reason %q, want the field %q", c.call, refusal.Reason, c.reason)
		}
		if !strings.Contains(refusal.Detail, c.detail) || !strings.Contains(refusal.Detail, c.names) {
			t.Errorf("%s: detail %q, want %q and %q in it", c.call, refusal.Detail, c.detail, c.names)
		}
	}
}

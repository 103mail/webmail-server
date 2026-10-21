package api

import (
	"bufio"
	"go/ast"
	"go/parser"
	"go/token"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// specDir holds the OpenAPI definition of everything Handler serves.
const specDir = "../../docs/openapi/v1"

// TestOpenAPICoversRoutes checks that docs/openapi/v1 and the router agree, in
// both directions: every method the code answers on is specified, and every
// specified operation is answered. The routes are read from the source rather
// than probed, because most handlers validate the id or the body before they
// look at the method, so a probe cannot tell a missing route from a bad request.
//
// An operation is identified by method and path with the trailing id dropped,
// since the router dispatches on /api/v1/{group}/{entity} and treats whatever
// follows as the id. ControlHandler is not covered: it is a test-only listener
// and never part of Handler (ADR 20).
func TestOpenAPICoversRoutes(t *testing.T) {
	inCode := routeOperations(t)
	inSpec := specOperations(t)

	for _, op := range sortedKeys(inCode) {
		if _, ok := inSpec[op]; !ok {
			t.Errorf("%s is served (%s) but not in %s", op, inCode[op], specDir)
		}
	}
	for _, op := range sortedKeys(inSpec) {
		if _, ok := inCode[op]; !ok {
			t.Errorf("%s is in %s but not served", op, inSpec[op])
		}
	}
	if len(inCode) == 0 || len(inSpec) == 0 {
		t.Fatalf("found %d routes and %d specified operations; the parsing is broken", len(inCode), len(inSpec))
	}
}

// routeOperations walks the mux in Handler and the handlers it registers, and
// returns each "METHOD /path" with the function it was found in.
func routeOperations(t *testing.T) map[string]string {
	t.Helper()
	funcs := parseFuncs(t, ".")
	handler, ok := funcs["Handler"]
	if !ok {
		t.Fatal("Server.Handler not found")
	}

	ops := map[string]string{}
	add := func(method, path, where string) {
		ops[method+" "+strings.TrimSuffix(path, "/")] = where
	}
	ast.Inspect(handler.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || !isSelector(call.Fun, "mux", "HandleFunc") || len(call.Args) != 2 {
			return true
		}
		pattern := stringValue(t, call.Args[0])
		switch h := call.Args[1].(type) {
		case *ast.CallExpr:
			// s.route(s.handleX): a group dispatching on entity and method.
			if !isSelector(h.Fun, "s", "route") || len(h.Args) != 1 {
				t.Fatalf("unrecognised handler for %s", pattern)
			}
			name := methodName(t, h.Args[0])
			for entity, methods := range entityMethods(t, funcs[name]) {
				for _, m := range methods {
					add(m, pattern+entity, name)
				}
			}
		default:
			// s.handleX: a plain handler that checks the method itself.
			name := methodName(t, h)
			methods := httpMethods(funcs[name].Body)
			if len(methods) == 0 {
				// It answers on any method; GET is the one to document.
				methods = []string{"GET"}
			}
			for _, m := range methods {
				add(m, pattern, name)
			}
		}
		return false
	})
	return ops
}

// entityMethods returns, per entity, the methods a group handler answers. A
// group either switches on req.Entity or, with a single entity, refuses every
// other one with `req.Entity != "name"`.
func entityMethods(t *testing.T, fn *ast.FuncDecl) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.SwitchStmt:
			if !isSelector(n.Tag, "req", "Entity") {
				return true
			}
			for _, stmt := range n.Body.List {
				clause := stmt.(*ast.CaseClause)
				for _, value := range clause.List {
					entity := stringValue(t, value)
					out[entity] = httpMethods(&ast.BlockStmt{List: clause.Body})
				}
			}
			return false
		case *ast.BinaryExpr:
			if n.Op == token.NEQ && isSelector(n.X, "req", "Entity") {
				out[stringValue(t, n.Y)] = httpMethods(fn.Body)
			}
		}
		return true
	})
	if len(out) == 0 {
		t.Fatalf("%s does not dispatch on req.Entity", fn.Name.Name)
	}
	return out
}

// httpMethods collects every http.MethodX a block mentions.
func httpMethods(block *ast.BlockStmt) []string {
	seen := map[string]bool{}
	ast.Inspect(block, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if ok && isIdent(sel.X, "http") && strings.HasPrefix(sel.Sel.Name, "Method") {
			method := strings.ToUpper(strings.TrimPrefix(sel.Sel.Name, "Method"))
			if method != "OPTIONS" {
				seen[method] = true
			}
		}
		return true
	})
	return sortedKeys(seen)
}

// parseFuncs returns the non-test functions and methods of the package in dir.
func parseFuncs(t *testing.T, dir string) map[string]*ast.FuncDecl {
	t.Helper()
	funcs := map[string]*ast.FuncDecl{}
	for _, f := range parseDir(t, dir) {
		for _, decl := range f.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok {
				funcs[fn.Name.Name] = fn
			}
		}
	}
	return funcs
}

func parseDir(t *testing.T, dir string) []*ast.File {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	var files []*ast.File
	fset := token.NewFileSet()
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	return files
}

// stringValue resolves a string literal, or a constant of a sibling internal
// package such as federation.InboundPath.
func stringValue(t *testing.T, expr ast.Expr) string {
	t.Helper()
	switch e := expr.(type) {
	case *ast.BasicLit:
		if v, err := strconv.Unquote(e.Value); err == nil {
			return v
		}
	case *ast.SelectorExpr:
		if pkg, ok := e.X.(*ast.Ident); ok {
			for _, f := range parseDir(t, filepath.Join("..", pkg.Name)) {
				if obj := f.Scope.Lookup(e.Sel.Name); obj != nil && obj.Kind == ast.Con {
					spec := obj.Decl.(*ast.ValueSpec)
					for i, name := range spec.Names {
						if name.Name == e.Sel.Name {
							return stringValue(t, spec.Values[i])
						}
					}
				}
			}
		}
	}
	t.Fatalf("cannot resolve %T to a string", expr)
	return ""
}

// methodName returns X for the expression s.X.
func methodName(t *testing.T, expr ast.Expr) string {
	t.Helper()
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok || !isIdent(sel.X, "s") {
		t.Fatalf("expected s.handleX, got %T", expr)
	}
	return sel.Sel.Name
}

func isSelector(expr ast.Expr, x, sel string) bool {
	s, ok := expr.(*ast.SelectorExpr)
	return ok && isIdent(s.X, x) && s.Sel.Name == sel
}

func isIdent(expr ast.Expr, name string) bool {
	id, ok := expr.(*ast.Ident)
	return ok && id.Name == name
}

var (
	specServer    = regexp.MustCompile(`^  - url: ['"]?([^'"\s]+)`)
	specPath      = regexp.MustCompile(`^  ['"]?(/[^'":]*)['"]?:\s*$`)
	specOperation = regexp.MustCompile(`^    (get|put|post|delete|patch|head|options):`)
	specParam     = regexp.MustCompile(`/\{[^}]+\}$`)
)

// specOperations reads every "METHOD /path" out of the spec files, with the
// file it is in. The files are plain block YAML, so the few keys needed are
// found by indentation rather than with a YAML library.
func specOperations(t *testing.T) map[string]string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(specDir, "*.yaml"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no spec files in %s: %v", specDir, err)
	}
	ops := map[string]string{}
	for _, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		var section, base, current string
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			line := scanner.Text()
			if line != "" && line[0] != ' ' && line[0] != '#' {
				section = strings.TrimSuffix(strings.Fields(line)[0], ":")
				continue
			}
			switch section {
			case "servers":
				if m := specServer.FindStringSubmatch(line); m != nil && base == "" {
					u, err := url.Parse(m[1])
					if err != nil {
						t.Fatalf("%s: server url: %v", path, err)
					}
					base = strings.TrimSuffix(u.Path, "/")
				}
			case "paths":
				if m := specPath.FindStringSubmatch(line); m != nil {
					current = base + specParam.ReplaceAllString(m[1], "")
				} else if m := specOperation.FindStringSubmatch(line); m != nil {
					op := strings.ToUpper(m[1]) + " " + current
					if other, ok := ops[op]; ok {
						t.Errorf("%s is specified in both %s and %s", op, other, path)
					}
					ops[op] = filepath.Base(path)
				}
			}
		}
		f.Close()
		if err := scanner.Err(); err != nil {
			t.Fatal(err)
		}
	}
	return ops
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

package server_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"net/http"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// changeMethods are the methods that change permissions, by the package
// path of the Store type that declares them.
var changeMethods = map[string]map[string]bool{
	permissionsPath: {"Grant": true, "Revoke": true, "GrantReviewer": true, "RevokeReviewer": true, "GrantDefaults": true, "Bootstrap": true},
}

// allowedChangeFiles may call changeMethods or refer to operatorIdents: the
// operator CLI and the defining file. cmd/server/main.go also starts the
// HTTP server, but its handlers live in internal/server, and any call inside
// an HTTP handler is flagged in every file anyway.
var allowedChangeFiles = map[string]bool{
	"cmd/server/main.go":                  true,
	"internal/permissions/permissions.go": true,
}

// sqlOwners lists, per protected write, the only package that may contain
// it in a SQL string literal. Table names may be schema-qualified and
// quoted. SQL assembled by concatenation or at run time is out of scope.
var sqlOwners = []struct {
	re    *regexp.Regexp
	owner string
}{
	{sqlWrite(`(user_permissions|reviewer_assignments|permission_audit)`), "internal/permissions"},
}

// sqlTable matches a table name, optionally quoted and schema-qualified.
func sqlTable(name string) string {
	return `("?[a-z_][a-z0-9_$]*"?\s*\.\s*)?"?` + name + `"?(\s|\(|$|;)`
}

func sqlWrite(tables string) *regexp.Regexp {
	return regexp.MustCompile(`(insert\s+into|update(\s+only)?|delete\s+from(\s+only)?|truncate(\s+table)?(\s+only)?)\s+` + sqlTable(tables))
}

// TestNoHTTPPathChangesPermissions type-checks every non-test package of the
// module and fails if code outside the operator CLI and internal/permissions
// changes permissions: calls to the change methods of permissions.Store, any
// operator actor (operatorIdents), any such call or use inside an HTTP
// handler, or SQL literals that write the permission or audit tables
// directly. TestHTTPLayerDoesNotUseOperator separately covers every package
// the HTTP layer imports.
func TestNoHTTPPathChangesPermissions(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	var pkgs []scanPackage
	for _, line := range goListLines(t, root, "-f", "{{.ImportPath}}|{{.Dir}}|{{join .GoFiles \",\"}}", "./...") {
		parts := strings.SplitN(line, "|", 3)
		if len(parts) == 3 && parts[2] != "" {
			pkgs = append(pkgs, scanPackage{parts[0], parts[1], strings.Split(parts[2], ",")})
		}
	}
	fset := token.NewFileSet()
	imp := exportImporter(t, root, fset, "./...")
	checked := 0
	for _, pkg := range pkgs {
		var files []*ast.File
		for _, name := range pkg.files {
			f, err := parser.ParseFile(fset, filepath.Join(pkg.dir, name), nil, parser.SkipObjectResolution)
			if err != nil {
				t.Fatal(err)
			}
			files = append(files, f)
		}
		info := &types.Info{Uses: map[*ast.Ident]types.Object{}, Selections: map[*ast.SelectorExpr]*types.Selection{}}
		if _, err := (&types.Config{Importer: imp}).Check(pkg.path, fset, files, info); err != nil {
			t.Fatalf("type-check %s: %v", pkg.path, err)
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(pkg.path, modulePath), "/")
		for _, f := range files {
			checkFile(t, fset, root, rel, f, info)
			checked++
		}
	}
	if checked < 10 {
		t.Fatalf("only %d files checked", checked)
	}
}

func checkFile(t *testing.T, fset *token.FileSet, root, pkgRel string, f *ast.File, info *types.Info) {
	t.Helper()
	pos := func(p token.Pos) string {
		position := fset.Position(p)
		rel, _ := filepath.Rel(root, position.Filename)
		return fmt.Sprintf("%s:%d", filepath.ToSlash(rel), position.Line)
	}
	fileRel, _ := filepath.Rel(root, fset.Position(f.Pos()).Filename)
	fileRel = filepath.ToSlash(fileRel)

	// Source ranges of functions that take an http.ResponseWriter.
	var handlers [][2]token.Pos
	ast.Inspect(f, func(n ast.Node) bool {
		var ft *ast.FuncType
		switch fn := n.(type) {
		case *ast.FuncDecl:
			ft = fn.Type
		case *ast.FuncLit:
			ft = fn.Type
		}
		if ft != nil && ft.Params != nil {
			for _, field := range ft.Params.List {
				if sel, ok := field.Type.(*ast.SelectorExpr); ok && sel.Sel.Name == "ResponseWriter" {
					if obj := info.Uses[sel.Sel]; obj != nil && obj.Pkg() != nil && obj.Pkg().Path() == "net/http" {
						handlers = append(handlers, [2]token.Pos{n.Pos(), n.End()})
					}
				}
			}
		}
		return true
	})
	inHandler := func(p token.Pos) bool {
		for _, h := range handlers {
			if p >= h[0] && p < h[1] {
				return true
			}
		}
		return false
	}

	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.SelectorExpr:
			// Resolve the method object itself, so calls, method values,
			// method expressions ((*Store).Grant) and methods promoted
			// through embedding all count. Calls through an interface do
			// not resolve to the Store method and are out of scope.
			fn, ok := info.Uses[x.Sel].(*types.Func)
			if !ok {
				return true
			}
			recv := fn.Type().(*types.Signature).Recv()
			if recv == nil {
				return true
			}
			rt := recv.Type()
			if p, ok := rt.(*types.Pointer); ok {
				rt = p.Elem()
			}
			if named, ok := rt.(*types.Named); ok && named.Obj().Name() == "Store" && named.Obj().Pkg() != nil &&
				changeMethods[named.Obj().Pkg().Path()][fn.Name()] {
				if !allowedChangeFiles[fileRel] || inHandler(x.Pos()) {
					t.Errorf("%s: %s.%s changes permissions outside the operator CLI", pos(x.Pos()), named.Obj().Name(), fn.Name())
				}
			}
		case *ast.Ident:
			if obj := info.Uses[x]; obj != nil && operatorIdents[obj.Name()] && obj.Pkg() != nil &&
				obj.Pkg().Path() == permissionsPath {
				if !allowedChangeFiles[fileRel] || inHandler(x.Pos()) {
					t.Errorf("%s: permissions.%s used outside the operator CLI", pos(x.Pos()), obj.Name())
				}
			}
		case *ast.BasicLit:
			if x.Kind != token.STRING {
				return true
			}
			s, err := strconv.Unquote(x.Value)
			if err != nil {
				return true
			}
			s = strings.ToLower(s)
			for _, o := range sqlOwners {
				if o.re.MatchString(s) && pkgRel != o.owner {
					t.Errorf("%s: SQL write %q belongs in %s", pos(x.Pos()), o.re.FindString(s), o.owner)
				}
			}
		}
		return true
	})
}

func TestNoPermissionRoutes(t *testing.T) {
	e := newEnv(t)
	token, csrf := e.mustLogin()
	for _, path := range []string{"/api/permissions", "/api/admin/permissions", "/api/users/1/permissions", "/api/admin/users/1"} {
		for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
			w := e.do(m, path, map[string]string{"permission": "administrator"}, withCookie(token), withCSRF(csrf))
			if w.Code != http.StatusNotFound {
				t.Errorf("%s %s = %d, want 404", m, path, w.Code)
			}
		}
	}
}

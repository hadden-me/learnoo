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

const accountsPath = modulePath + "/internal/accounts"

// changeMethods are the methods that change permissions or account status,
// by the package path of the Store type that declares them.
var changeMethods = map[string]map[string]bool{
	permissionsPath: {"Grant": true, "Revoke": true, "GrantReviewer": true, "RevokeReviewer": true, "GrantDefaults": true, "Bootstrap": true},
	accountsPath:    {"Disable": true, "Enable": true},
}

// changeRule is what one file may do with one package's changeMethods.
type changeRule struct {
	// methods lists the allowed change methods; nil allows all of them.
	methods map[string]bool
	// http allows calls inside HTTP handlers (functions taking an
	// http.ResponseWriter). Such a file may only call the methods directly
	// (no method values or expressions), and every call's actor argument
	// must be a direct call of permissions.UserActor, so a change is always
	// attributed to a signed-in user, whom the stores recheck as an active
	// administrator inside their transaction.
	http bool
}

// adminAPIFile is the administration API (LMS-B10, docs/DECISIONS.md
// "Administration UI (LMS-B10)"), the only HTTP code that changes
// permissions or account status.
const adminAPIFile = "internal/studioapi/admin.go"

// allowedChangeFiles lists, per declaring package, the files that may call
// that package's changeMethods: the operator CLI and the defining file, and
// the administration API for the per-user changes only (never
// GrantDefaults or Bootstrap). cmd/server/main.go also starts the HTTP
// server, but its handlers live in internal/server, and any call inside an
// HTTP handler of a file without http is flagged. internal/accounts calls
// only the permissions guards, never the permissions change methods.
var allowedChangeFiles = map[string]map[string]changeRule{
	permissionsPath: {
		"cmd/server/main.go":                  {},
		"internal/permissions/permissions.go": {},
		adminAPIFile:                          {methods: map[string]bool{"Grant": true, "Revoke": true, "GrantReviewer": true, "RevokeReviewer": true}, http: true},
	},
	accountsPath: {
		"cmd/server/main.go": {},
		adminAPIFile:         {methods: map[string]bool{"Disable": true, "Enable": true}, http: true},
	},
}

// operatorFiles may refer to operatorIdents, outside HTTP handlers. The
// administration API may not: its changes are always a user's.
var operatorFiles = map[string]bool{
	"cmd/server/main.go":                  true,
	"internal/permissions/permissions.go": true,
}

// sqlOwners lists, per protected write, the only package that may contain
// it in a SQL string literal; an empty owner means no package may (yet).
// Table names may be schema-qualified and quoted. SQL assembled by
// concatenation or at run time is out of scope.
var sqlOwners = []struct {
	re    *regexp.Regexp
	owner string
}{
	{sqlWrite(`(user_permissions|reviewer_assignments|permission_audit)`), "internal/permissions"},
	{sqlWrite(`account_status_audit`), "internal/accounts"},
	// Disabling an account can remove an active administrator, so it must
	// go through the last-administrator guard in accounts.Store. Tests set
	// disabled_at directly.
	{regexp.MustCompile(`update\s+(only\s+)?` + sqlTable(`users`) + `[^;]*\bdisabled_at\b`), "internal/accounts"},
}

// sqlViolations reports the protected SQL writes in the string literal s
// (already unquoted) that package pkgRel may not contain.
func sqlViolations(pkgRel, s string) []string {
	s = strings.ToLower(s)
	var out []string
	for _, o := range sqlOwners {
		if !o.re.MatchString(s) || pkgRel == o.owner {
			continue
		}
		if o.owner == "" {
			out = append(out, fmt.Sprintf("SQL write %q is not allowed in any package", o.re.FindString(s)))
		} else {
			out = append(out, fmt.Sprintf("SQL write %q belongs in %s", o.re.FindString(s), o.owner))
		}
	}
	return out
}

// sqlTable matches a table name, optionally quoted and schema-qualified.
func sqlTable(name string) string {
	return `("?[a-z_][a-z0-9_$]*"?\s*\.\s*)?"?` + name + `"?(\s|\(|$|;)`
}

func sqlWrite(tables string) *regexp.Regexp {
	return regexp.MustCompile(`(insert\s+into|update(\s+only)?|delete\s+from(\s+only)?|truncate(\s+table)?(\s+only)?)\s+` + sqlTable(tables))
}

// TestNoHTTPPathChangesPermissions type-checks every non-test package of the
// module and fails if code outside the operator CLI, the owning packages and
// the administration API changes permissions or account status: calls to
// the change methods of permissions.Store or accounts.Store outside
// allowedChangeFiles, any such call inside an HTTP handler outside the
// administration API, any call there that is not direct or whose actor is
// not permissions.UserActor(…), any operator actor (operatorIdents) outside
// operatorFiles or inside a handler, or SQL literals that write the
// permission, account status or audit tables directly.
// TestHTTPLayerDoesNotUseOperator separately covers every package the HTTP
// layer imports.
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
			for _, v := range checkFile(fset, root, rel, f, info) {
				t.Error(v)
			}
			checked++
		}
	}
	if checked < 10 {
		t.Fatalf("only %d files checked", checked)
	}
}

// checkFile returns the violations in f, a file of package pkgRel.
func checkFile(fset *token.FileSet, root, pkgRel string, f *ast.File, info *types.Info) []string {
	var out []string
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

	// Calls by their selector, to check how an allowed HTTP file calls.
	calls := map[*ast.SelectorExpr]*ast.CallExpr{}
	ast.Inspect(f, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if sel, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr); ok {
				calls[sel] = call
			}
		}
		return true
	})
	isUserActorCall := func(e ast.Expr) bool {
		call, ok := ast.Unparen(e).(*ast.CallExpr)
		if !ok {
			return false
		}
		var id *ast.Ident
		switch fun := ast.Unparen(call.Fun).(type) {
		case *ast.SelectorExpr:
			id = fun.Sel
		case *ast.Ident:
			id = fun
		}
		obj, ok := info.Uses[id].(*types.Func)
		return ok && obj.Name() == "UserActor" && obj.Pkg() != nil && obj.Pkg().Path() == permissionsPath
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
			named, ok := rt.(*types.Named)
			if !ok || named.Obj().Name() != "Store" || named.Obj().Pkg() == nil || !changeMethods[named.Obj().Pkg().Path()][fn.Name()] {
				return true
			}
			rule, allowed := allowedChangeFiles[named.Obj().Pkg().Path()][fileRel]
			switch {
			case !allowed || (rule.methods != nil && !rule.methods[fn.Name()]):
				out = append(out, fmt.Sprintf("%s: %s.%s changes permissions or account status outside the operator CLI and the administration API", pos(x.Pos()), named.Obj().Name(), fn.Name()))
			case inHandler(x.Pos()) && !rule.http:
				out = append(out, fmt.Sprintf("%s: %s.%s called from an HTTP handler outside the administration API", pos(x.Pos()), named.Obj().Name(), fn.Name()))
			case rule.http:
				call := calls[x]
				if call == nil || len(call.Args) < 2 || !isUserActorCall(call.Args[1]) {
					out = append(out, fmt.Sprintf("%s: %s.%s in the administration API must be called directly with permissions.UserActor(…) as its actor", pos(x.Pos()), named.Obj().Name(), fn.Name()))
				}
			}
		case *ast.Ident:
			if obj := info.Uses[x]; obj != nil && operatorIdents[obj.Name()] && obj.Pkg() != nil &&
				obj.Pkg().Path() == permissionsPath {
				if !operatorFiles[fileRel] || inHandler(x.Pos()) {
					out = append(out, fmt.Sprintf("%s: permissions.%s used outside the operator CLI", pos(x.Pos()), obj.Name()))
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
			for _, v := range sqlViolations(pkgRel, s) {
				out = append(out, fmt.Sprintf("%s: %s", pos(x.Pos()), v))
			}
		}
		return true
	})
	return out
}

// The SQL rules flag protected writes in probe handlers and allow them only
// in their owner.
func TestSQLWriteScanDetectsProbes(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "probe.go", `package probe
import "net/http"
var db interface{ Exec(string, ...any) }
var h = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	db.Exec(`+"`UPDATE users SET disabled_at = now() WHERE id = $1`"+`, 1)
	db.Exec(`+"`update \"public\".\"users\" set display_name = 'x', disabled_at = NULL`"+`)
	db.Exec(`+"`DELETE FROM user_permissions WHERE user_id = $1`"+`, 1)
	db.Exec(`+"`INSERT INTO public.permission_audit (action) VALUES ('grant')`"+`)
	db.Exec(`+"`UPDATE users SET display_name = $1`"+`, "x")
	db.Exec(`+"`INSERT INTO account_status_audit (actor_id, target_user_id, action) VALUES (1, 2, 'disable')`"+`)
})`, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	var lits []string
	ast.Inspect(f, func(n ast.Node) bool {
		if x, ok := n.(*ast.BasicLit); ok && x.Kind == token.STRING && strings.HasPrefix(x.Value, "`") {
			s, _ := strconv.Unquote(x.Value)
			lits = append(lits, s)
		}
		return true
	})
	if len(lits) != 6 {
		t.Fatalf("probe has %d SQL literals, want 6", len(lits))
	}
	for i, s := range lits {
		got := len(sqlViolations("internal/server", s))
		if want := map[bool]int{true: 0, false: 1}[i == 4]; got != want {
			t.Errorf("%q in internal/server: %d violations, want %d", s, got, want)
		}
	}
	// Each owner may write its own tables and only those.
	for _, c := range []struct {
		pkg  string
		lit  int
		want int
	}{
		{"internal/permissions", 2, 0},
		{"internal/permissions", 3, 0},
		{"internal/permissions", 0, 1},
		{"internal/permissions", 5, 1},
		{"internal/accounts", 0, 0},
		{"internal/accounts", 1, 0},
		{"internal/accounts", 5, 0},
		{"internal/accounts", 2, 1},
		{"internal/accounts", 3, 1},
		{"internal/auth", 0, 1},
		{"cmd/server", 1, 1},
		{"cmd/server", 5, 1},
	} {
		if got := len(sqlViolations(c.pkg, lits[c.lit])); got != c.want {
			t.Errorf("%q in %s: %d violations, want %d", lits[c.lit], c.pkg, got, c.want)
		}
	}
}

// The method rules flag accounts.Store.Disable and Enable (calls, method
// values, method expressions and promoted methods) everywhere except the
// operator CLI outside HTTP handlers, and permissions change methods outside
// their allowed files. Probes are type-checked as if they were the named
// file of the module.
func TestChangeMethodScanDetectsProbes(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	imp := exportImporter(t, root, fset, "./...")
	const head = `package probe
import (
	"context"
	"net/http"
	"` + accountsPath + `"
	"` + permissionsPath + `"
)
var (
	ctx   = context.Background()
	store *accounts.Store
	perms *permissions.Store
	actor = permissions.UserActor(1)
)
type wrapped struct{ *accounts.Store }
var _ http.Handler
`
	probes := map[string]string{
		"disable call":       `var _, _ = store.Disable(ctx, actor, 2)`,
		"enable call":        `var _, _ = store.Enable(ctx, actor, 2)`,
		"method value":       `var f = store.Disable`,
		"method expression":  `var f = (*accounts.Store).Enable`,
		"promoted":           `var _, _ = wrapped{}.Disable(ctx, actor, 2)`,
		"permissions change": `var _, _ = perms.Grant(ctx, actor, 2, permissions.Author)`,
		"in handler": `var h = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	_, _ = store.Disable(r.Context(), actor, 2)
})`,
		// The administration API's shape: direct calls in a handler with
		// the signed-in user as actor.
		"handler user disable": `var h = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	_, _ = store.Disable(r.Context(), permissions.UserActor(1), 2)
})`,
		"handler user grant": `var h = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	_, _ = perms.GrantReviewer(r.Context(), (permissions.UserActor(1)), 2, 3)
})`,
		"handler grant defaults": `var h = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	_ = perms.GrantDefaults(r.Context(), permissions.UserActor(1), 2, permissions.Admin)
})`,
		"handler operator": `var h = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	op, _ := permissions.OperatorAs("x")
	_, _ = store.Disable(r.Context(), op, 2)
})`,
	}
	// allowed[file] lists the probes that are not violations when the probe
	// is the named file; every other probe must be flagged at least once
	// ("handler operator" twice outside the operator files: the operator
	// actor and its use as an actor).
	allowed := map[string]map[string]bool{
		"cmd/server/main.go": {"disable call": true, "enable call": true, "method value": true,
			"method expression": true, "promoted": true, "permissions change": true},
		"internal/accounts/accounts.go":       {},
		"internal/permissions/permissions.go": {"permissions change": true},
		"internal/server/server.go":           {},
		adminAPIFile:                          {"handler user disable": true, "handler user grant": true},
	}
	for file, ok := range allowed {
		for name, body := range probes {
			wantN := 1
			if ok[name] {
				wantN = 0
			}
			f, err := parser.ParseFile(fset, filepath.Join(root, file), head+body, parser.SkipObjectResolution)
			if err != nil {
				t.Fatal(err)
			}
			info := &types.Info{Uses: map[*ast.Ident]types.Object{}, Selections: map[*ast.SelectorExpr]*types.Selection{}}
			if _, err := (&types.Config{Importer: imp}).Check(modulePath+"/probe", fset, []*ast.File{f}, info); err != nil {
				t.Fatalf("%s as %s: type-check: %v", name, file, err)
			}
			pkgRel := filepath.ToSlash(filepath.Dir(file))
			got := checkFile(fset, root, pkgRel, f, info)
			if (wantN == 0 && len(got) != 0) || (wantN == 1 && len(got) == 0) {
				t.Errorf("%s as %s: %d violations, want %s: %v", name, file, len(got), map[int]string{0: "none", 1: "at least one"}[wantN], got)
			}
		}
	}
}

// Before LMS-B10 no route changed permissions or account status; the
// administration API is now the only one. Paths outside it stay unrouted,
// and every administration route refuses an account without administrator
// (here a student) with 403, for existing and missing accounts alike, before any
// body is read.
func TestNoPermissionRoutes(t *testing.T) {
	e := newEnv(t)
	token, csrf := e.mustLogin()
	for _, path := range []string{"/api/permissions", "/api/admin/permissions", "/api/users/1/permissions", "/api/users/1/disable", "/api/users/1/enable", "/api/admin/users/1/status", "/api/admin/users/1/permissions"} {
		for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
			w := e.do(m, path, map[string]string{"permission": "administrator"}, withCookie(token), withCSRF(csrf))
			if w.Code != http.StatusNotFound {
				t.Errorf("%s %s = %d, want 404", m, path, w.Code)
			}
		}
	}
	for _, path := range []string{"/api/admin/users/1/grant", "/api/admin/users/999999/grant", "/api/admin/users/1/revoke", "/api/admin/users/1/disable", "/api/admin/users/1/enable"} {
		w := e.do(http.MethodPost, path, map[string]string{"permission": "administrator"}, withCookie(token), withCSRF(csrf))
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), `"forbidden"`) {
			t.Errorf("student POST %s = %d %s, want 403 forbidden", path, w.Code, w.Body)
		}
	}
}

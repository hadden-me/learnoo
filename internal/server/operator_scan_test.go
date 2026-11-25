package server_test

import (
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	modulePath      = "github.com/hadden-me/learnoo"
	permissionsPath = modulePath + "/internal/permissions"
	serverPath      = modulePath + "/internal/server"
)

// Operator actors skip authorization; they are reserved for local operator
// tools (docs/DECISIONS.md "Operator trust boundary"), and
// permissions.OperatorAs is the only way to build one. These tests
// type-check the HTTP layer, internal/server and every package of this module
// it imports (except internal/permissions, which defines it), and fail if any
// of them refers to it.

// operatorIdents are the permissions identifiers that yield operator actors.
var operatorIdents = map[string]bool{"OperatorAs": true}

type scanPackage struct {
	path, dir string
	files     []string
}

func goListLines(t *testing.T, root string, args ...string) []string {
	t.Helper()
	cmd := exec.Command("go", append([]string{"list"}, args...)...)
	cmd.Dir = root
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list %v: %v", args, err)
	}
	return strings.Split(strings.TrimSpace(string(out)), "\n")
}

// exportImporter resolves imports from compiler export data of serverPath's
// dependencies, as built by go list -export.
func exportImporter(t *testing.T, root string, fset *token.FileSet) types.Importer {
	t.Helper()
	exports := map[string]string{}
	for _, line := range goListLines(t, root, "-deps", "-export", "-f", "{{.ImportPath}}={{.Export}}", serverPath, permissionsPath) {
		if path, file, ok := strings.Cut(line, "="); ok && file != "" {
			exports[path] = file
		}
	}
	return importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
		file, ok := exports[path]
		if !ok {
			return nil, fmt.Errorf("no export data for %s", path)
		}
		return os.Open(file)
	})
}

// operatorUses type-checks files as package path and returns the positions
// of identifiers that resolve to an operator constructor in permissions.
func operatorUses(fset *token.FileSet, imp types.Importer, path string, files []*ast.File) ([]string, error) {
	info := &types.Info{Uses: map[*ast.Ident]types.Object{}}
	if _, err := (&types.Config{Importer: imp}).Check(path, fset, files, info); err != nil {
		return nil, fmt.Errorf("type-check %s: %w", path, err)
	}
	var found []string
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok {
				if obj := info.Uses[id]; obj != nil && operatorIdents[obj.Name()] &&
					obj.Pkg() != nil && obj.Pkg().Path() == permissionsPath {
					found = append(found, fset.Position(id.Pos()).String())
				}
			}
			return true
		})
	}
	return found, nil
}

func TestHTTPLayerDoesNotUseOperator(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	var pkgs []scanPackage
	for _, line := range goListLines(t, root, "-deps", "-f", "{{.ImportPath}}|{{.Dir}}|{{join .GoFiles \",\"}}", serverPath) {
		parts := strings.SplitN(line, "|", 3)
		if len(parts) != 3 || parts[2] == "" || parts[0] == permissionsPath ||
			(parts[0] != modulePath && !strings.HasPrefix(parts[0], modulePath+"/")) {
			continue
		}
		pkgs = append(pkgs, scanPackage{parts[0], parts[1], strings.Split(parts[2], ",")})
	}
	fset := token.NewFileSet()
	imp := exportImporter(t, root, fset)
	scanned := map[string]bool{}
	for _, pkg := range pkgs {
		var files []*ast.File
		for _, name := range pkg.files {
			f, err := parser.ParseFile(fset, filepath.Join(pkg.dir, name), nil, parser.SkipObjectResolution)
			if err != nil {
				t.Fatal(err)
			}
			files = append(files, f)
		}
		uses, err := operatorUses(fset, imp, pkg.path, files)
		if err != nil {
			t.Fatal(err)
		}
		for _, u := range uses {
			t.Errorf("%s: operator actor used in the HTTP layer (%s)", u, pkg.path)
		}
		scanned[pkg.path] = true
	}
	for _, want := range []string{serverPath, modulePath + "/internal/auth"} {
		if !scanned[want] {
			t.Errorf("%s was not scanned; scanned %v", want, scanned)
		}
	}
}

// The detector itself finds direct, aliased and dot-imported references.
func TestOperatorScanDetectsUses(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	imp := exportImporter(t, root, fset)
	for name, src := range map[string]string{
		"direct": `package probe
import "` + permissionsPath + `"
var a, _ = permissions.OperatorAs("x")`,
		"alias": `package probe
import p "` + permissionsPath + `"
func f() any { a, _ := p.OperatorAs("x"); return a }`,
		"dot": `package probe
import . "` + permissionsPath + `"
var a, _ = OperatorAs("x")`,
		"value": `package probe
import p "` + permissionsPath + `"
var f = p.OperatorAs`,
	} {
		f, err := parser.ParseFile(fset, name+".go", src, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		uses, err := operatorUses(fset, imp, serverPath+"/probe", []*ast.File{f})
		if err != nil {
			t.Fatal(err)
		}
		if len(uses) != 1 {
			t.Errorf("%s: found %d uses, want 1", name, len(uses))
		}
	}
	// A package that only uses other identifiers is clean.
	f, err := parser.ParseFile(fset, "clean.go", `package probe
import "`+permissionsPath+`"
var a = permissions.UserActor(1)`, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	if uses, err := operatorUses(fset, imp, serverPath+"/probe", []*ast.File{f}); err != nil || len(uses) != 0 {
		t.Errorf("clean probe: uses=%v err=%v", uses, err)
	}
}

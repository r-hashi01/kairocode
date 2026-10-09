// Package invariants checks the invariants in AGENTS.md that can be read off
// the sources: deterministic workflow code (1), declared tool effects (3) and
// no secrets in the tree (6). The behavioural invariants (no double effects,
// resume) are checked by the crash tests.
package invariants

import (
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

const module = "github.com/r-hashi01/kairocode"

// workflowImports are the packages workflow code may import. Anything that
// reads the clock, randomness, the environment or does I/O is left out; the
// time package is allowed for its types only (see forbiddenCalls).
var workflowImports = map[string]bool{
	"context": true, "encoding/hex": true, "encoding/json": true, "errors": true,
	"fmt": true, "maps": true, "slices": true, "sort": true, "strconv": true,
	"strings": true, "time": true, "unicode": true, "unicode/utf8": true,
	"crypto/sha256":                     true,
	"github.com/r-hashi01/kairo/sdk/go": true, // the workflow context: Call, Parallel, WaitFor, Child
	module + "/internal/msg":            true,
	module + "/internal/permission":     true,
	module + "/internal/plan":           true,
	module + "/internal/tools":          true, // for the Def and Effect types
}

// workflowImportPrefixes are allowed with everything below them.
var workflowImportPrefixes = []string{module + "/internal/workflows"}

// forbiddenCalls are package-level functions of allowed packages that are not
// deterministic.
var forbiddenCalls = map[string][]string{
	"time": {"Now", "Since", "Until", "Sleep", "After", "AfterFunc", "NewTimer", "NewTicker", "Tick"},
}

// Workflows reports every place in the non-test Go files under dir (the
// workflow packages) that would make a workflow behave differently when it is
// replayed: imports outside the allow list, clock calls, goroutines, select,
// and ranging over a map (or over maps.Keys/Values/All) whose order Go
// randomises.
func Workflows(dir string) ([]string, error) {
	pkgs, err := packages(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, files := range pkgs {
		out = append(out, checkWorkflowPackage(files)...)
	}
	slices.Sort(out)
	return out, nil
}

type parsedFile struct {
	fset *token.FileSet
	path string
	file *ast.File
}

func packages(dir string) (map[string][]parsedFile, error) {
	pkgs := map[string][]parsedFile{}
	fset := token.NewFileSet()
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) && p == dir {
				return filepath.SkipDir
			}
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" && p != dir {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, p, nil, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		pkgs[filepath.Dir(p)] = append(pkgs[filepath.Dir(p)], parsedFile{fset, p, f})
		return nil
	})
	return pkgs, err
}

func checkWorkflowPackage(files []parsedFile) []string {
	var out []string
	report := func(f parsedFile, pos token.Pos, format string, args ...any) {
		out = append(out, fmt.Sprintf("%s: %s", f.fset.Position(pos), fmt.Sprintf(format, args...)))
	}

	// Types, for ranging over maps. Type errors in our own imports are
	// tolerated (a package that does not build fails go build anyway); what
	// matters is the type of each range expression we can resolve.
	info := &types.Info{Types: map[ast.Expr]types.TypeAndValue{}}
	conf := types.Config{Importer: importer.ForCompiler(files[0].fset, "source", nil), Error: func(error) {}}
	asts := make([]*ast.File, len(files))
	for i, f := range files {
		asts[i] = f.file
	}
	_, _ = conf.Check(files[0].file.Name.Name, files[0].fset, asts, info)

	for _, f := range files {
		names := map[string]string{} // local name -> import path
		for _, imp := range f.file.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			name := path[strings.LastIndex(path, "/")+1:]
			if imp.Name != nil {
				name = imp.Name.Name
			}
			names[name] = path
			if !allowedImport(path) {
				report(f, imp.Pos(), "workflow code may not import %q (clock, randomness, env or I/O belong in an action)", path)
			}
		}
		ast.Inspect(f.file, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.GoStmt:
				report(f, n.Pos(), "goroutine in workflow code; run calls at once with kairo.Parallel")
			case *ast.SelectStmt:
				report(f, n.Pos(), "select in workflow code; its choice is not replayed")
			case *ast.SelectorExpr:
				if id, ok := n.X.(*ast.Ident); ok {
					if fns, ok := forbiddenCalls[names[id.Name]]; ok && slices.Contains(fns, n.Sel.Name) {
						report(f, n.Pos(), "%s.%s in workflow code; use ctx.Now() (or ctx.Sleep)", id.Name, n.Sel.Name)
					}
				}
			case *ast.RangeStmt:
				if call, ok := n.X.(*ast.CallExpr); ok {
					if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
						if id, ok := sel.X.(*ast.Ident); ok && names[id.Name] == "maps" {
							report(f, n.Pos(), "ranging over maps.%s; its order is random. Use slices.Sorted(maps.Keys(m))", sel.Sel.Name)
						}
					}
				}
				if tv, ok := info.Types[n.X]; ok && tv.Type != nil {
					if _, isMap := tv.Type.Underlying().(*types.Map); isMap {
						report(f, n.Pos(), "ranging over a map; its order is random. Range over slices.Sorted(maps.Keys(m))")
					}
				}
			}
			return true
		})
	}
	return out
}

func allowedImport(path string) bool {
	if workflowImports[path] {
		return true
	}
	for _, p := range workflowImportPrefixes {
		if path == p || strings.HasPrefix(path, p+"/") {
			return true
		}
	}
	return false
}

// ToolEffects reports every tools.Def literal under dir that does not set
// Effect explicitly. The zero value means Real, which is safe, but a tool's
// effect is a decision the author has to make and write down.
func ToolEffects(dir string) ([]string, error) {
	pkgs, err := packages(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, files := range pkgs {
		for _, f := range files {
			ast.Inspect(f.file, func(n ast.Node) bool {
				lit, ok := n.(*ast.CompositeLit)
				if !ok || !isDefType(lit.Type) {
					return true
				}
				set := slices.ContainsFunc(lit.Elts, func(e ast.Expr) bool {
					kv, ok := e.(*ast.KeyValueExpr)
					if !ok {
						return false
					}
					id, ok := kv.Key.(*ast.Ident)
					return ok && id.Name == "Effect"
				})
				if !set {
					out = append(out, fmt.Sprintf("%s: tool Def without an explicit Effect (Read, Idempotent or Real)", f.fset.Position(lit.Pos())))
				}
				return true
			})
		}
	}
	slices.Sort(out)
	return out, nil
}

func isDefType(e ast.Expr) bool {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name == "Def"
	case *ast.SelectorExpr:
		id, ok := t.X.(*ast.Ident)
		return ok && id.Name == "tools" && t.Sel.Name == "Def"
	}
	return false
}

// secretPattern matches API keys of the providers KairoCode talks to.
var secretPattern = regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{20,}|sk-[A-Za-z0-9]{40,}`)

// Secrets reports files under the given roots that contain what looks like an
// API key.
func Secrets(roots ...string) ([]string, error) {
	var out []string
	for _, root := range roots {
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				if os.IsNotExist(err) && p == root {
					return filepath.SkipDir
				}
				return err
			}
			if d.IsDir() {
				return nil
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			if secretPattern.Match(b) {
				out = append(out, p)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// commandStarters are the files allowed to start a command (ADR 0005):
// the sandbox, and the Claude Code engine's start of claude (ADR 0007).
var commandStarters = []string{"internal/sandbox/", "internal/runtime/claude.go"}

// Commands reports every call of exec.Command or exec.CommandContext in the
// non-test Go files under root outside commandStarters: anything else that
// starts a command would run it outside the sandbox.
func Commands(root string) ([]string, error) {
	var out []string
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == "testdata" || d.Name() == ".git") {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		for _, ok := range commandStarters {
			if strings.HasPrefix(rel, ok) {
				return nil
			}
		}
		f, err := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		name := ""
		for _, imp := range f.Imports {
			if path, _ := strconv.Unquote(imp.Path.Value); path == "os/exec" {
				name = "exec"
				if imp.Name != nil {
					name = imp.Name.Name
				}
			}
		}
		if name == "" {
			return nil
		}
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == name && (sel.Sel.Name == "Command" || sel.Sel.Name == "CommandContext") {
				out = append(out, fmt.Sprintf("%s: exec.%s outside the sandbox (use internal/sandbox.Command)", fset.Position(sel.Pos()), sel.Sel.Name))
			}
			return true
		})
		return nil
	})
	return out, err
}

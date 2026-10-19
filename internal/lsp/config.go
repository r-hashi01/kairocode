// Package lsp gives a file's errors as language servers see them (ADR 0009):
// after a tool writes a file, the server for its language is told of the
// new content, and the errors it publishes for that file are added to the
// tool's result, in opencode's form. The servers run in the sandbox, live
// in the daemon across turns, and stop when not used for a while. A server
// that is not there, does not start, or does not answer in time adds
// nothing.
package lsp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Server is how to run a language server, and for which files.
type Server struct {
	ID         string   `json:"id"`
	Command    []string `json:"command"`
	Extensions []string `json:"extensions"`
	// Roots are the files that mark a project's root, nearest first in
	// each directory, looked for up from the file (not above the working
	// directory). None found: the working directory.
	Roots []string          `json:"roots,omitempty"`
	Env   map[string]string `json:"env,omitempty"`
	// Initialization is the server's initializationOptions.
	Initialization json.RawMessage `json:"initialization,omitempty"`
	// Caches are directories under the user's cache directory the server
	// may write (its index), besides the working and temporary ones.
	Caches []string `json:"caches,omitempty"`
}

// Builtin are the servers known without settings, as opencode names them.
var Builtin = []Server{
	{ID: "gopls", Command: []string{"gopls"}, Extensions: []string{".go"},
		Roots: []string{"go.work", "go.mod"}, Caches: []string{"gopls", "go-build"}},
	{ID: "typescript", Command: []string{"typescript-language-server", "--stdio"},
		Extensions: []string{".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs", ".mts", ".cts"},
		Roots:      []string{"package-lock.json", "bun.lockb", "bun.lock", "pnpm-lock.yaml", "yarn.lock", "package.json"}},
	{ID: "pyright", Command: []string{"pyright-langserver", "--stdio"}, Extensions: []string{".py", ".pyi"},
		Roots: []string{"pyproject.toml", "setup.py", "setup.cfg", "requirements.txt", "Pipfile", "pyrightconfig.json"}},
	{ID: "rust", Command: []string{"rust-analyzer"}, Extensions: []string{".rs"}, Roots: []string{"Cargo.toml"}},
	{ID: "clangd", Command: []string{"clangd"}, Extensions: []string{".c", ".cc", ".cpp", ".cxx", ".c++", ".h", ".hh", ".hpp", ".hxx"},
		Roots: []string{"compile_commands.json", "compile_flags.txt", ".clangd", "CMakeLists.txt", "Makefile"}},
}

// Off is the environment variable that, set to "off", starts no server
// (the tests of packages that write files set it).
const Off = "KAIROCODE_LSP"

// override is a server's entry in kairocode.json's "lsp".
type override struct {
	Disabled       bool              `json:"disabled"`
	Command        []string          `json:"command"`
	Extensions     []string          `json:"extensions"`
	Env            map[string]string `json:"env"`
	Initialization json.RawMessage   `json:"initialization"`
}

// Servers are the servers for the working directory dir: the built-in ones
// as kairocode.json's "lsp" (the user's, then the project's) changes them.
// "lsp": false starts none; {"<id>": {"disabled": true}} not that one;
// {"<id>": {"command", "extensions", "env", "initialization"}} adds one or
// changes it (one not built in needs its extensions).
func Servers(dir string, files ...string) ([]Server, error) {
	if os.Getenv(Off) == "off" {
		return nil, nil
	}
	servers := slices.Clone(Builtin)
	for _, path := range files {
		b, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		var cfg struct {
			LSP json.RawMessage `json:"lsp"`
		}
		if err := json.Unmarshal(b, &cfg); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if len(cfg.LSP) == 0 || string(cfg.LSP) == "null" || string(bytes.TrimSpace(cfg.LSP)) == "true" {
			continue
		}
		if string(bytes.TrimSpace(cfg.LSP)) == "false" {
			return nil, nil
		}
		var entries map[string]override
		if err := json.Unmarshal(cfg.LSP, &entries); err != nil {
			return nil, fmt.Errorf("%s: lsp: want false or {\"<id>\": {...}}: %w", path, err)
		}
		for _, id := range slices.Sorted(func(yield func(string) bool) {
			for id := range entries {
				if !yield(id) {
					return
				}
			}
		}) {
			o := entries[id]
			i := slices.IndexFunc(servers, func(s Server) bool { return s.ID == id })
			if o.Disabled {
				if i >= 0 {
					servers = slices.Delete(servers, i, i+1)
				}
				continue
			}
			if i < 0 {
				if len(o.Command) == 0 || len(o.Extensions) == 0 {
					return nil, fmt.Errorf("%s: lsp: %s is not built in: it needs a command and extensions", path, id)
				}
				servers = append(servers, Server{ID: id})
				i = len(servers) - 1
			}
			s := &servers[i]
			if len(o.Command) > 0 {
				s.Command = o.Command
			}
			if len(o.Extensions) > 0 {
				s.Extensions = o.Extensions
			}
			if o.Env != nil {
				s.Env = o.Env
			}
			if len(o.Initialization) > 0 {
				s.Initialization = o.Initialization
			}
		}
	}
	return servers, nil
}

// For is the server for file, and the root it runs in: the nearest
// directory from file up to dir that has one of its Roots, else dir.
func For(servers []Server, dir, file string) (Server, string, bool) {
	ext := filepath.Ext(file)
	for _, s := range servers {
		if !slices.Contains(s.Extensions, ext) {
			continue
		}
		return s, root(s, dir, file), true
	}
	return Server{}, "", false
}

func root(s Server, dir, file string) string {
	for d := filepath.Dir(file); ; d = filepath.Dir(d) {
		if d != dir && !strings.HasPrefix(d, dir+string(filepath.Separator)) {
			return dir
		}
		for _, name := range s.Roots {
			if _, err := os.Stat(filepath.Join(d, name)); err == nil {
				return d
			}
		}
		if d == dir {
			return dir
		}
	}
}

// languages are the languageId of a file by its extension.
var languages = map[string]string{
	".go": "go", ".ts": "typescript", ".mts": "typescript", ".cts": "typescript", ".tsx": "typescriptreact",
	".js": "javascript", ".mjs": "javascript", ".cjs": "javascript", ".jsx": "javascriptreact",
	".py": "python", ".pyi": "python", ".rs": "rust", ".c": "c", ".h": "c",
	".cc": "cpp", ".cpp": "cpp", ".cxx": "cpp", ".c++": "cpp", ".hh": "cpp", ".hpp": "cpp", ".hxx": "cpp",
}

func language(file string) string {
	if l, ok := languages[filepath.Ext(file)]; ok {
		return l
	}
	return "plaintext"
}

// Package agents is the ACP agents a user can install from ACP's registry
// and run as a provider (ADR 0015): the registry, read and kept for an
// hour; what this machine can run; installing one, when the user chose
// to; and what is installed, and how to start it.
package agents

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
)

// RegistryURL is ACP's registry of agents.
const RegistryURL = "https://cdn.agentclientprotocol.com/registry/v1/latest/registry.json"

// CacheFor is how long a registry read is used before it is read again.
const CacheFor = time.Hour

// Package is an npx or uvx distribution: a package at a version.
type Package struct {
	Package string            `json:"package"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

// Binary is a binary distribution for one platform.
type Binary struct {
	Archive string            `json:"archive"`
	Cmd     string            `json:"cmd"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	SHA256  string            `json:"sha256"`
}

// Agent is an agent of the registry.
type Agent struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Version      string `json:"version"`
	Description  string `json:"description"`
	Distribution struct {
		Npx    *Package          `json:"npx,omitempty"`
		Uvx    *Package          `json:"uvx,omitempty"`
		Binary map[string]Binary `json:"binary,omitempty"`
	} `json:"distribution"`
}

// Installed is an agent installed here: how to start it.
type Installed struct {
	Version string            `json:"version"`
	Command []string          `json:"command"`
	Env     map[string]string `json:"env,omitempty"`
}

// Entry is an agent as /provider lists it.
type Entry struct {
	Agent
	// Installed: the version installed, if any.
	Installed string `json:"installed,omitempty"`
	// Usable: it can be installed and run here; else Reason says why not.
	Usable bool   `json:"usable"`
	Reason string `json:"reason,omitempty"`
	// Source is what installing it fetches: a package, or an archive's URL.
	Source string `json:"source,omitempty"`
}

// Manager keeps the agents of a data directory (Dir: <data>/agents).
type Manager struct {
	Dir      string
	Registry string // the registry's URL
	HTTP     *http.Client
	// LookPath finds a command (exec.LookPath; tests change it).
	LookPath func(string) (string, error)

	mu sync.Mutex // installing, and installed.json
}

// New is a manager of dir, reading the registry at url (RegistryURL for
// ACP's; "" reads none: only what is installed is there).
func New(dir, url string) *Manager {
	return &Manager{Dir: dir, Registry: url, HTTP: &http.Client{Timeout: 5 * time.Minute}, LookPath: exec.LookPath}
}

// Platform is this machine as the registry names it: darwin-aarch64.
func Platform() string {
	arch := map[string]string{"arm64": "aarch64", "amd64": "x86_64"}[runtime.GOARCH]
	if arch == "" {
		arch = runtime.GOARCH
	}
	return runtime.GOOS + "-" + arch
}

// Agents are the registry's agents, read at most once in CacheFor; when it
// cannot be read, the copy kept is used.
func (m *Manager) Agents(ctx context.Context) ([]Agent, error) {
	if m.Registry == "" {
		return nil, nil
	}
	cache := filepath.Join(m.Dir, "registry.json")
	if st, err := os.Stat(cache); err == nil && time.Since(st.ModTime()) < CacheFor {
		if as, err := readRegistry(cache); err == nil {
			return as, nil
		}
	}
	b, err := m.get(ctx, m.Registry, 32<<20)
	if err == nil {
		var reg struct {
			Agents []Agent `json:"agents"`
		}
		if err = json.Unmarshal(b, &reg); err == nil {
			if err := os.MkdirAll(m.Dir, 0o755); err == nil {
				_ = writeAtomic(cache, b)
			}
			return reg.Agents, nil
		}
	}
	if as, cerr := readRegistry(cache); cerr == nil {
		return as, nil
	}
	return nil, fmt.Errorf("the ACP registry: %w", err)
}

func readRegistry(path string) ([]Agent, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var reg struct {
		Agents []Agent `json:"agents"`
	}
	return reg.Agents, json.Unmarshal(b, &reg)
}

// List is the registry's agents, with whether each is installed and can
// run here.
func (m *Manager) List(ctx context.Context) ([]Entry, error) {
	as, err := m.Agents(ctx)
	if err != nil {
		return nil, err
	}
	inst, err := m.installed()
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(as))
	for _, a := range as {
		e := Entry{Agent: a, Installed: inst[a.ID].Version}
		e.Source, e.Reason = m.source(a)
		e.Usable = e.Reason == ""
		out = append(out, e)
	}
	return out, nil
}

// source is what installing a fetches, or why it cannot be installed here.
func (m *Manager) source(a Agent) (string, string) {
	d := a.Distribution
	if b, ok := d.Binary[Platform()]; ok {
		if !archiveKind(b.Archive) {
			return b.Archive, "its archive is of a kind KairoCode does not open"
		}
		return b.Archive, ""
	}
	if d.Npx != nil {
		if _, err := m.LookPath("npm"); err != nil {
			return d.Npx.Package, "needs npm (Node.js)"
		}
		return d.Npx.Package, ""
	}
	if d.Uvx != nil {
		if _, err := m.LookPath("uv"); err != nil {
			return d.Uvx.Package, "needs uv"
		}
		return d.Uvx.Package, ""
	}
	return "", "no build for " + Platform()
}

func archiveKind(url string) bool {
	for _, s := range []string{".tar.gz", ".tgz", ".tar.bz2", ".zip"} {
		if strings.HasSuffix(url, s) {
			return true
		}
	}
	return false
}

// Command is how to start installed agent id; ok false if it is not.
func (m *Manager) Command(id string) (Installed, bool) {
	inst, err := m.installed()
	if err != nil {
		return Installed{}, false
	}
	i, ok := inst[id]
	return i, ok
}

func (m *Manager) installed() (map[string]Installed, error) {
	b, err := os.ReadFile(filepath.Join(m.Dir, "installed.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]Installed{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string]Installed{}
	return out, json.Unmarshal(b, &out)
}

// ErrUnknown: the registry has no such agent.
var ErrUnknown = errors.New("no such agent in the ACP registry")

// ErrChanged: the registry's version of the agent is not the one the user
// was shown.
var ErrChanged = errors.New("the registry has another version now")

// Install installs agent id's version of the registry here: the user chose
// to (ADR 0015), seeing version ("" for whatever the registry has). A
// binary is checked against its sha256 before anything of it is kept.
func (m *Manager) Install(ctx context.Context, id, version string) (Installed, error) {
	as, err := m.Agents(ctx)
	if err != nil {
		return Installed{}, err
	}
	i := slices.IndexFunc(as, func(a Agent) bool { return a.ID == id })
	if i < 0 {
		return Installed{}, fmt.Errorf("%w: %s", ErrUnknown, id)
	}
	a := as[i]
	if version != "" && version != a.Version {
		return Installed{}, fmt.Errorf("%w: %s %s, not %s; choose it again", ErrChanged, a.Name, a.Version, version)
	}
	if _, why := m.source(a); why != "" {
		return Installed{}, fmt.Errorf("%s cannot be installed here: %s", a.Name, why)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	// Into a directory of its own, new for each install, where it stays:
	// an installer may write its paths into what it installs, and an agent
	// installed before may be running. What a failed install leaves is
	// removed.
	if err := os.MkdirAll(filepath.Join(m.Dir, safeName(a.ID)), 0o755); err != nil {
		return Installed{}, err
	}
	dir, err := os.MkdirTemp(filepath.Join(m.Dir, safeName(a.ID)), safeName(a.Version)+"-")
	if err != nil {
		return Installed{}, err
	}
	tmp := dir
	done := false
	defer func() {
		if !done {
			os.RemoveAll(dir)
		}
	}()
	var cmd []string
	var env map[string]string
	d := a.Distribution
	switch b, ok := d.Binary[Platform()]; {
	case ok:
		if err := m.installBinary(ctx, b, tmp); err != nil {
			return Installed{}, err
		}
		rel := path.Clean(strings.TrimPrefix(b.Cmd, "./"))
		if !filepath.IsLocal(filepath.FromSlash(rel)) {
			return Installed{}, fmt.Errorf("%s: its command %q is outside what it installs", a.Name, b.Cmd)
		}
		cmd = append([]string{filepath.Join(dir, filepath.FromSlash(rel))}, b.Args...)
		env = b.Env
	case d.Npx != nil:
		bin, err := installNpm(ctx, d.Npx.Package, tmp)
		if err != nil {
			return Installed{}, err
		}
		cmd = append([]string{filepath.Join(dir, "node_modules", ".bin", bin)}, d.Npx.Args...)
		env = d.Npx.Env
	case d.Uvx != nil:
		bin, err := installUv(ctx, d.Uvx.Package, tmp)
		if err != nil {
			return Installed{}, err
		}
		cmd = append([]string{filepath.Join(dir, "bin", bin)}, d.Uvx.Args...)
		env = d.Uvx.Env
	}
	in := Installed{Version: a.Version, Command: cmd, Env: env}
	inst, err := m.installed()
	if err != nil {
		return Installed{}, err
	}
	inst[a.ID] = in
	b, err := json.MarshalIndent(inst, "", "  ")
	if err != nil {
		return Installed{}, err
	}
	if err := writeAtomic(filepath.Join(m.Dir, "installed.json"), b); err != nil {
		return Installed{}, err
	}
	done = true // recorded: it stays
	return in, nil
}

// writeAtomic writes b to name whole: a reader sees the old file or the
// new one.
func writeAtomic(name string, b []byte) error {
	f, err := os.CreateTemp(filepath.Dir(name), "."+filepath.Base(name)+"-")
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), name)
	}
	if err != nil {
		os.Remove(f.Name())
	}
	return err
}

// safeName is s as one path element.
func safeName(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == 0 {
			return '_'
		}
		return r
	}, s)
	if s == "" || s == "." || s == ".." {
		return "_"
	}
	return s
}

func (m *Manager) get(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := m.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%s: more than %d bytes", url, limit)
	}
	return b, nil
}

// installBinary fetches b's archive, checks it, and opens it into dir.
func (m *Manager) installBinary(ctx context.Context, b Binary, dir string) error {
	data, err := m.get(ctx, b.Archive, 1<<30)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); !strings.EqualFold(got, b.SHA256) {
		return fmt.Errorf("%s: its sha256 is %s, the registry says %s: not installed", b.Archive, got, b.SHA256)
	}
	root, err := os.OpenRoot(dir) // nothing of the archive lands outside dir
	if err != nil {
		return err
	}
	defer root.Close()
	switch {
	case strings.HasSuffix(b.Archive, ".zip"):
		return unzip(root, data)
	case strings.HasSuffix(b.Archive, ".tar.bz2"):
		return untar(root, bzip2.NewReader(bytes.NewReader(data)))
	default:
		z, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return err
		}
		return untar(root, z)
	}
}

func untar(root *os.Root, r io.Reader) error {
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := path.Clean(strings.TrimPrefix(h.Name, "./"))
		if name == "." {
			continue
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(name, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := writeFile(root, name, tr, fs.FileMode(h.Mode)&0o755|0o644); err != nil {
				return err
			}
		}
	}
}

func unzip(root *os.Root, data []byte) error {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return err
	}
	for _, f := range zr.File {
		name := path.Clean(strings.TrimPrefix(f.Name, "./"))
		if f.FileInfo().IsDir() {
			if err := root.MkdirAll(name, 0o755); err != nil {
				return err
			}
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		err = writeFile(root, name, rc, f.Mode().Perm()&0o755|0o644)
		rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func writeFile(root *os.Root, name string, r io.Reader, mode fs.FileMode) error {
	if d := path.Dir(name); d != "." {
		if err := root.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	f, err := root.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// env is a command's environment while installing: this process's, less
// what looks like a secret (invariant 6).
func env(extra ...string) []string {
	var out []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		u := strings.ToUpper(name)
		if strings.Contains(u, "KEY") || strings.Contains(u, "TOKEN") || strings.Contains(u, "SECRET") || strings.Contains(u, "PASSWORD") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, extra...)
}

func run(ctx context.Context, dir string, extra []string, argv ...string) error {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir, cmd.Env = dir, env(extra...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w: %s", strings.Join(argv, " "), err, tail(string(out), 400))
	}
	return nil
}

func tail(s string, n int) string {
	if len(s) > n {
		return "…" + s[len(s)-n:]
	}
	return s
}

// installNpm installs pkg (name@version) into dir, and returns the command
// it gives (its bin).
func installNpm(ctx context.Context, pkg, dir string) (string, error) {
	if err := run(ctx, dir, nil, "npm", "install", "--prefix", dir, "--no-fund", "--no-audit", pkg); err != nil {
		return "", err
	}
	name := pkg
	if i := strings.LastIndex(pkg, "@"); i > 0 {
		name = pkg[:i]
	}
	b, err := os.ReadFile(filepath.Join(dir, "node_modules", filepath.FromSlash(name), "package.json"))
	if err != nil {
		return "", err
	}
	var p struct {
		Bin json.RawMessage `json:"bin"`
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return "", err
	}
	var one string
	if json.Unmarshal(p.Bin, &one) == nil && one != "" {
		return plainBin(path.Base(name))
	}
	var bins map[string]string
	if err := json.Unmarshal(p.Bin, &bins); err != nil || len(bins) == 0 {
		return "", fmt.Errorf("%s gives no command", pkg)
	}
	if _, ok := bins[path.Base(name)]; ok {
		return plainBin(path.Base(name))
	}
	keys := make([]string, 0, len(bins))
	for k := range bins {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return plainBin(keys[0])
}

// plainBin is name if it is a command's plain name (one element, in .bin).
func plainBin(name string) (string, error) {
	if !filepath.IsLocal(name) || strings.ContainsAny(name, `/\`) {
		return "", fmt.Errorf("the command %q is not a plain name", name)
	}
	return name, nil
}

// installUv installs pkg (name==version) as a uv tool in dir, and returns
// the command it gives.
func installUv(ctx context.Context, pkg, dir string) (string, error) {
	bin := filepath.Join(dir, "bin")
	if err := run(ctx, dir, []string{"UV_TOOL_DIR=" + filepath.Join(dir, "tools"), "UV_TOOL_BIN_DIR=" + bin},
		"uv", "tool", "install", pkg); err != nil {
		return "", err
	}
	es, err := os.ReadDir(bin)
	if err != nil || len(es) == 0 {
		return "", fmt.Errorf("%s gives no command", pkg)
	}
	return es[0].Name(), nil
}

package runtime

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// Plugins' credentials (ADR 0025): kept in the system's key store (macOS's
// keychain through security, Linux's Secret Service through secret-tool),
// else in a file only the user can read. A value is read when a plugin's
// server is started or reached, and never recorded (invariant 6).

// SecretStore keeps credentials by key ("<plugin>/<name>").
type SecretStore interface {
	Get(key string) (string, bool, error)
	Set(key, value string) error
	Delete(key string) error
	// Where says where the values are kept, to tell the user.
	Where() string
}

// secretService is the name values are kept under in the key store.
const secretService = "kairocode"

// Secrets is this machine's store: KAIROCODE_SECRETS=file (or no key store
// here) keeps them in credentials.json of the user's config directory.
func Secrets() SecretStore {
	if os.Getenv("KAIROCODE_SECRETS") != "file" {
		switch {
		case runtime.GOOS == "darwin" && onPath("security"):
			return keychain{}
		case runtime.GOOS == "linux" && onPath("secret-tool"):
			return secretTool{}
		}
	}
	return &fileSecrets{path: filepath.Join(userConfigDir(), "credentials.json")}
}

func onPath(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// keychain is macOS's keychain, through security(1).
type keychain struct{}

func (keychain) Where() string { return "the macOS keychain" }

func (keychain) Get(key string) (string, bool, error) {
	out, err := exec.Command("security", "find-generic-password", "-s", secretService, "-a", key, "-w").Output()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return "", false, nil // not there
	}
	if err != nil {
		return "", false, err
	}
	return strings.TrimSuffix(string(out), "\n"), true, nil
}

func (keychain) Set(key, value string) error {
	// -U updates one that is there; the value goes as an argument, which
	// security takes without echoing it.
	return exec.Command("security", "add-generic-password", "-U", "-s", secretService, "-a", key, "-w", value).Run()
}

func (keychain) Delete(key string) error {
	err := exec.Command("security", "delete-generic-password", "-s", secretService, "-a", key).Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return nil // not there
	}
	return err
}

// secretTool is the Secret Service, through secret-tool(1).
type secretTool struct{}

func (secretTool) Where() string { return "the Secret Service" }

func (secretTool) Get(key string) (string, bool, error) {
	out, err := exec.Command("secret-tool", "lookup", "service", secretService, "account", key).Output()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return strings.TrimSuffix(string(out), "\n"), true, nil
}

func (secretTool) Set(key, value string) error {
	cmd := exec.Command("secret-tool", "store", "--label", "kairocode "+key, "service", secretService, "account", key)
	cmd.Stdin = strings.NewReader(value) // not an argument: not in ps
	return cmd.Run()
}

func (secretTool) Delete(key string) error {
	err := exec.Command("secret-tool", "clear", "service", secretService, "account", key).Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return nil
	}
	return err
}

// fileSecrets keeps values in a JSON file of mode 0600.
type fileSecrets struct {
	mu   sync.Mutex
	path string
}

func (f *fileSecrets) Where() string {
	return f.path + " (no key store here: a file only you can read)"
}

func (f *fileSecrets) read() (map[string]string, error) {
	m := map[string]string{}
	b, err := os.ReadFile(f.path)
	if errors.Is(err, fs.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	return m, json.Unmarshal(b, &m)
}

func (f *fileSecrets) write(m map[string]string) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(f.path), 0o700); err != nil {
		return err
	}
	return writeJSONBytes(f.path, b)
}

func (f *fileSecrets) Get(key string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, err := f.read()
	if err != nil {
		return "", false, err
	}
	v, ok := m[key]
	return v, ok, nil
}

func (f *fileSecrets) Set(key, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, err := f.read()
	if err != nil {
		return err
	}
	m[key] = value
	return f.write(m)
}

func (f *fileSecrets) Delete(key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, err := f.read()
	if err != nil {
		return err
	}
	delete(m, key)
	return f.write(m)
}

// writeJSONBytes replaces path with b, mode 0600, all at once.
func writeJSONBytes(path string, b []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".credentials-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(bytes.TrimSpace(b), '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

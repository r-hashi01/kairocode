package atomicfile

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// Written whole, with its mode; writers at once leave one of theirs whole,
// and no temporary file.
func TestWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.json")
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := Write(path, []byte(fmt.Sprintf("writer %02d\n", i)), 0o600); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	b, err := os.ReadFile(path)
	if err != nil || len(b) != len("writer 00\n") {
		t.Fatalf("%q %v", b, err)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", st.Mode())
	}
	if es, _ := os.ReadDir(dir); len(es) != 1 {
		t.Errorf("%d files left", len(es))
	}
}

package sandbox

import (
	"encoding/binary"
	"io"
	"testing"

	seccomp "github.com/elastic/go-seccomp-bpf"
	"github.com/elastic/go-seccomp-bpf/arch"
	"golang.org/x/net/bpf"
)

// The filter, run on the call's data as the kernel hands it: socket with
// AF_UNIX is refused however its upper bits are set, other families and
// calls pass, and every call of another architecture is refused.
func TestSeccompFilter(t *testing.T) {
	f, err := seccompFilter(true)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	raw := make([]bpf.RawInstruction, len(b)/8)
	for i := range raw {
		raw[i] = bpf.RawInstruction{
			Op: binary.NativeEndian.Uint16(b[8*i:]),
			Jt: b[8*i+2], Jf: b[8*i+3],
			K: binary.NativeEndian.Uint32(b[8*i+4:]),
		}
	}
	insts, ok := bpf.Disassemble(raw)
	if !ok {
		t.Fatal("the filter does not disassemble")
	}
	vm, err := bpf.NewVM(insts)
	if err != nil {
		t.Fatal(err)
	}
	info, err := arch.GetInfo("")
	if err != nil {
		t.Fatal(err)
	}
	call := func(a uint32, name string, arg0 uint64) uint32 {
		data := make([]byte, 64)
		nr, ok := info.SyscallNames[name]
		if !ok {
			t.Fatalf("no %s", name)
		}
		binary.NativeEndian.PutUint32(data[0:], uint32(nr))
		binary.NativeEndian.PutUint32(data[4:], a)
		binary.NativeEndian.PutUint64(data[16:], arg0)
		// The VM reads big-endian words, as the kernel does natively: swap.
		for i := 0; i+4 <= len(data); i += 4 {
			w := binary.NativeEndian.Uint32(data[i:])
			binary.BigEndian.PutUint32(data[i:], w)
		}
		out, err := vm.Run(data)
		if err != nil {
			t.Fatal(err)
		}
		return uint32(out)
	}
	allow := uint32(seccomp.ActionAllow)
	native := uint32(info.ID)
	for _, c := range []struct {
		arch  uint32
		name  string
		arg0  uint64
		allow bool
	}{
		{native, "socket", 1, false},
		{native, "socket", 1<<32 | 1, false},
		{native, "socket", 2, true},
		{native, "socketpair", 1, true},
		{native, "read", 0, true},
		{native, "ptrace", 0, false},
		{native ^ 1, "read", 0, false},
	} {
		if got := call(c.arch, c.name, c.arg0) == allow; got != c.allow {
			t.Errorf("%s(%#x) on arch %#x: allowed %v, want %v", c.name, c.arg0, c.arch, got, c.allow)
		}
	}
}

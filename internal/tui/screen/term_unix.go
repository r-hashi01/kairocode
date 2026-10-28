//go:build darwin || linux

package screen

import (
	"errors"
	"os"
	"syscall"
	"unsafe"
)

// The terminal's settings the UI changes (Bubble Tea, run without its
// renderer, changes none: ADR 0033): raw input, and what it is asked to
// report. Restore puts them back.

// Modes asked of the terminal while the UI runs, as Bubble Tea's renderer
// asks them: bracketed paste, focus, modifyOtherKeys and the kitty keyboard
// protocol (Shift+Enter told apart from Enter).
const (
	modesOn  = "\x1b[?2004h\x1b[?1004h\x1b[>4;2m\x1b[>1u"
	modesOff = "\x1b[<1u\x1b[>4m\x1b[?1004l\x1b[?2004l\x1b[?25h"
)

// Terminal is the terminal the UI takes: its input in raw mode.
type Terminal struct {
	in, out    *os.File
	saved, raw *syscall.Termios
}

func ioctl(fd uintptr, req uint, arg unsafe.Pointer) error {
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, uintptr(req), uintptr(arg)); e != 0 {
		return e
	}
	return nil
}

// Take puts in in raw mode and asks the terminal for the modes; Restore
// undoes it.
func Take(in, out *os.File) (*Terminal, error) {
	var saved syscall.Termios
	if err := ioctl(in.Fd(), ioctlGetTermios, unsafe.Pointer(&saved)); err != nil {
		return nil, errors.New("screen: not a terminal")
	}
	raw := saved
	// As cfmakeraw (and x/term's MakeRaw).
	raw.Iflag &^= syscall.IGNBRK | syscall.BRKINT | syscall.PARMRK | syscall.ISTRIP | syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IXON
	raw.Oflag &^= syscall.OPOST
	raw.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	raw.Cflag &^= syscall.CSIZE | syscall.PARENB
	raw.Cflag |= syscall.CS8
	raw.Cc[syscall.VMIN] = 1
	raw.Cc[syscall.VTIME] = 0
	if err := ioctl(in.Fd(), ioctlSetTermios, unsafe.Pointer(&raw)); err != nil {
		return nil, err
	}
	_, _ = out.WriteString(modesOn)
	return &Terminal{in: in, out: out, saved: &saved, raw: &raw}, nil
}

// Retake takes the terminal again, as Take did, after Restore.
func (t *Terminal) Retake() {
	_ = ioctl(t.in.Fd(), ioctlSetTermios, unsafe.Pointer(t.raw))
	_, _ = t.out.WriteString(modesOn)
}

// Restore puts the terminal back as it was.
func (t *Terminal) Restore() error {
	_, _ = t.out.WriteString(modesOff)
	return ioctl(t.in.Fd(), ioctlSetTermios, unsafe.Pointer(t.saved))
}

// Size is the terminal's size in cells.
func Size(out *os.File) (w, h int, err error) {
	var ws struct{ Row, Col, X, Y uint16 }
	if err := ioctl(out.Fd(), syscall.TIOCGWINSZ, unsafe.Pointer(&ws)); err != nil {
		return 0, 0, err
	}
	return int(ws.Col), int(ws.Row), nil
}

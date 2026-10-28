//go:build !darwin && !linux

package screen

import (
	"errors"
	"os"
)

// Terminal is the terminal the UI takes; only macOS and Linux are done.
type Terminal struct{}

var errNotDone = errors.New("screen: this system's terminal is not done (macOS and Linux are)")

func Take(in, out *os.File) (*Terminal, error) { return nil, errNotDone }
func (t *Terminal) Retake()                    {}
func (t *Terminal) Restore() error             { return nil }
func Size(out *os.File) (w, h int, err error)  { return 0, 0, errNotDone }

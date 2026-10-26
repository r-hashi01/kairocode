//go:build !darwin && !linux

package cli

import (
	"errors"
	"os"
)

// noEcho cannot hide what is typed here: the credential is read as typed.
func noEcho(*os.File) (func(), error) { return nil, errors.New("not supported here") }

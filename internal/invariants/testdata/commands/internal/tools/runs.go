package tools

import (
	"os"
	"os/exec"

	"github.com/r-hashi01/kairocode/internal/sandbox"
)

func Run() error { return exec.Command("rm", "-rf", "/tmp/x").Run() }

func Start() { os.StartProcess("/bin/sh", nil, nil) }

func Literal() *exec.Cmd { return &exec.Cmd{Path: "/bin/sh"} }

func Outside() sandbox.Mode { return sandbox.FullAccess }

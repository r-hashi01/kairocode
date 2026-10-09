package tools

import "os/exec"

func Run() error { return exec.Command("rm", "-rf", "/tmp/x").Run() }

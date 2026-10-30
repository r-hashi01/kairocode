package cli

import (
	"fmt"
	"io"
	"runtime"
	"runtime/debug"
	"strings"
)

// Version is the release, set when the binary is built for a release:
//
//	go build -ldflags "-X github.com/r-hashi01/kairocode/internal/cli.Version=v0.1.0"
//
// Without it, the version comes from Go's build information (the module's
// version under go install, or the commit of a build in the repository).
var Version string

// versionLine is what kairocode version prints, on one line.
func versionLine(info *debug.BuildInfo, ok bool) string {
	version, commit, dirty := Version, "", false
	if ok {
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				commit = s.Value
			case "vcs.modified":
				dirty = s.Value == "true"
			}
		}
	}
	if len(commit) > 12 {
		commit = commit[:12]
	}
	if version == "" && ok {
		// A build in the repository stamps a tag (v0.1.0, +dirty when
		// changed) or a pseudo-version that ends with the commit: the
		// commit is shown on its own, so only a tag is a version here.
		v := strings.TrimSuffix(info.Main.Version, "+dirty")
		if v != "(devel)" && (commit == "" || !strings.HasSuffix(v, commit)) {
			version = v
		}
	}
	if version == "" {
		version = "dev"
	}
	if commit != "" && dirty {
		commit += "+dirty"
	}
	line := "kairocode " + version
	if commit != "" {
		line += " (" + commit + ")"
	}
	return fmt.Sprintf("%s %s %s/%s", line, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}

func printVersion(w io.Writer) {
	info, ok := debug.ReadBuildInfo()
	fmt.Fprintln(w, versionLine(info, ok))
}

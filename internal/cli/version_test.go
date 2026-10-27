package cli

import (
	"bytes"
	"context"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
)

func TestVersionLine(t *testing.T) {
	tail := " " + runtime.Version() + " " + runtime.GOOS + "/" + runtime.GOARCH
	commit := debug.BuildSetting{Key: "vcs.revision", Value: "0123456789abcdef0123"}
	dirty := debug.BuildSetting{Key: "vcs.modified", Value: "true"}
	cases := []struct {
		name, set string
		info      *debug.BuildInfo
		ok        bool
		want      string
	}{
		{name: "no build info", want: "kairocode dev"},
		{name: "repository build", ok: true,
			info: &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}, Settings: []debug.BuildSetting{commit}},
			want: "kairocode dev (0123456789ab)"},
		{name: "uncommitted changes", ok: true,
			info: &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}, Settings: []debug.BuildSetting{commit, dirty}},
			want: "kairocode dev (0123456789ab+dirty)"},
		{name: "pseudo-version in the repository", ok: true,
			info: &debug.BuildInfo{Main: debug.Module{Version: "v0.0.0-20261027065014-0123456789ab+dirty"}, Settings: []debug.BuildSetting{commit, dirty}},
			want: "kairocode dev (0123456789ab+dirty)"},
		{name: "tag in the repository", ok: true,
			info: &debug.BuildInfo{Main: debug.Module{Version: "v0.1.0+dirty"}, Settings: []debug.BuildSetting{commit, dirty}},
			want: "kairocode v0.1.0 (0123456789ab+dirty)"},
		{name: "go install of a commit", ok: true,
			info: &debug.BuildInfo{Main: debug.Module{Version: "v0.0.0-20261027065014-0123456789ab"}},
			want: "kairocode v0.0.0-20261027065014-0123456789ab"},
		{name: "go install", ok: true,
			info: &debug.BuildInfo{Main: debug.Module{Version: "v0.2.0"}},
			want: "kairocode v0.2.0"},
		{name: "release build", set: "v0.3.0", ok: true,
			info: &debug.BuildInfo{Main: debug.Module{Version: "v0.2.0"}, Settings: []debug.BuildSetting{commit}},
			want: "kairocode v0.3.0 (0123456789ab)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			old := Version
			Version = c.set
			defer func() { Version = old }()
			if got := versionLine(c.info, c.ok); got != c.want+tail {
				t.Errorf("versionLine = %q, want %q", got, c.want+tail)
			}
		})
	}
}

func TestVersionCommand(t *testing.T) {
	for _, arg := range []string{"version", "-version", "--version"} {
		var out, errOut bytes.Buffer
		if code := Main(context.Background(), []string{arg}, &out, &errOut); code != 0 {
			t.Fatalf("%s: exit %d, stderr %q", arg, code, errOut.String())
		}
		if !strings.HasPrefix(out.String(), "kairocode ") || !strings.HasSuffix(out.String(), runtime.GOOS+"/"+runtime.GOARCH+"\n") {
			t.Errorf("%s: printed %q", arg, out.String())
		}
	}
}

package buildinfo

import (
	"runtime/debug"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestResolve(t *testing.T) {
	tagged := &debug.BuildInfo{
		Main: debug.Module{Path: "github.com/giantswarm/cluster-manager", Version: "v0.5.0"},
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "4badca6a61657033b833aaacca25bccc7b8bdda7"},
			{Key: "vcs.time", Value: "2026-09-15T23:54:44Z"},
			{Key: "vcs.modified", Value: "false"},
		},
	}
	dirty := &debug.BuildInfo{
		Main:     debug.Module{Version: "(devel)"},
		Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "abcdef1234"}, {Key: "vcs.modified", Value: "true"}},
	}

	tests := map[string]struct {
		version, commit, date string
		bi                    *debug.BuildInfo
		want                  Info
	}{
		"tag at HEAD fills the ldflags defaults": {
			version: DevVersion, commit: UnknownCommit, date: UnknownDate, bi: tagged,
			want: Info{Version: "0.5.0", Commit: "4badca6", Date: "2026-09-15T23:54:44Z"},
		},
		"tag at HEAD in a modified tree: the version stays the release, the commit is marked": {
			version: DevVersion, commit: UnknownCommit, date: UnknownDate,
			bi: &debug.BuildInfo{
				Main:     debug.Module{Version: "v0.4.1+dirty"},
				Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "a306a963b69d8e687f75728ecd49c66291ce6d02"}, {Key: "vcs.modified", Value: "true"}},
			},
			want: Info{Version: "0.4.1", Commit: "a306a96-dirty", Date: "unknown"},
		},
		"ldflags values win": {
			version: "0.6.0", commit: "1234567", date: "2026-10-01T00:00:00Z", bi: tagged,
			want: Info{Version: "0.6.0", Commit: "1234567", Date: "2026-10-01T00:00:00Z"},
		},
		"untagged dirty checkout stays dev and marks the commit": {
			version: DevVersion, commit: UnknownCommit, date: UnknownDate, bi: dirty,
			want: Info{Version: "dev", Commit: "abcdef1-dirty", Date: "unknown"},
		},
		"no build info leaves everything as given": {
			version: DevVersion, commit: UnknownCommit, date: UnknownDate, bi: nil,
			want: Info{Version: "dev", Commit: "unknown", Date: "unknown"},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, Resolve(tc.version, tc.commit, tc.date, tc.bi))
		})
	}
}

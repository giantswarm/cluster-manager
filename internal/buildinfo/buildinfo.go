// Package buildinfo resolves the version cluster-manager reports: what the
// build stamped with -ldflags -X when it did, else what the Go toolchain
// recorded from version control — the tag at HEAD as the module version, the
// commit and its time — so a binary built from a tagged checkout names its
// release without a build flag.
package buildinfo

import (
	"runtime/debug"
	"strings"
)

// The ldflags defaults a build without -X leaves in place.
const (
	DevVersion    = "dev"
	UnknownCommit = "unknown"
	UnknownDate   = "unknown"
)

// Info is the resolved build identity.
type Info struct {
	Version string
	Commit  string
	Date    string
}

// Read resolves the build identity from the ldflags values and the running
// binary's build info.
func Read(version, commit, date string) Info {
	bi, _ := debug.ReadBuildInfo()
	return Resolve(version, commit, date, bi)
}

// Resolve keeps every ldflags value that was set and fills the ones left at
// their defaults from bi: the main module's version without its v (never
// the toolchain's "(devel)" placeholder), the short VCS revision with
// -dirty for uncommitted changes, the commit time.
func Resolve(version, commit, date string, bi *debug.BuildInfo) Info {
	out := Info{Version: version, Commit: commit, Date: date}
	if bi == nil {
		return out
	}
	if unset(out.Version, DevVersion) {
		if v := strings.TrimPrefix(bi.Main.Version, "v"); v != "" && v != "(devel)" {
			out.Version = v
		}
	}
	settings := map[string]string{}
	for _, s := range bi.Settings {
		settings[s.Key] = s.Value
	}
	if rev := settings["vcs.revision"]; unset(out.Commit, UnknownCommit) && rev != "" {
		out.Commit = shortRevision(rev)
		if settings["vcs.modified"] == "true" {
			out.Commit += "-dirty"
		}
	}
	if t := settings["vcs.time"]; unset(out.Date, UnknownDate) && t != "" {
		out.Date = t
	}
	return out
}

func unset(v, def string) bool { return v == "" || v == def }

// shortRevision abbreviates a commit hash the way `git rev-parse --short`
// does.
func shortRevision(rev string) string {
	if len(rev) > 7 {
		return rev[:7]
	}
	return rev
}

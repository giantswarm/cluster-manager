package main

import (
	"github.com/giantswarm/cluster-manager/cmd"
	"github.com/giantswarm/cluster-manager/internal/buildinfo"
)

// Set by the build via ldflags (-X main.version=...); what the build leaves
// unset is read from the binary's own build info (the tag at HEAD, the
// commit, its time).
var (
	version = buildinfo.DevVersion
	commit  = buildinfo.UnknownCommit
	date    = buildinfo.UnknownDate
)

func main() {
	info := buildinfo.Read(version, commit, date)
	cmd.SetVersion(info.Version)
	cmd.SetBuildInfo(info.Commit, info.Date)
	cmd.Execute()
}

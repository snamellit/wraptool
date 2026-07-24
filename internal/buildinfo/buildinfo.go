// Package buildinfo exposes wraptool's build identity so a running server can
// report exactly which binary it is. Release builds inject the version derived
// from their Git tag; local builds retain the development marker and append the
// VCS revision Go embeds at build time.
package buildinfo

import (
	"runtime"
	"runtime/debug"
	"time"
)

// version is set from the exact release tag with go build -ldflags -X. It must
// remain a variable for linker injection. Untagged builds identify themselves
// by revision instead of duplicating a release version in source.
var version = "devel"

// startedAt records process start so wraptool_info can report uptime.
var startedAt = time.Now()

// Info is wraptool's build identity plus this process's start time.
type Info struct {
	Version   string    `json:"version"`            // release SemVer, or devel+<short-sha>[-dirty]
	Revision  string    `json:"revision,omitempty"` // full VCS revision, "" if unavailable
	Modified  bool      `json:"modified"`           // working tree was dirty at build
	GoVersion string    `json:"go_version"`
	StartedAt time.Time `json:"started_at"`
}

// Get reads the linker-injected release version and Go's embedded VCS metadata.
// Development builds append the revision when available; release builds keep
// the exact SemVer while exposing the full commit separately in Revision.
func Get() Info {
	info := Info{Version: version, GoVersion: runtime.Version(), StartedAt: startedAt}
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return info
	}
	if bi.GoVersion != "" {
		info.GoVersion = bi.GoVersion
	}
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			info.Revision = s.Value
		case "vcs.modified":
			info.Modified = s.Value == "true"
		}
	}
	if info.Revision != "" && version == "devel" {
		short := info.Revision
		if len(short) > 12 {
			short = short[:12]
		}
		info.Version = version + "+" + short
		if info.Modified {
			info.Version += "-dirty"
		}
	}
	return info
}

// Version is the stamped version string, for MCP serverInfo/ClientInfo.
func Version() string { return Get().Version }

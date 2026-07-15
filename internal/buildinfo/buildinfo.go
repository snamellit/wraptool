// Package buildinfo exposes wraptool's build identity — a base version stamped
// with the VCS revision Go embeds at build time — so a running server can report
// exactly which binary it is. This is what makes "is the new binary actually
// running?" answerable instead of a schema-fingerprinting guessing game.
package buildinfo

import (
	"runtime"
	"runtime/debug"
	"time"
)

// BaseVersion is the human semantic version. Get appends the VCS revision at
// runtime, giving e.g. "0.1.0+ab2d99bce123" or "0.1.0+ab2d99bce123-dirty".
const BaseVersion = "0.1.0"

// startedAt records process start so wraptool_info can report uptime.
var startedAt = time.Now()

// Info is wraptool's build identity plus this process's start time.
type Info struct {
	Version   string    `json:"version"`            // BaseVersion + "+"<short-sha>[-dirty]
	Revision  string    `json:"revision,omitempty"` // full VCS revision, "" if unavailable
	Modified  bool      `json:"modified"`           // working tree was dirty at build
	GoVersion string    `json:"go_version"`
	StartedAt time.Time `json:"started_at"`
}

// Get reads the embedded build info. Works with Go's default -buildvcs — no
// ldflags or build-tooling change required. Falls back to just the base version
// when VCS info is unavailable (e.g. `go run`, or a build with -buildvcs=false).
func Get() Info {
	info := Info{Version: BaseVersion, GoVersion: runtime.Version(), StartedAt: startedAt}
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
	if info.Revision != "" {
		short := info.Revision
		if len(short) > 12 {
			short = short[:12]
		}
		info.Version = BaseVersion + "+" + short
		if info.Modified {
			info.Version += "-dirty"
		}
	}
	return info
}

// Version is the stamped version string, for MCP serverInfo/ClientInfo.
func Version() string { return Get().Version }

package version

import (
	"fmt"
	"runtime/debug"
)

const (
	defaultVersion = "dev"
	unknown        = "unknown"
)

// Info is the resolved build identity.
type Info struct {
	Version   string
	Commit    string
	BuildDate string
}

// Get resolves the build identity from ldflags values, falling back to the Go build info.
func Get() Info {
	bi, ok := debug.ReadBuildInfo()
	return resolve(Version, Commit, BuildDate, bi, ok)
}

func resolve(v, c, d string, bi *debug.BuildInfo, ok bool) Info {
	info := Info{Version: v, Commit: c, BuildDate: d}
	if !ok || bi == nil {
		return info
	}
	if info.Version == defaultVersion && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		info.Version = bi.Main.Version
	}
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			if info.Commit == unknown && s.Value != "" {
				info.Commit = s.Value
			}
		case "vcs.time":
			if info.BuildDate == unknown && s.Value != "" {
				info.BuildDate = s.Value
			}
		}
	}
	return info
}

// String returns the one-line form printed by --version.
func (i Info) String() string {
	return fmt.Sprintf("workload-knocker version=%s commit=%s build_date=%s", i.Version, i.Commit, i.BuildDate)
}

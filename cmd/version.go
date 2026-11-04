package cmd

import (
	"fmt"
	"runtime/debug"
	"strings"

	"github.com/spf13/cobra"
)

// Release builds set these with -ldflags (see .goreleaser.yaml). Any other
// build leaves them at their defaults, and versionInfo fills in what it can
// from the build info Go embeds.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print m8 version information",
	Run: func(cmd *cobra.Command, args []string) {
		info, _ := debug.ReadBuildInfo()
		v, c, d := versionInfo(info)
		fmt.Printf("m8 %s (commit: %s, built: %s)\n", v, c, d)
	},
}

// versionInfo returns the version, commit and build time to print. A value
// -ldflags set always wins. Otherwise:
//
//   - `go install github.com/ags-slc/m8@v0.4.0` records the module version,
//     v0.4.0. A module fetched from the proxy carries no VCS metadata, so
//     commit and date stay unknown.
//   - `go build` in a checkout records a pseudo-version derived from the
//     nearest tag, plus the commit, its time, and whether the tree was dirty.
//     The commit's time stands in for the build time, which Go does not
//     record, so that builds stay reproducible.
//
// The version is printed without its leading "v", as GoReleaser prints it.
func versionInfo(info *debug.BuildInfo) (v, c, d string) {
	v, c, d = version, commit, date
	if info == nil {
		return v, c, d
	}
	if v == "dev" && info.Main.Version != "" && info.Main.Version != "(devel)" {
		v = strings.TrimPrefix(info.Main.Version, "v")
	}
	var revision, modified string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.time":
			if d == "unknown" {
				d = s.Value
			}
		case "vcs.modified":
			modified = s.Value
		}
	}
	if c == "none" && revision != "" {
		c = revision[:min(len(revision), 7)]
		if modified == "true" {
			c += "-dirty"
		}
	}
	return v, c, d
}

func init() {
	rootCmd.AddCommand(versionCmd)
}

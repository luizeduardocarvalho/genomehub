package cmd

import (
	"fmt"
	"runtime"
	"runtime/debug"

	"github.com/spf13/cobra"
)

// Build metadata. Overridden at build time via -ldflags
//
//	-X github.com/luizeduardocarvalho/genomehub/cmd.version=v1.2.3
//	-X github.com/luizeduardocarvalho/genomehub/cmd.commit=abc1234
//	-X github.com/luizeduardocarvalho/genomehub/cmd.date=2026-06-13T00:00:00Z
//
// When built without ldflags (e.g. `go install ...@latest` or `go build`),
// these stay "dev" but versionString backfills version/commit from the Go
// module build info so installed binaries still report something useful.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// versionString renders the human-readable build line, backfilling from
// debug.ReadBuildInfo when ldflags weren't supplied.
func versionString() string {
	v, c, d := version, commit, date
	if info, ok := debug.ReadBuildInfo(); ok {
		if v == "dev" && info.Main.Version != "" && info.Main.Version != "(devel)" {
			v = info.Main.Version
		}
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				if c == "none" && s.Value != "" {
					c = s.Value
					if len(c) > 12 {
						c = c[:12]
					}
				}
			case "vcs.time":
				if d == "unknown" && s.Value != "" {
					d = s.Value
				}
			}
		}
	}
	return fmt.Sprintf("genomehub %s (commit %s, built %s, %s)", v, c, d, runtime.Version())
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print version, build commit, and date",
	Run: func(_ *cobra.Command, _ []string) {
		fmt.Println(versionString())
	},
}

func init() {
	rootCmd.AddCommand(versionCmd)
	rootCmd.Version = versionString()
	rootCmd.SetVersionTemplate("{{.Version}}\n")
}

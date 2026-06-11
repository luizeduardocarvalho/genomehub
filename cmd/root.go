package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
)

var storeDir string
var cmdStart time.Time

var rootCmd = &cobra.Command{
	Use:   "genomehub",
	Short: "Content-addressable genomic data protocol",
	PersistentPreRun: func(_ *cobra.Command, _ []string) {
		cmdStart = time.Now()
	},
	PersistentPostRun: func(_ *cobra.Command, _ []string) {
		fmt.Fprintf(os.Stderr, "elapsed: %s\n", time.Since(cmdStart).Round(time.Millisecond))
	},
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func indexDir() string {
	return filepath.Join(filepath.Dir(storeDir), "index")
}

func alnCacheDir() string {
	return filepath.Join(filepath.Dir(storeDir), "aligncache")
}

func sketchDir() string {
	return filepath.Join(filepath.Dir(storeDir), "sketches")
}

// eventsPath is the local activity log (import/download history) kept beside the
// store, tailed by a running node into its /status.
func eventsPath() string {
	return filepath.Join(filepath.Dir(storeDir), "events.jsonl")
}

// manifestCacheDir holds manifests fetched by `download`, kept beside the store.
// A node merges these into its catalog so a pure cache peer can name (and show
// coverage for) the genomes whose segments it holds — answering "am I seeding X?"
// even when it was started with an empty catalog.
func manifestCacheDir() string {
	return filepath.Join(filepath.Dir(storeDir), "manifests")
}

func init() {
	home, _ := os.UserHomeDir()
	defaultStore := filepath.Join(home, ".genomehub", "segments")
	rootCmd.PersistentFlags().StringVar(&storeDir, "store", defaultStore, "segment store directory")
}

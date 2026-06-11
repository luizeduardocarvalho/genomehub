package cmd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/luizcarvalho/genome-hub/internal/jobs"
	"github.com/spf13/cobra"
)

var (
	enqCoordinator string
	enqTarget      string
	enqQuery       string
	enqPreset      string
	enqMinExact    int
	enqTile        bool

	jobsCoordinator string
	jobsWatch       bool
	jobsInterval    time.Duration
)

var jobEnqueueCmd = &cobra.Command{
	Use:   "job-enqueue",
	Short: "Enqueue a MEM-finding job (genome pair) on the coordinator",
	RunE:  runJobEnqueue,
}

var jobsCmd = &cobra.Command{
	Use:   "jobs",
	Short: "List jobs on the coordinator and their state",
	RunE:  runJobs,
}

func init() {
	jobEnqueueCmd.Flags().StringVar(&enqCoordinator, "coordinator", "", "coordinator URL (required)")
	jobEnqueueCmd.Flags().StringVar(&enqTarget, "target", "", "target assembly (required)")
	jobEnqueueCmd.Flags().StringVar(&enqQuery, "query", "", "query assembly (required)")
	jobEnqueueCmd.Flags().StringVar(&enqPreset, "preset", "asm5", "minimap2 preset")
	jobEnqueueCmd.Flags().IntVar(&enqMinExact, "min-exact", 500, "minimum exact-match length")
	jobEnqueueCmd.Flags().BoolVar(&enqTile, "tile", false, "split into one job per query chromosome (parallel, streams progress)")
	jobEnqueueCmd.MarkFlagRequired("coordinator")
	jobEnqueueCmd.MarkFlagRequired("target")
	jobEnqueueCmd.MarkFlagRequired("query")
	rootCmd.AddCommand(jobEnqueueCmd)

	jobsCmd.Flags().StringVar(&jobsCoordinator, "coordinator", "", "coordinator URL (required)")
	jobsCmd.Flags().BoolVar(&jobsWatch, "watch", false, "live-refresh view (Ctrl-C to quit)")
	jobsCmd.Flags().DurationVar(&jobsInterval, "interval", 2*time.Second, "refresh interval with --watch")
	jobsCmd.MarkFlagRequired("coordinator")
	rootCmd.AddCommand(jobsCmd)
}

func runJobEnqueue(_ *cobra.Command, _ []string) error {
	body, status, err := postJSON(strings.TrimRight(enqCoordinator, "/")+"/jobs/enqueue", map[string]any{
		"Target": enqTarget, "Query": enqQuery, "Preset": enqPreset, "min_exact": enqMinExact, "tile": enqTile,
	})
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("enqueue failed: status %d (%s)", status, strings.TrimSpace(string(body)))
	}
	var created []jobs.Job
	if err := json.Unmarshal(body, &created); err != nil {
		return err
	}
	fmt.Printf("enqueued %d job(s): %s × %s (%s, min-exact %d)\n", len(created), enqTarget, enqQuery, enqPreset, enqMinExact)
	for _, j := range created {
		tile := j.QueryChrom
		if tile == "" {
			tile = "(whole genome)"
		}
		fmt.Printf("  %s  tile=%s\n", j.ID, tile)
	}
	return nil
}

func runJobs(_ *cobra.Command, _ []string) error {
	base := strings.TrimRight(jobsCoordinator, "/")
	if jobsWatch {
		return liveLoop(jobsInterval, func() string { return renderJobs(base) })
	}
	fmt.Print(renderJobs(base))
	return nil
}

// renderJobs fetches the coordinator's job list and formats it as a table with a
// summary header (counts by state, total verified MEMs).
func renderJobs(base string) string {
	var list []jobs.Job
	if err := getJSON(base+"/jobs", &list); err != nil {
		return fmt.Sprintf("%sGenomeHub — MEM jobs%s\n\n%scoordinator %s unreachable: %v%s\n", ansiBold, ansiReset, ansiRed, base, err, ansiReset)
	}

	var pending, claimed, done, totalValid, totalFound int
	for _, j := range list {
		switch j.State {
		case jobs.Pending:
			pending++
		case jobs.Claimed:
			claimed++
		case jobs.Done:
			done++
		}
		totalFound += j.Found
		totalValid += j.Valid
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%sGenomeHub — MEM jobs%s   %d job(s): %d pending, %d running, %d done   verified %d/%d MEMs   %s%s%s\n\n",
		ansiBold, ansiReset, len(list), pending, claimed, done, totalValid, totalFound,
		ansiDim, time.Now().Format("15:04:05"), ansiReset)
	if len(list) == 0 {
		fmt.Fprintf(&b, "  %s(no jobs enqueued)%s\n", ansiDim, ansiReset)
		return b.String()
	}
	fmt.Fprintf(&b, "%s  %-8s %-9s %-14s %-14s %7s %7s  %s%s\n", ansiDim, "ID", "STATE", "PAIR", "TILE", "FOUND", "VALID", "WORKER", ansiReset)
	for _, j := range list {
		col := ansiDim
		switch j.State {
		case jobs.Claimed:
			col = ansiAmber
		case jobs.Done:
			col = ansiGreen
		}
		tile := j.QueryChrom
		if tile == "" {
			tile = "whole"
		}
		fmt.Fprintf(&b, "  %-8s %s%-9s%s %-14s %-14s %7d %7d  %s\n",
			j.ID, col, j.State, ansiReset, trunc(j.Target+"×"+j.Query, 14), trunc(tile, 14), j.Found, j.Valid, j.Worker)
	}
	fmt.Fprintf(&b, "\n%sCtrl-C to quit%s\n", ansiDim, ansiReset)
	return b.String()
}

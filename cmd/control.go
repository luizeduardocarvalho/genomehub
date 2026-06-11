package cmd

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/luizeduardocarvalho/genomehub/internal/httpapi"
	"github.com/luizeduardocarvalho/genomehub/internal/jobs"
	"github.com/luizeduardocarvalho/genomehub/internal/tracker"
	"github.com/spf13/cobra"
)

var (
	ctlOrigin      string
	ctlTracker     string
	ctlCoordinator string
	ctlInterval    time.Duration
	ctlMaxRows     int
)

var controlCmd = &cobra.Command{
	Use:   "control",
	Short: "Live single-pane view of our infrastructure: origin archive, tracker, coordinator",
	Long: `The operator dashboard for the control + archive layer we run ourselves — the
parts that own correctness and durability. One screen, three panels, polled live:

  ORIGIN       the authoritative archive (uptime, segments held, bytes served)
  TRACKER      peer discovery + liveness (who is online, what they reseed)
  COORDINATOR  the MEM job queue (pending / running / done, MEMs verified)

Peers and workers are the community's; this view is the part that is ours. Each
panel degrades on its own — a down or unset endpoint never blanks the others.
Set at least one of --origin / --tracker / --coordinator. Ctrl-C to quit.`,
	RunE: runControl,
}

func init() {
	controlCmd.Flags().StringVar(&ctlOrigin, "origin", "", "origin node base URL (its /status)")
	controlCmd.Flags().StringVar(&ctlTracker, "tracker", "", "tracker base URL (its /nodes)")
	controlCmd.Flags().StringVar(&ctlCoordinator, "coordinator", "", "coordinator base URL (its /jobs)")
	controlCmd.Flags().DurationVar(&ctlInterval, "interval", 2*time.Second, "refresh interval")
	controlCmd.Flags().IntVar(&ctlMaxRows, "max-rows", 12, "max rows shown per table panel")
	rootCmd.AddCommand(controlCmd)
}

func runControl(_ *cobra.Command, _ []string) error {
	origin := strings.TrimRight(ctlOrigin, "/")
	trk := strings.TrimRight(ctlTracker, "/")
	coord := strings.TrimRight(ctlCoordinator, "/")
	if origin == "" && trk == "" && coord == "" {
		return fmt.Errorf("set at least one of --origin, --tracker, --coordinator")
	}

	return liveLoop(ctlInterval, func() string {
		// Fetch all three panels concurrently so one slow/dead endpoint never
		// stalls the frame past a single interval.
		var panels [3]string
		var wg sync.WaitGroup
		fetch := []struct {
			base string
			fn   func(string) string
		}{
			{origin, panelOrigin},
			{trk, panelTracker},
			{coord, panelCoordinator},
		}
		for i := range fetch {
			f := fetch[i]
			if f.base == "" {
				continue
			}
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				panels[idx] = f.fn(f.base)
			}(i)
		}
		wg.Wait()

		var b strings.Builder
		fmt.Fprintf(&b, "%sGenomeHub — control plane%s   %sour infrastructure%s   %s%s%s\n\n",
			ansiBold, ansiReset, ansiDim, ansiReset, ansiDim, time.Now().Format("15:04:05"), ansiReset)
		for _, p := range panels {
			if p != "" {
				b.WriteString(p)
				b.WriteString("\n")
			}
		}
		fmt.Fprintf(&b, "%sCtrl-C to quit%s\n", ansiDim, ansiReset)
		return b.String()
	})
}

// panelHeader renders a bold panel title with its endpoint URL and an optional
// right-aligned summary.
func panelHeader(title, base, summary string) string {
	if summary != "" {
		summary = "   " + summary
	}
	return fmt.Sprintf("%s%s%s  %s%s%s%s\n", ansiBold, title, ansiReset, ansiDim, base, ansiReset, summary)
}

func panelErr(title, base string, err error) string {
	return panelHeader(title, base, "") + fmt.Sprintf("  %sunreachable: %v%s\n", ansiRed, err, ansiReset)
}

func panelOrigin(base string) string {
	var st httpapi.Status
	if err := getJSON(base+"/status", &st); err != nil {
		return panelErr("ORIGIN", base, err)
	}
	var b strings.Builder
	b.WriteString(panelHeader("ORIGIN", base, fmt.Sprintf("up %s", fmtAge(st.UptimeSeconds))))
	fmt.Fprintf(&b, "  %ssegments%s %d   %srequests%s %d   %sserved%s %s\n",
		ansiDim, ansiReset, st.SegmentsHeld, ansiDim, ansiReset, st.Requests,
		ansiDim, ansiReset, fmtBytesInt(st.BytesServed))
	fmt.Fprintf(&b, "  %smanifests%s %s\n", ansiDim, ansiReset, orNone(st.Manifests))
	fmt.Fprintf(&b, "  %sdeltas%s    %s\n", ansiDim, ansiReset, orNone(st.Deltas))
	return b.String()
}

func panelTracker(base string) string {
	var nodes []tracker.NodeView
	if err := getJSON(base+"/nodes", &nodes); err != nil {
		return panelErr("TRACKER", base, err)
	}
	online := 0
	for _, n := range nodes {
		if n.Online {
			online++
		}
	}
	var b strings.Builder
	b.WriteString(panelHeader("TRACKER", base, fmt.Sprintf("%d node(s), %d online", len(nodes), online)))
	if len(nodes) == 0 {
		fmt.Fprintf(&b, "  %s(no nodes announced)%s\n", ansiDim, ansiReset)
		return b.String()
	}
	fmt.Fprintf(&b, "%s  %-26s %-7s %8s %8s   %s%s\n", ansiDim, "NODE", "KIND", "HELD", "AGE", "STATUS", ansiReset)
	for i, n := range nodes {
		if i >= ctlMaxRows {
			fmt.Fprintf(&b, "  %s… %d more%s\n", ansiDim, len(nodes)-ctlMaxRows, ansiReset)
			break
		}
		dot, col := "offline", ansiRed
		if n.Online {
			dot, col = "online", ansiGreen
		}
		fmt.Fprintf(&b, "  %-26s %-7s %8d %8s   %s● %s%s\n",
			trunc(n.NodeID, 26), n.Kind, n.Held, fmtAge(n.AgeSeconds), col, dot, ansiReset)
	}
	return b.String()
}

func panelCoordinator(base string) string {
	var list []jobs.Job
	if err := getJSON(base+"/jobs", &list); err != nil {
		return panelErr("COORDINATOR", base, err)
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
	b.WriteString(panelHeader("COORDINATOR", base,
		fmt.Sprintf("%d job(s): %d pending, %d running, %d done   verified %d/%d MEMs",
			len(list), pending, claimed, done, totalValid, totalFound)))
	if len(list) == 0 {
		fmt.Fprintf(&b, "  %s(no jobs enqueued)%s\n", ansiDim, ansiReset)
		return b.String()
	}
	fmt.Fprintf(&b, "%s  %-8s %-9s %-14s %-14s %7s %7s  %s%s\n", ansiDim, "ID", "STATE", "PAIR", "TILE", "FOUND", "VALID", "WORKER", ansiReset)
	for i, j := range list {
		if i >= ctlMaxRows {
			fmt.Fprintf(&b, "  %s… %d more%s\n", ansiDim, len(list)-ctlMaxRows, ansiReset)
			break
		}
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
			j.ID, col, j.State, ansiReset, trunc(j.Target+"×"+j.Query, 14), trunc(tile, 14), j.Found, j.Valid, trunc(j.Worker, 18))
	}
	return b.String()
}

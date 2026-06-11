package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/luizeduardocarvalho/genomehub/internal/events"
	"github.com/luizeduardocarvalho/genomehub/internal/httpapi"
	"github.com/luizeduardocarvalho/genomehub/internal/tracker"
	"github.com/spf13/cobra"
)

const (
	ansiClear = "\033[H\033[2J"
	ansiHide  = "\033[?25l"
	ansiShow  = "\033[?25h"
	ansiReset = "\033[0m"
	ansiBold  = "\033[1m"
	ansiDim   = "\033[2m"
	ansiGreen = "\033[32m"
	ansiRed   = "\033[31m"
	ansiAmber = "\033[33m"
)

var (
	topTracker  string
	topInterval time.Duration

	statusServer   string
	statusInterval time.Duration
	statusTracker  string
	statusID       string
)

var topCmd = &cobra.Command{
	Use:   "top",
	Short: "Live network view: all nodes, liveness and what they hold (polls the tracker)",
	Long:  "A k9s-style live table of every node the tracker knows — id, kind, segments held, heartbeat age, online/offline. Refreshes on an interval; Ctrl-C to quit.",
	RunE:  runTop,
}

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Live view of a single node (polls its /status endpoint)",
	Long: `A live panel for one node — uptime, segments held, requests served, bytes
served, and what it can serve. This is the participant's self-view: a peer or
worker watching their own box.

Pass --tracker to add a SWARM STANDING panel showing how the network sees this
node: whether the tracker counts it online, how many segments it is reseeding
for others, and its heartbeat age. Answers "am I contributing, and am I visible?"
Refreshes on an interval; Ctrl-C to quit.`,
	RunE: runStatus,
}

func init() {
	topCmd.Flags().StringVar(&topTracker, "tracker", "", "tracker base URL (required)")
	topCmd.Flags().DurationVar(&topInterval, "interval", 2*time.Second, "refresh interval")
	topCmd.MarkFlagRequired("tracker")
	rootCmd.AddCommand(topCmd)

	statusCmd.Flags().StringVar(&statusServer, "server", "", "node base URL (required)")
	statusCmd.Flags().DurationVar(&statusInterval, "interval", 2*time.Second, "refresh interval")
	statusCmd.Flags().StringVar(&statusTracker, "tracker", "", "tracker URL — also show how the swarm sees this node")
	statusCmd.Flags().StringVar(&statusID, "id", "", "this node's id/advertised URL in the tracker (default: --server)")
	statusCmd.MarkFlagRequired("server")
	rootCmd.AddCommand(statusCmd)
}

// liveLoop renders frame() on an interval until the user interrupts, restoring
// the cursor on exit.
func liveLoop(interval time.Duration, frame func() string) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	fmt.Print(ansiHide)
	defer fmt.Print(ansiShow)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		fmt.Print(ansiClear)
		fmt.Print(frame())
		select {
		case <-ctx.Done():
			fmt.Print(ansiShow + "\n")
			return nil
		case <-ticker.C:
		}
	}
}

func runTop(_ *cobra.Command, _ []string) error {
	base := strings.TrimRight(topTracker, "/")
	return liveLoop(topInterval, func() string {
		var nodes []tracker.NodeView
		if err := getJSON(base+"/nodes", &nodes); err != nil {
			return fmt.Sprintf("%sGenomeHub — network%s\n\ntracker %s\n\n%serror: %v%s\n", ansiBold, ansiReset, base, ansiRed, err, ansiReset)
		}
		online := 0
		for _, n := range nodes {
			if n.Online {
				online++
			}
		}
		var b strings.Builder
		fmt.Fprintf(&b, "%sGenomeHub — network%s   tracker %s   %d node(s), %d online   %s%s%s\n\n",
			ansiBold, ansiReset, base, len(nodes), online, ansiDim, time.Now().Format("15:04:05"), ansiReset)
		fmt.Fprintf(&b, "%s  %-26s %-7s %8s %8s   %s%s\n", ansiDim, "NODE", "KIND", "HELD", "AGE", "STATUS", ansiReset)
		if len(nodes) == 0 {
			fmt.Fprintf(&b, "  %s(no nodes announced)%s\n", ansiDim, ansiReset)
		}
		for _, n := range nodes {
			dot, col := "offline", ansiRed
			if n.Online {
				dot, col = "online", ansiGreen
			}
			fmt.Fprintf(&b, "  %-26s %-7s %8d %8s   %s● %s%s\n",
				trunc(n.NodeID, 26), n.Kind, n.Held, fmtAge(n.AgeSeconds), col, dot, ansiReset)
		}
		fmt.Fprintf(&b, "\n%sCtrl-C to quit%s\n", ansiDim, ansiReset)
		return b.String()
	})
}

func runStatus(_ *cobra.Command, _ []string) error {
	base := strings.TrimRight(statusServer, "/")
	return liveLoop(statusInterval, func() string {
		var st httpapi.Status
		if err := getJSON(base+"/status", &st); err != nil {
			return fmt.Sprintf("%sGenomeHub — node%s\n\n%s\n\n%serror: %v%s\n", ansiBold, ansiReset, base, ansiRed, err, ansiReset)
		}
		var b strings.Builder
		fmt.Fprintf(&b, "%sGenomeHub — node%s   %s   %s%s%s\n\n", ansiBold, ansiReset, base, ansiDim, time.Now().Format("15:04:05"), ansiReset)
		row := func(k, v string) { fmt.Fprintf(&b, "  %s%-16s%s %s\n", ansiDim, k, ansiReset, v) }
		row("uptime", fmtAge(st.UptimeSeconds))
		row("segments held", fmt.Sprintf("%d", st.SegmentsHeld))
		row("requests", fmt.Sprintf("%d", st.Requests))
		row("bytes served", fmtBytesInt(st.BytesServed))

		b.WriteString(seedingPanel(st.Seeding))
		b.WriteString(servingPanel(st))
		b.WriteString(recentPanel(st.Recent))
		if statusTracker != "" {
			b.WriteString(swarmStanding(base))
		}
		fmt.Fprintf(&b, "\n%sCtrl-C to quit%s\n", ansiDim, ansiReset)
		return b.String()
	})
}

// seedingPanel lists each genome this node can serve with a coverage bar — full
// seed vs partial cache vs file-served delta. Answers "am I seeding X?".
func seedingPanel(seeds []httpapi.Seeding) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\n  %sSEEDING%s\n", ansiBold, ansiReset)
	if len(seeds) == 0 {
		fmt.Fprintf(&b, "  %s(serving nothing — not a seed for any genome)%s\n", ansiDim, ansiReset)
		return b.String()
	}
	for _, s := range seeds {
		if s.Kind == "delta" || s.Total <= 0 {
			fmt.Fprintf(&b, "  %-14s %sdelta (file-served)%s\n", trunc(s.Assembly, 14), ansiDim, ansiReset)
			continue
		}
		pct := s.Have * 100 / s.Total
		col := ansiAmber
		switch {
		case s.Have >= s.Total:
			col = ansiGreen
		case s.Have == 0:
			col = ansiRed
		}
		fmt.Fprintf(&b, "  %-14s %s%s%s %s%3d%%%s  %s(%d/%d)%s\n",
			trunc(s.Assembly, 14), col, bar(s.Have, s.Total, 12), ansiReset,
			col, pct, ansiReset, ansiDim, s.Have, s.Total, ansiReset)
	}
	return b.String()
}

// servingPanel shows live upload rate and the last requests served — the "I am a
// source right now" feed.
func servingPanel(st httpapi.Status) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\n  %sSERVING%s   %s%.1f req/s · %s/s%s\n",
		ansiBold, ansiReset, ansiDim, st.ReqPerSec, fmtBytesInt(int64(st.BytesPerSec)), ansiReset)
	if len(st.Served) == 0 {
		fmt.Fprintf(&b, "  %s(idle — no recent requests)%s\n", ansiDim, ansiReset)
		return b.String()
	}
	for i, h := range st.Served {
		if i >= 6 {
			break
		}
		asm := h.Assembly
		if asm == "" {
			asm = "—"
		}
		col := ansiGreen
		if h.Status >= 400 {
			col = ansiRed
		}
		fmt.Fprintf(&b, "  %s%-22s%s → %-10s %8s  %s%2ds ago%s  %s%d%s\n",
			ansiDim, trunc(h.Path, 22), ansiReset, trunc(asm, 10), fmtBytesInt(int64(h.Bytes)),
			ansiDim, h.AgoSec, ansiReset, col, h.Status, ansiReset)
	}
	return b.String()
}

// recentPanel shows the last local import/download operations from the event log.
func recentPanel(evs []events.Event) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\n  %sRECENT (local ops)%s\n", ansiBold, ansiReset)
	if len(evs) == 0 {
		fmt.Fprintf(&b, "  %s(no imports or downloads recorded)%s\n", ansiDim, ansiReset)
		return b.String()
	}
	// newest first
	for i := len(evs) - 1; i >= 0; i-- {
		e := evs[i]
		icon, verb := "↓", "download"
		if e.Op == events.Import {
			icon, verb = "↑", "import"
		}
		ago := fmtAge(int(time.Since(e.Time).Seconds()))
		line := fmt.Sprintf("  %s %-8s %-14s %9s  %d seg  %s%s ago%s",
			icon, verb, trunc(e.Assembly, 14), fmtBytesInt(e.Bytes), e.Segments, ansiDim, ago, ansiReset)
		if e.Note != "" {
			line += fmt.Sprintf("  %s(%s)%s", ansiDim, e.Note, ansiReset)
		}
		b.WriteString(line + "\n")
	}
	return b.String()
}

// bar renders a fixed-width ▓/░ progress bar for have/total.
func bar(have, total, width int) string {
	if total <= 0 {
		return strings.Repeat("░", width)
	}
	filled := have * width / total
	if filled > width {
		filled = width
	}
	return strings.Repeat("▓", filled) + strings.Repeat("░", width-filled)
}

// swarmStanding renders how the tracker sees this node — online/offline,
// segments reseeded for peers, heartbeat age. id defaults to the node's own URL
// (the default node id when --id is unset on `node`). It is best-effort: if the
// tracker is down or has not seen us, it says so rather than failing the frame.
func swarmStanding(serverBase string) string {
	id := statusID
	if id == "" {
		id = serverBase
	}
	trk := strings.TrimRight(statusTracker, "/")
	var b strings.Builder
	fmt.Fprintf(&b, "\n  %sSWARM STANDING%s  %s%s%s\n", ansiBold, ansiReset, ansiDim, trk, ansiReset)
	var nodes []tracker.NodeView
	if err := getJSON(trk+"/nodes", &nodes); err != nil {
		fmt.Fprintf(&b, "  %stracker unreachable: %v%s\n", ansiRed, err, ansiReset)
		return b.String()
	}
	for _, n := range nodes {
		if n.NodeID != id {
			continue
		}
		state, col := "offline", ansiRed
		if n.Online {
			state, col = "online", ansiGreen
		}
		fmt.Fprintf(&b, "  %s%-16s%s %s● %s%s\n", ansiDim, "visibility", ansiReset, col, state, ansiReset)
		fmt.Fprintf(&b, "  %s%-16s%s %d for peers\n", ansiDim, "reseeding", ansiReset, n.Held)
		fmt.Fprintf(&b, "  %s%-16s%s %s\n", ansiDim, "heartbeat age", ansiReset, fmtAge(n.AgeSeconds))
		return b.String()
	}
	fmt.Fprintf(&b, "  %snot visible to tracker (not announced as %s)%s\n", ansiAmber, trunc(id, 40), ansiReset)
	return b.String()
}

func getJSON(url string, v any) error {
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	return json.Unmarshal(body, v)
}

func fmtAge(sec int) string {
	if sec < 60 {
		return fmt.Sprintf("%ds", sec)
	}
	return (time.Duration(sec) * time.Second).String()
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func orNone(xs []string) string {
	if len(xs) == 0 {
		return "—"
	}
	return strings.Join(xs, ", ")
}

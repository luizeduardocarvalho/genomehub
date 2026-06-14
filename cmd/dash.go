package cmd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/dustin/go-humanize"
	"github.com/luizeduardocarvalho/genomehub/internal/httpapi"
	"github.com/luizeduardocarvalho/genomehub/internal/tracker"
	"github.com/spf13/cobra"
)

var (
	dashServer   string
	dashTracker  string
	dashID       string
	dashInterval time.Duration
)

var dashCmd = &cobra.Command{
	Use:   "dash",
	Short: "Interactive tabbed dashboard for a node (bubbletea TUI)",
	Long: `A full-screen, navigable TUI for the participant's box. Tabs for an overview,
the genomes you seed (searchable), what you are serving right now, the swarm, and
your recent local activity. Unlike 'status' (a flat live panel), 'dash' is
keyboard-driven: switch tabs, search the seeding list, scroll long tables.

  genomehub dash --server http://node:8080 --tracker http://tracker:9000`,
	RunE: runDash,
}

func init() {
	dashCmd.Flags().StringVar(&dashServer, "server", "", "node base URL (required)")
	dashCmd.Flags().StringVar(&dashTracker, "tracker", "", "tracker URL (enables the Swarm tab + standing)")
	dashCmd.Flags().StringVar(&dashID, "id", "", "this node's id in the tracker (default: --server)")
	dashCmd.Flags().DurationVar(&dashInterval, "interval", 2*time.Second, "poll interval")
	dashCmd.MarkFlagRequired("server")
	rootCmd.AddCommand(dashCmd)
}

func runDash(_ *cobra.Command, _ []string) error {
	base := strings.TrimRight(dashServer, "/")
	id := dashID
	if id == "" {
		id = base
	}
	m := newDashModel(base, strings.TrimRight(dashTracker, "/"), id, dashInterval)
	_, err := tea.NewProgram(m, tea.WithAltScreen()).Run()
	return err
}

// ── tabs ────────────────────────────────────────────────────────────────────

type tab int

const (
	tabOverview tab = iota
	tabSeeding
	tabDiscover
	tabServing
	tabSwarm
	tabActivity
	numTabs
)

func (t tab) title() string {
	return [...]string{"Overview", "Seeding", "Discover", "Serving", "Swarm", "Activity"}[t]
}

// ── styling (placeholder palette; the design pass refines these) ─────────────

var (
	colDim    = lipgloss.Color("240")
	colHead   = lipgloss.Color("39")
	colGreen  = lipgloss.Color("42")
	colRed    = lipgloss.Color("203")
	colAmber  = lipgloss.Color("214")
	colSelBg  = lipgloss.Color("236")
	colAccent = lipgloss.Color("213")

	stTitle     = lipgloss.NewStyle().Bold(true).Foreground(colHead)
	stDim       = lipgloss.NewStyle().Foreground(colDim)
	stGreen     = lipgloss.NewStyle().Foreground(colGreen)
	stRed       = lipgloss.NewStyle().Foreground(colRed)
	stAmber     = lipgloss.NewStyle().Foreground(colAmber)
	stTabOn     = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("231")).Background(colAccent).Padding(0, 1)
	stTabOff    = lipgloss.NewStyle().Foreground(colDim).Padding(0, 1)
	stFooter    = lipgloss.NewStyle().Foreground(colDim)
	stHeaderRow = lipgloss.NewStyle().Foreground(colDim).Bold(true)
)

// ── messages ─────────────────────────────────────────────────────────────────

type tickMsg time.Time
type statusMsg struct {
	st  httpapi.Status
	err error
}
type nodesMsg struct {
	nodes []tracker.NodeView
	err   error
}
type discoverMsg struct {
	entries []httpapi.DiscoverEntry
	err     error
}
type actionMsg struct {
	text string
	err  error
}
type chromsMsg struct {
	assembly string
	chroms   []httpapi.ChromCoverage
	err      error
}
type coverageMsg struct {
	assembly string
	have     int
	total    int
}

// ── model ────────────────────────────────────────────────────────────────────

type dashModel struct {
	base, tracker, id string
	interval          time.Duration
	width, height     int

	active tab

	st       httpapi.Status
	stErr    error
	nodes    []tracker.NodeView
	ndErr    error
	discover []httpapi.DiscoverEntry
	dscErr   error
	covCache map[string][2]int // assembly -> {have,total} fetched on demand for big genomes
	lastAt   time.Time

	search    textinput.Model
	searching bool
	cursor    int    // selected row in the active tab's table
	flash     string // transient action feedback shown in the footer
	confirm   string // pending destructive action awaiting y/N, e.g. "delete:ATHENA2"

	// chromosome picker (opened with [c] on a Discover/Seeding genome)
	picking      bool
	pickAssembly string
	pickChroms   []httpapi.ChromCoverage
	pickSel      map[string]bool
	pickCursor   int
}

func newDashModel(base, trk, id string, interval time.Duration) dashModel {
	ti := textinput.New()
	ti.Placeholder = "filter genomes…"
	ti.Prompt = "/"
	ti.CharLimit = 64
	return dashModel{base: base, tracker: trk, id: id, interval: interval, search: ti}
}

func (m dashModel) Init() tea.Cmd {
	return tea.Batch(m.fetchStatus(), m.fetchNodes(), m.fetchDiscover(), tick(m.interval))
}

func tick(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m dashModel) fetchStatus() tea.Cmd {
	base := m.base
	return func() tea.Msg {
		var st httpapi.Status
		err := getJSON(base+"/status", &st)
		return statusMsg{st: st, err: err}
	}
}

func (m dashModel) fetchNodes() tea.Cmd {
	trk := m.tracker
	if trk == "" {
		return func() tea.Msg { return nodesMsg{} }
	}
	return func() tea.Msg {
		var ns []tracker.NodeView
		err := getJSON(trk+"/nodes", &ns)
		return nodesMsg{nodes: ns, err: err}
	}
}

func (m dashModel) fetchDiscover() tea.Cmd {
	base := m.base
	return func() tea.Msg {
		var ds []httpapi.DiscoverEntry
		err := getJSON(base+"/discover", &ds)
		return discoverMsg{entries: ds, err: err}
	}
}

func (m dashModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case tickMsg:
		return m, tea.Batch(m.fetchStatus(), m.fetchNodes(), m.fetchDiscover(), tick(m.interval))

	case statusMsg:
		m.st, m.stErr, m.lastAt = msg.st, msg.err, time.Now()
		m.clampCursor()
		return m, nil

	case nodesMsg:
		m.nodes, m.ndErr = msg.nodes, msg.err
		m.clampCursor()
		return m, nil

	case discoverMsg:
		m.discover, m.dscErr = msg.entries, msg.err
		m.applyCovCache()
		m.clampCursor()
		return m, m.maybeCoverageCmd()

	case coverageMsg:
		if m.covCache == nil {
			m.covCache = map[string][2]int{}
		}
		m.covCache[msg.assembly] = [2]int{msg.have, msg.total}
		m.applyCovCache()
		return m, nil

	case actionMsg:
		if msg.err != nil {
			m.flash = "✗ " + msg.text + ": " + msg.err.Error()
			return m, nil
		}
		m.flash = "✓ " + msg.text
		// reflect the new manifest immediately
		return m, tea.Batch(m.fetchStatus(), m.fetchDiscover())

	case chromsMsg:
		if msg.err != nil {
			m.flash = "✗ chromosomes " + msg.assembly + ": " + msg.err.Error()
			return m, nil
		}
		m.picking = true
		m.pickAssembly = msg.assembly
		m.pickChroms = msg.chroms
		m.pickCursor = 0
		m.pickSel = map[string]bool{}
		for _, c := range msg.chroms { // default-select chromosomes you don't fully hold
			if c.Have < c.Segments {
				m.pickSel[c.Name] = true
			}
		}
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)
	}

	// forward to the search input while it has focus
	if m.searching {
		var cmd tea.Cmd
		m.search, cmd = m.search.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m dashModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// A pending destructive action captures the next key (y confirms, else cancels).
	if m.confirm != "" {
		action, assembly, _ := strings.Cut(m.confirm, ":")
		m.confirm = ""
		if msg.String() != "y" && msg.String() != "Y" {
			m.flash = "cancelled"
			return m, nil
		}
		switch action {
		case "delete":
			m.flash = "deleting " + assembly + "…"
			return m, m.simpleActionCmd("/actions/delete", assembly, "deleted "+assembly)
		case "unseed":
			m.flash = "stopped seeding " + assembly
			return m, m.simpleActionCmd("/actions/unseed", assembly, "stopped seeding "+assembly)
		}
		return m, nil
	}
	// Chromosome picker captures keys until closed.
	if m.picking {
		return m.handlePickerKey(msg)
	}
	// Search mode swallows most keys until Esc/Enter.
	if m.searching {
		switch msg.String() {
		case "esc":
			m.searching = false
			m.search.Blur()
			m.search.SetValue("")
			return m, nil
		case "enter":
			m.searching = false
			m.search.Blur()
			return m, nil
		default:
			var cmd tea.Cmd
			m.search, cmd = m.search.Update(msg)
			m.cursor = 0 // filtering changes the list; restart selection at the top
			return m, cmd
		}
	}

	switch msg.String() {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "tab", "right", "l":
		m.active = (m.active + 1) % numTabs
		m.cursor = 0
		return m, nil
	case "shift+tab", "left", "h":
		m.active = (m.active + numTabs - 1) % numTabs
		m.cursor = 0
		return m, nil
	case "1", "2", "3", "4", "5", "6":
		if t := tab(msg.String()[0] - '1'); t < numTabs {
			m.active = t
			m.cursor = 0
		}
		return m, nil
	case "/":
		if m.active == tabSeeding || m.active == tabDiscover {
			m.searching = true
			m.search.Focus()
			return m, textinput.Blink
		}
	case "down", "j":
		if m.cursor < m.activeRows()-1 {
			m.cursor++
		}
		return m, m.maybeCoverageCmd()
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
		return m, m.maybeCoverageCmd()
	case "g", "home":
		m.cursor = 0
		return m, m.maybeCoverageCmd()
	case "G", "end":
		m.cursor = max(0, m.activeRows()-1)
		return m, m.maybeCoverageCmd()
	case "t", "T": // track the selected genome's manifest (Discover)
		if a := m.selectedAssembly(); a != "" {
			m.flash = "tracking " + a + "…"
			return m, m.manifestCmd(a, "tracked")
		}
		return m, nil
	case "u", "U": // update (refresh) the selected genome's manifest
		if a := m.selectedAssembly(); a != "" {
			m.flash = "updating " + a + "…"
			return m, m.manifestCmd(a, "updated")
		}
		return m, nil
	case "d", "D": // download missing segments of the selected genome
		if a := m.selectedAssembly(); a != "" {
			m.flash = "downloading " + a + "…"
			return m, m.downloadCmd(a, "", "download started for "+a)
		}
		return m, nil
	case "p", "P": // pause / resume the selected genome's download
		if a := m.selectedAssembly(); a != "" {
			op, verb := "pause", "paused"
			if d := m.dlFor(a); d != nil && d.State == "paused" {
				op, verb = "resume", "resumed"
			}
			return m, m.downloadCmd(a, "/"+op, verb+" "+a)
		}
		return m, nil
	case "x": // cancel the selected genome's download
		if a := m.selectedAssembly(); a != "" {
			return m, m.downloadCmd(a, "/cancel", "cancelled "+a)
		}
		return m, nil
	case "c", "C": // open the chromosome picker for the selected genome
		if a := m.selectedAssembly(); a != "" {
			if m.isDelta(a) {
				m.flash = stDim.Render(a + ": chromosome picker not available for delta genomes")
				return m, nil
			}
			m.flash = "loading chromosomes…"
			return m, m.fetchChromsCmd(a)
		}
		return m, nil
	case "X": // delete the selected genome from cache (destructive → confirm)
		if a := m.selectedAssembly(); a != "" {
			m.confirm = "delete:" + a
			m.flash = stAmber.Render("delete " + a + " from cache? frees its unique segments — [y/N]")
		}
		return m, nil
	case "R": // reconstruct the selected genome to a FASTA on the node
		if a := m.selectedAssembly(); a != "" {
			m.flash = "reconstructing " + a + "…"
			return m, m.reconstructCmd(a)
		}
		return m, nil
	case "S": // stop seeding the selected genome (keep segments → confirm)
		if a := m.selectedAssembly(); a != "" {
			m.confirm = "unseed:" + a
			m.flash = stAmber.Render("stop seeding " + a + "? keeps segments — [y/N]")
		}
		return m, nil
	case "r":
		return m, tea.Batch(m.fetchStatus(), m.fetchNodes(), m.fetchDiscover())
	}
	return m, nil
}

// ── view ─────────────────────────────────────────────────────────────────────

func (m dashModel) View() string {
	if m.width == 0 {
		return "loading…"
	}
	var b strings.Builder
	b.WriteString(m.headerBar())
	b.WriteString("\n")
	b.WriteString(m.tabBar())
	b.WriteString("\n\n")
	b.WriteString(m.body())
	b.WriteString("\n")
	b.WriteString(m.footer())
	return b.String()
}

func (m dashModel) headerBar() string {
	dot := stRed.Render("● offline")
	if m.stErr == nil && m.st.UptimeSeconds >= 0 && !m.lastAt.IsZero() {
		dot = stGreen.Render("● online")
	}
	if m.stErr != nil {
		dot = stRed.Render("● unreachable")
	}
	left := stTitle.Render("GenomeHub") + stDim.Render("  "+m.base)
	right := dot + stDim.Render("  "+time.Now().Format("15:04:05"))
	return padBetween(left, right, m.width)
}

func (m dashModel) tabBar() string {
	var parts []string
	for t := tab(0); t < numTabs; t++ {
		label := fmt.Sprintf("%d %s", t+1, t.title())
		if t == m.active {
			parts = append(parts, stTabOn.Render(label))
		} else {
			parts = append(parts, stTabOff.Render(label))
		}
	}
	left := strings.Join(parts, " ")
	// Right-aligned at-a-glance counts: genomes seeded, swarm online/total.
	counts := stDim.Render(fmt.Sprintf("seeding %d", len(m.st.Seeding)))
	if m.tracker != "" {
		online := 0
		for _, n := range m.nodes {
			if n.Online {
				online++
			}
		}
		counts += stDim.Render(fmt.Sprintf("   swarm %d/%d", online, len(m.nodes)))
	}
	return padBetween(left, counts, m.width)
}

// orDash returns s, or an em dash when empty.
func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}

func (m dashModel) footer() string {
	keys := "[Tab] switch   [1-6] jump   [↑↓] select   [g/G] top/btm   [r] refresh   [q] quit"
	if m.picking {
		keys = "[↑↓] move   [space] toggle   [a] all   [enter] download selected   [esc] cancel"
	} else if m.searching {
		keys = "[esc] cancel   [enter] apply   — typing filter"
	} else if m.active == tabSeeding {
		keys = "[/] search  [↑↓] sel  [D] download  [c] chroms  [R] reconstruct  [U] update  [S] stop  [X] del  [q] quit"
	} else if m.active == tabDiscover {
		keys = "[/] search  [↑↓] sel  [T] track  [D] dl  [c] chroms  [R] reconstruct  [p] pause  [x] cancel  [S] stop  [X] del"
	}
	if m.flash != "" && !m.searching {
		return stFooter.Render(keys) + "\n  " + m.flash
	}
	return stFooter.Render(keys)
}

// body dispatches to the active tab's renderer.
func (m dashModel) body() string {
	if m.picking {
		return m.viewPicker()
	}
	if m.stErr != nil {
		return stRed.Render(fmt.Sprintf("node unreachable: %v", m.stErr))
	}
	switch m.active {
	case tabOverview:
		return m.viewOverview()
	case tabSeeding:
		return m.viewSeeding()
	case tabDiscover:
		return m.viewDiscover()
	case tabServing:
		return m.viewServing()
	case tabSwarm:
		return m.viewSwarm()
	case tabActivity:
		return m.viewActivity()
	}
	return ""
}

// seedStats summarises the seeding list for the Overview cards.
type seedStats struct {
	full, partial, delta, manifests int
	sumHave, sumTotal               int
}

func (m dashModel) seedStats() seedStats {
	var s seedStats
	for _, sd := range m.st.Seeding {
		if sd.Kind == "delta" || sd.Total <= 0 {
			s.delta++
			continue
		}
		s.manifests++
		s.sumHave += sd.Have
		s.sumTotal += sd.Total
		if sd.Have >= sd.Total {
			s.full++
		} else {
			s.partial++
		}
	}
	return s
}

func (m dashModel) viewOverview() string {
	s := m.seedStats()

	// Library coverage = how much of the genomes you track you actually hold.
	// Deliberately NOT shown as "held / cap": the store has no cap, and X/Y
	// reads like a quota. Big count stands alone; coverage is a separate %.
	covPct := 100.0
	if s.sumTotal > 0 {
		covPct = float64(s.sumHave) * 100 / float64(s.sumTotal)
	}
	peers := ""
	if m.tracker != "" {
		online := 0
		for _, n := range m.nodes {
			if n.Online {
				online++
			}
		}
		peers = fmt.Sprintf(" · %d peers", online)
	}
	nodeState := stGreen.Render("● online")
	if m.stErr != nil {
		nodeState = stRed.Render("● unreachable")
	}

	rows := [][2]string{
		{"SEGMENTS HELD", humanize.Comma(int64(m.st.SegmentsHeld))},
		{"LIBRARY COVERAGE", fmt.Sprintf("%.1f%%   %s", covPct, stDim.Render(fmt.Sprintf("(%d of %d genomes complete)", s.full, s.manifests)))},
		{"UPTIME", fmtAge(m.st.UptimeSeconds)},
		{"UPLOAD RATE", fmt.Sprintf("%.1f req/s · %s/s", m.st.ReqPerSec, fmtBytesInt(int64(m.st.BytesPerSec)))},
		{"BYTES SERVED", fmtBytesInt(m.st.BytesServed)},
		{"NODE", nodeState + stDim.Render(peers)},
		{"GENOMES", fmt.Sprintf("%s full · %s partial · %s delta · %d total",
			stGreen.Render(fmt.Sprintf("%d", s.full)),
			stAmber.Render(fmt.Sprintf("%d", s.partial)),
			stDim.Render(fmt.Sprintf("%d", s.delta)),
			len(m.st.Seeding))},
	}
	var b strings.Builder
	for _, r := range rows {
		fmt.Fprintf(&b, "  %s %s\n", stDim.Render(fmt.Sprintf("%-18s", r[0])), r[1])
	}
	// The full assembly list lives in the Seeding tab — don't re-dump it here.
	b.WriteString("\n" + stDim.Render("  → press 2 for the full genome list (searchable)"))
	return b.String()
}

func (m dashModel) viewSeeding() string {
	var b strings.Builder
	if m.searching || m.search.Value() != "" {
		b.WriteString("  " + m.search.View() + "\n\n")
	}
	filtered := m.filteredSeeding()
	if len(filtered) == 0 {
		return b.String() + stDim.Render("  (no matching genomes — you are not a seed for any)")
	}
	fmt.Fprintf(&b, "%s\n", stHeaderRow.Render(fmt.Sprintf("  %-18s %-14s %5s   %-12s %-14s %s", "GENOME", "COVERAGE", "PCT", "SEGMENTS", "ROLE", "SPECIES")))
	rows, more := m.window(len(filtered))
	for i := rows.lo; i < rows.hi; i++ {
		s := filtered[i]
		species := stDim.Render(orDash(s.Organism))
		if s.Kind == "delta" {
			if s.Total <= 0 {
				// Raw blob delta: file-served directly from disk.
				fmt.Fprintf(&b, "%s%-18s %-31s %-14s %s\n", m.caret(i), trunc(s.Assembly, 18), stDim.Render("delta (file-served)"), "", species)
			} else {
				// Recipe-backed delta: show real chunk coverage.
				pct := s.Have * 100 / s.Total
				st := stAmber
				role := "delta partial"
				switch {
				case s.Have >= s.Total:
					st, role = stGreen, "delta full"
				case s.Have == 0:
					st, role = stRed, "delta 0 chunks"
				}
				fmt.Fprintf(&b, "%s%-18s %s %s   %-12s %-14s %s\n",
					m.caret(i), trunc(s.Assembly, 18), st.Render(bar(s.Have, s.Total, 12)),
					st.Render(fmt.Sprintf("%4d%%", pct)),
					fmt.Sprintf("%d/%d", s.Have, s.Total), stDim.Render(role), species)
			}
			continue
		}
		pct := s.Have * 100 / s.Total
		st := stAmber
		role := "partial cache"
		switch {
		case s.Have >= s.Total:
			st, role = stGreen, "full seed"
		case s.Have == 0:
			st, role = stRed, "empty"
		}
		fmt.Fprintf(&b, "%s%-18s %s %s   %-12s %-14s %s\n",
			m.caret(i), trunc(s.Assembly, 18), st.Render(bar(s.Have, s.Total, 12)),
			st.Render(fmt.Sprintf("%4d%%", pct)),
			fmt.Sprintf("%d/%d", s.Have, s.Total), stDim.Render(role), species)
	}
	if more != "" {
		b.WriteString(stDim.Render("  " + more))
	}
	return b.String()
}

func (m dashModel) viewPicker() string {
	var b strings.Builder
	sel := 0
	for _, c := range m.pickChroms {
		if m.pickSel[c.Name] {
			sel++
		}
	}
	fmt.Fprintf(&b, "  %s  %s\n\n", stTitle.Render("Chromosomes — "+m.pickAssembly),
		stDim.Render(fmt.Sprintf("%d selected", sel)))
	for i, c := range m.pickChroms {
		box := "☐"
		if m.pickSel[c.Name] {
			box = stGreen.Render("☑")
		}
		st := stAmber
		if c.Have >= c.Segments {
			st = stGreen
		} else if c.Have == 0 {
			st = stRed
		}
		fmt.Fprintf(&b, "%s%s %-10s %s %s\n",
			m.pickCaret(i), box, trunc(c.Name, 10), st.Render(bar(c.Have, c.Segments, 10)),
			stDim.Render(fmt.Sprintf("%d/%d held", c.Have, c.Segments)))
	}
	return b.String()
}

func (m dashModel) pickCaret(i int) string {
	if i == m.pickCursor {
		return lipgloss.NewStyle().Foreground(colAccent).Bold(true).Render("▸ ")
	}
	return "  "
}

func (m dashModel) viewDiscover() string {
	var b strings.Builder
	if m.searching || m.search.Value() != "" {
		b.WriteString("  " + m.search.View() + "\n\n")
	}
	if m.dscErr != nil {
		return b.String() + stRed.Render(fmt.Sprintf("  registry unreachable: %v", m.dscErr))
	}
	filtered := m.filteredDiscover()
	if len(filtered) == 0 {
		return b.String() + stDim.Render("  (no genomes in the registry — is the node's --registry set?)")
	}
	fmt.Fprintf(&b, "%s\n", stHeaderRow.Render(fmt.Sprintf("  %-16s %-14s %5s   %-12s %4s %4s %-13s %s",
		"GENOME", "YOU HAVE", "PCT", "SEGMENTS", "VER", "SRC", "SPECIES", "STATE")))
	rows, more := m.window(len(filtered))
	for i := rows.lo; i < rows.hi; i++ {
		e := filtered[i]
		pct := 0
		if e.Total > 0 {
			pct = e.Have * 100 / e.Total
		}
		st := stAmber
		state := "partial"
		switch {
		case e.Total > 0 && e.Have >= e.Total:
			st, state = stGreen, "have all"
		case e.Have == 0:
			st, state = stRed, "not held"
		}
		if e.Local {
			state += " · seeding"
		}
		if d := m.dlFor(e.Assembly); d != nil {
			switch d.State {
			case "running":
				state = stGreen.Render(fmt.Sprintf("↓ %d/%d", d.Have, d.Total))
			case "paused":
				state = stAmber.Render("⏸ paused")
			case "error":
				state = stRed.Render("✗ error")
			}
		}
		src := stDim.Render(fmt.Sprintf("%4d", e.Sources))
		if e.Sources == 0 {
			src = stRed.Render("   0") // nobody holds it — unavailable
		}
		// Coverage deferred for a big genome — show a placeholder until selected.
		barCell := st.Render(bar(e.Have, e.Total, 12))
		pctCell := st.Render(fmt.Sprintf("%4d%%", pct))
		segCell := fmt.Sprintf("%d/%d", e.Have, e.Total)
		if !e.CoverageKnown {
			barCell = stDim.Render(strings.Repeat("·", 12))
			pctCell = stDim.Render("   ?")
			segCell = "?"
			state = stDim.Render("select to size")
		}
		fmt.Fprintf(&b, "%s%-16s %s %s   %-12s %4d %s %-13s %s\n",
			m.caret(i), trunc(e.Assembly, 16), barCell, pctCell,
			segCell, e.Version, src,
			trunc(orDash(e.Organism), 13), stDim.Render(state))
	}
	if more != "" {
		b.WriteString(stDim.Render("  " + more + "\n"))
	}
	// Detail line for the selected genome — the target of [T]/[U]/[D].
	if m.cursor < len(filtered) {
		e := filtered[m.cursor]
		if d := m.dlFor(e.Assembly); d != nil && (d.State == "running" || d.State == "paused") {
			col := stGreen
			if d.State == "paused" {
				col = stAmber
			}
			b.WriteString("\n  " + col.Render(bar(d.Have, d.Total, 16)) +
				fmt.Sprintf("  %s  %d/%d segments  %s  ", col.Render(strings.ToUpper(d.State)), d.Have, d.Total, fmtBytesInt(d.Bytes)) +
				stDim.Render("[p] pause/resume  [x] cancel"))
		} else {
			missing := e.Total - e.Have
			hint := "[D] download the rest"
			if missing == 0 {
				hint = "[U] update manifest"
			}
			b.WriteString("\n" + stDim.Render(fmt.Sprintf("  selected: %s — hold %d/%d segments, %d missing   ", e.Assembly, e.Have, e.Total, missing)) + stDim.Render(hint))
		}
	}
	return b.String()
}

func (m dashModel) viewServing() string {
	var b strings.Builder
	fmt.Fprintf(&b, "  %s\n\n", stDim.Render(fmt.Sprintf("%.1f req/s · %s/s (last 10s)", m.st.ReqPerSec, fmtBytesInt(int64(m.st.BytesPerSec)))))
	if len(m.st.Served) == 0 {
		return b.String() + stDim.Render("  (idle — no recent requests)")
	}
	fmt.Fprintf(&b, "%s\n", stHeaderRow.Render(fmt.Sprintf("  %-26s %-12s %9s %7s %s", "PATH", "GENOME", "BYTES", "AGE", "CODE")))
	rows, more := m.window(len(m.st.Served))
	for i := rows.lo; i < rows.hi; i++ {
		h := m.st.Served[i]
		asm := h.Assembly
		if asm == "" {
			asm = "—"
		}
		st := stGreen
		if h.Status >= 400 {
			st = stRed
		}
		fmt.Fprintf(&b, "%s%-26s %-12s %9s %6ds %s\n",
			m.caret(i), trunc(h.Path, 26), trunc(asm, 12), fmtBytesInt(int64(h.Bytes)), h.AgoSec, st.Render(fmt.Sprintf("%d", h.Status)))
	}
	if more != "" {
		b.WriteString(stDim.Render("  " + more))
	}
	return b.String()
}

func (m dashModel) viewSwarm() string {
	if m.tracker == "" {
		return stDim.Render("  (no --tracker set — swarm view unavailable)")
	}
	if m.ndErr != nil {
		return stRed.Render(fmt.Sprintf("  tracker unreachable: %v", m.ndErr))
	}
	nodes := append([]tracker.NodeView(nil), m.nodes...)
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].NodeID < nodes[j].NodeID })
	var b strings.Builder
	online := 0
	for _, n := range nodes {
		if n.Online {
			online++
		}
	}
	fmt.Fprintf(&b, "  %s\n\n", stDim.Render(fmt.Sprintf("%d node(s), %d online", len(nodes), online)))
	if len(nodes) == 0 {
		return b.String() + stDim.Render("  (no nodes announced)")
	}
	fmt.Fprintf(&b, "%s\n", stHeaderRow.Render(fmt.Sprintf("  %-28s %-7s %8s %8s   %s", "NODE", "KIND", "HELD", "AGE", "STATUS")))
	rows, more := m.window(len(nodes))
	for i := rows.lo; i < rows.hi; i++ {
		n := nodes[i]
		st, label := stRed, "offline"
		if n.Online {
			st, label = stGreen, "online"
		}
		me := ""
		if n.NodeID == m.id {
			me = stAmber.Render(" ← you")
		}
		fmt.Fprintf(&b, "%s%-28s %-7s %8d %8s   %s%s\n",
			m.caret(i), trunc(n.NodeID, 28), n.Kind, n.Held, fmtAge(n.AgeSeconds), st.Render("● "+label), me)
	}
	if more != "" {
		b.WriteString(stDim.Render("  " + more))
	}
	return b.String()
}

func (m dashModel) viewActivity() string {
	if len(m.st.Recent) == 0 {
		return stDim.Render("  (no imports or downloads recorded)")
	}
	var b strings.Builder
	for i := len(m.st.Recent) - 1; i >= 0; i-- {
		e := m.st.Recent[i]
		icon, verb := "↓", "download"
		if string(e.Op) == "import" {
			icon, verb = "↑", "import"
		}
		ago := fmtAge(int(time.Since(e.Time).Seconds()))
		line := fmt.Sprintf("  %s %-9s %-16s %9s  %d seg  %s",
			icon, verb, trunc(e.Assembly, 16), fmtBytesInt(e.Bytes), e.Segments, stDim.Render(ago+" ago"))
		if e.Note != "" {
			line += stDim.Render("  (" + e.Note + ")")
		}
		b.WriteString(line + "\n")
	}
	return b.String()
}

// ── table windowing (scroll) ─────────────────────────────────────────────────

type rowRange struct{ lo, hi int }

// window computes the visible row slice so the cursor stays in view, plus a
// "… N more" hint when the table overflows.
func (m dashModel) window(total int) (rowRange, string) {
	// Reserve lines for header bar, tab bar, blank, table header, footer.
	avail := m.height - 9
	if avail < 3 {
		avail = 3
	}
	if total <= avail {
		return rowRange{0, total}, ""
	}
	// Center the cursor in the viewport, clamped to the ends.
	lo := m.cursor - avail/2
	if lo > total-avail {
		lo = total - avail
	}
	if lo < 0 {
		lo = 0
	}
	hi := lo + avail
	hint := ""
	if hi < total {
		hint = fmt.Sprintf("… %d more (↓)", total-hi)
	}
	return rowRange{lo, hi}, hint
}

// activeRows is the number of selectable rows in the current tab — bounds the
// cursor.
func (m dashModel) activeRows() int {
	switch m.active {
	case tabSeeding:
		return len(m.filteredSeeding())
	case tabDiscover:
		return len(m.filteredDiscover())
	case tabSwarm:
		return len(m.nodes)
	case tabServing:
		return len(m.st.Served)
	}
	return 0
}

// filteredSeeding / filteredDiscover apply the search filter; shared by the
// views and the cursor bound so selection and display stay in sync.
func (m dashModel) filteredSeeding() []httpapi.Seeding {
	q := strings.ToLower(strings.TrimSpace(m.search.Value()))
	out := make([]httpapi.Seeding, 0, len(m.st.Seeding))
	for _, s := range m.st.Seeding {
		if q == "" || strings.Contains(strings.ToLower(s.Assembly), q) || strings.Contains(strings.ToLower(s.Organism), q) {
			out = append(out, s)
		}
	}
	return out
}

func (m dashModel) filteredDiscover() []httpapi.DiscoverEntry {
	q := strings.ToLower(strings.TrimSpace(m.search.Value()))
	out := make([]httpapi.DiscoverEntry, 0, len(m.discover))
	for _, e := range m.discover {
		if q == "" || strings.Contains(strings.ToLower(e.Assembly), q) || strings.Contains(strings.ToLower(e.Organism), q) {
			out = append(out, e)
		}
	}
	return out
}

// clampCursor keeps the selection in range after the underlying data changes.
func (m *dashModel) clampCursor() {
	if r := m.activeRows(); m.cursor > r-1 {
		if r > 0 {
			m.cursor = r - 1
		} else {
			m.cursor = 0
		}
	}
}

// selectedAssembly returns the genome under the cursor on the Discover/Seeding
// tabs (the target of track/update actions), or "" if none.
func (m dashModel) selectedAssembly() string {
	switch m.active {
	case tabDiscover:
		if f := m.filteredDiscover(); m.cursor < len(f) {
			return f[m.cursor].Assembly
		}
	case tabSeeding:
		if f := m.filteredSeeding(); m.cursor < len(f) {
			return f[m.cursor].Assembly
		}
	}
	return ""
}

// manifestCmd posts a track/update of a genome's manifest to the node and reports
// the outcome as an actionMsg.
func (m dashModel) manifestCmd(assembly, verb string) tea.Cmd {
	base := m.base
	return func() tea.Msg {
		body, status, err := postJSON(base+"/actions/manifest", map[string]any{"assembly": assembly})
		if err != nil {
			return actionMsg{text: assembly, err: err}
		}
		if status != http.StatusOK {
			return actionMsg{text: assembly, err: fmt.Errorf("%s", strings.TrimSpace(string(body)))}
		}
		return actionMsg{text: fmt.Sprintf("%s %s", assembly, verb)}
	}
}

// handlePickerKey drives the chromosome picker overlay.
func (m dashModel) handlePickerKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "q":
		m.picking = false
		return m, nil
	case "down", "j":
		if m.pickCursor < len(m.pickChroms)-1 {
			m.pickCursor++
		}
	case "up", "k":
		if m.pickCursor > 0 {
			m.pickCursor--
		}
	case " ", "space", "x":
		if m.pickCursor < len(m.pickChroms) {
			name := m.pickChroms[m.pickCursor].Name
			m.pickSel[name] = !m.pickSel[name]
		}
	case "a": // toggle all
		all := true
		for _, c := range m.pickChroms {
			if !m.pickSel[c.Name] {
				all = false
				break
			}
		}
		for _, c := range m.pickChroms {
			m.pickSel[c.Name] = !all
		}
	case "enter", "d", "D":
		var chroms []string
		for _, c := range m.pickChroms {
			if m.pickSel[c.Name] {
				chroms = append(chroms, c.Name)
			}
		}
		assembly := m.pickAssembly
		m.picking = false
		if len(chroms) == 0 {
			m.flash = "nothing selected"
			return m, nil
		}
		m.flash = fmt.Sprintf("downloading %d chromosome(s) of %s…", len(chroms), assembly)
		return m, m.downloadChromsCmd(assembly, chroms)
	}
	return m, nil
}

// fetchChromsCmd loads per-chromosome coverage for a genome (opens the picker).
func (m dashModel) fetchChromsCmd(assembly string) tea.Cmd {
	base := m.base
	return func() tea.Msg {
		var cs []httpapi.ChromCoverage
		err := getJSON(base+"/genomes/"+assembly+"/chromosomes", &cs)
		return chromsMsg{assembly: assembly, chroms: cs, err: err}
	}
}

// downloadChromsCmd starts a download restricted to the named chromosomes.
func (m dashModel) downloadChromsCmd(assembly string, chroms []string) tea.Cmd {
	base := m.base
	return func() tea.Msg {
		body, status, err := postJSON(base+"/actions/download", map[string]any{"assembly": assembly, "chroms": chroms})
		if err != nil {
			return actionMsg{text: assembly, err: err}
		}
		if status != http.StatusNoContent && status != http.StatusOK {
			return actionMsg{text: assembly, err: fmt.Errorf("%s", strings.TrimSpace(string(body)))}
		}
		return actionMsg{text: fmt.Sprintf("downloading %d chrom(s) of %s", len(chroms), assembly)}
	}
}

// reconstructCmd asks the node to rebuild a genome's FASTA on its filesystem and
// reports the written path (or the verification/missing-segment error).
func (m dashModel) reconstructCmd(assembly string) tea.Cmd {
	base := m.base
	return func() tea.Msg {
		body, status, err := postJSON(base+"/actions/reconstruct", map[string]any{"assembly": assembly})
		if err != nil {
			return actionMsg{text: assembly, err: err}
		}
		if status != http.StatusOK {
			return actionMsg{text: assembly, err: fmt.Errorf("%s", strings.TrimSpace(string(body)))}
		}
		var res httpapi.ReconstructResult
		if json.Unmarshal(body, &res) == nil {
			return actionMsg{text: fmt.Sprintf("%s → %s (%d bp, verified)", assembly, res.Path, res.Bases)}
		}
		return actionMsg{text: "reconstructed " + assembly}
	}
}

// simpleActionCmd POSTs {assembly} to a node action path and reports the result.
func (m dashModel) simpleActionCmd(path, assembly, okText string) tea.Cmd {
	base := m.base
	return func() tea.Msg {
		body, status, err := postJSON(base+path, map[string]any{"assembly": assembly})
		if err != nil {
			return actionMsg{text: assembly, err: err}
		}
		if status != http.StatusNoContent && status != http.StatusOK {
			return actionMsg{text: assembly, err: fmt.Errorf("%s", strings.TrimSpace(string(body)))}
		}
		return actionMsg{text: okText}
	}
}

// applyCovCache overlays on-demand-fetched coverage onto discover entries whose
// coverage was deferred (big manifests), so it survives the 2s discover refresh.
func (m *dashModel) applyCovCache() {
	for i := range m.discover {
		if m.discover[i].CoverageKnown {
			continue
		}
		if c, ok := m.covCache[m.discover[i].Assembly]; ok {
			m.discover[i].Have, m.discover[i].Total, m.discover[i].CoverageKnown = c[0], c[1], true
		}
	}
}

// maybeCoverageCmd fetches exact coverage for the selected Discover genome when
// it was deferred (big manifest) and not yet cached.
func (m dashModel) maybeCoverageCmd() tea.Cmd {
	if m.active != tabDiscover {
		return nil
	}
	f := m.filteredDiscover()
	if m.cursor >= len(f) {
		return nil
	}
	e := f[m.cursor]
	if e.CoverageKnown {
		return nil
	}
	if _, cached := m.covCache[e.Assembly]; cached {
		return nil
	}
	return m.coverageCmd(e.Assembly)
}

// coverageCmd asks the node to compute exact coverage of one genome on demand.
func (m dashModel) coverageCmd(assembly string) tea.Cmd {
	base := m.base
	return func() tea.Msg {
		var e httpapi.DiscoverEntry
		if err := getJSON(base+"/coverage/"+assembly, &e); err != nil {
			return coverageMsg{assembly: assembly, have: 0, total: 0}
		}
		return coverageMsg{assembly: assembly, have: e.Have, total: e.Total}
	}
}

// dlFor returns the live download status for an assembly, or nil.
func (m dashModel) dlFor(assembly string) *httpapi.DownloadStatus {
	for i := range m.st.Downloads {
		if m.st.Downloads[i].Assembly == assembly {
			return &m.st.Downloads[i]
		}
	}
	return nil
}

// downloadCmd posts a download control (start/pause/resume/cancel) to the node.
// sub is "" for start, or "/pause"|"/resume"|"/cancel".
func (m dashModel) downloadCmd(assembly, sub, okText string) tea.Cmd {
	base := m.base
	return func() tea.Msg {
		body, status, err := postJSON(base+"/actions/download"+sub, map[string]any{"assembly": assembly})
		if err != nil {
			return actionMsg{text: assembly, err: err}
		}
		if status != http.StatusNoContent && status != http.StatusOK {
			return actionMsg{text: assembly, err: fmt.Errorf("%s", strings.TrimSpace(string(body)))}
		}
		return actionMsg{text: okText}
	}
}

// caret renders the selection marker for row i on the active tab.
func (m dashModel) caret(i int) string {
	if i == m.cursor && m.activeRows() > 0 {
		return lipgloss.NewStyle().Foreground(colAccent).Bold(true).Render("▸ ")
	}
	return "  "
}

// isDelta returns true if the selected assembly is a delta genome (recipe or
// raw blob), consulting both the seeding and discover lists.
func (m dashModel) isDelta(assembly string) bool {
	for _, s := range m.st.Seeding {
		if s.Assembly == assembly {
			return s.Kind == "delta"
		}
	}
	for _, e := range m.discover {
		if e.Assembly == assembly {
			return e.Kind == "delta"
		}
	}
	return false
}

// padBetween left-justifies left and right-justifies right across width.
func padBetween(left, right string, width int) string {
	lw := lipgloss.Width(left)
	rw := lipgloss.Width(right)
	gap := width - lw - rw
	if gap < 1 {
		gap = 1
	}
	return left + strings.Repeat(" ", gap) + right
}

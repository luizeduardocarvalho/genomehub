package cmd

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/luizeduardocarvalho/genomehub/internal/httpapi"
)

func key(s string) tea.KeyMsg {
	switch s {
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	default:
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}
}

func send(m dashModel, msg tea.Msg) dashModel {
	nm, _ := m.Update(msg)
	return nm.(dashModel)
}

func sized() dashModel {
	m := newDashModel("http://node", "http://tracker", "node", time.Second)
	return send(m, tea.WindowSizeMsg{Width: 100, Height: 30})
}

func sampleStatus() httpapi.Status {
	return httpapi.Status{
		UptimeSeconds: 42, SegmentsHeld: 5, Requests: 9, BytesServed: 1000,
		Seeding: []httpapi.Seeding{
			{Assembly: "ATHENA", Organism: "Athena ficticia", Kind: "manifest", Have: 5, Total: 5},
			{Assembly: "ATHENA2", Organism: "Athena ficticia", Kind: "manifest", Have: 3, Total: 5},
		},
		Served:    []httpapi.ServeHit{{Path: "/segments/abc", Assembly: "ATHENA", Bytes: 100, Status: 200, AgoSec: 2}},
		Downloads: []httpapi.DownloadStatus{{Assembly: "ATHENA2", State: "running", Have: 3, Total: 5, Bytes: 200}},
	}
}

func sampleDiscover() []httpapi.DiscoverEntry {
	return []httpapi.DiscoverEntry{
		{RegistryEntry: httpapi.RegistryEntry{Assembly: "ATHENA", Organism: "Athena ficticia", Version: 1, Segments: 5}, Have: 5, Total: 5, Local: true, Sources: 3},
		{RegistryEntry: httpapi.RegistryEntry{Assembly: "ECOLI", Organism: "Escherichia coli", Version: 1, Segments: 1}, Have: 0, Total: 1, Sources: 2},
	}
}

func TestViewLoadsThenRenders(t *testing.T) {
	m := newDashModel("http://node", "", "node", time.Second)
	if !strings.Contains(m.View(), "loading") {
		t.Fatal("zero-size model should show loading")
	}
	m = sized()
	m = send(m, statusMsg{st: sampleStatus()})
	m = send(m, discoverMsg{entries: sampleDiscover()})
	if v := m.View(); !strings.Contains(v, "GenomeHub") {
		t.Fatalf("missing header: %q", v)
	}
}

func TestEveryTabRenders(t *testing.T) {
	m := sized()
	m = send(m, statusMsg{st: sampleStatus()})
	m = send(m, discoverMsg{entries: sampleDiscover()})
	for tabN := tab(0); tabN < numTabs; tabN++ {
		m.active = tabN
		if v := m.View(); strings.TrimSpace(v) == "" {
			t.Fatalf("tab %d rendered empty", tabN)
		}
	}
}

func TestTabSwitchAndJump(t *testing.T) {
	m := sized()
	m = send(m, key("3"))
	if m.active != tabDiscover {
		t.Fatalf("key 3 -> %d want Discover", m.active)
	}
	m = send(m, key("tab"))
	if m.active != tabServing {
		t.Fatalf("tab after Discover -> %d want Serving", m.active)
	}
}

func TestSearchModeRoutesText(t *testing.T) {
	m := sized()
	m = send(m, statusMsg{st: sampleStatus()})
	m = send(m, key("2")) // Seeding
	m = send(m, key("/"))
	if !m.searching {
		t.Fatal("/ should enter search")
	}
	for _, r := range "ath" {
		m = send(m, key(string(r)))
	}
	if got := m.search.Value(); got != "ath" {
		t.Fatalf("search value = %q want ath", got)
	}
	if n := len(m.filteredSeeding()); n != 2 { // both contain "ath" (ATHENA, ATHENA2)
		t.Fatalf("filtered = %d want 2", n)
	}
	m = send(m, key("esc"))
	if m.searching || m.search.Value() != "" {
		t.Fatal("esc should exit + clear search")
	}
}

func TestConfirmGuard(t *testing.T) {
	m := sized()
	m = send(m, discoverMsg{entries: sampleDiscover()})
	m.active = tabDiscover
	m = send(m, key("X")) // delete selected -> arms confirm
	if !strings.HasPrefix(m.confirm, "delete:") {
		t.Fatalf("X should arm delete confirm, got %q", m.confirm)
	}
	m = send(m, key("n")) // anything but y cancels
	if m.confirm != "" {
		t.Fatal("n should clear pending confirm")
	}
}

func TestDeferredCoverageMerge(t *testing.T) {
	m := sized()
	// A big genome whose coverage was deferred by the node.
	m = send(m, discoverMsg{entries: []httpapi.DiscoverEntry{
		{RegistryEntry: httpapi.RegistryEntry{Assembly: "CVI0", Segments: 3_000_000}, Total: 3_000_000, CoverageKnown: false},
	}})
	if m.discover[0].CoverageKnown {
		t.Fatal("expected deferred coverage")
	}
	// On-demand coverage arrives → merged.
	m = send(m, coverageMsg{assembly: "CVI0", have: 1_500_000, total: 3_000_000})
	if !m.discover[0].CoverageKnown || m.discover[0].Have != 1_500_000 {
		t.Fatalf("coverage not merged: %+v", m.discover[0])
	}
	// A fresh discover poll (still deferred) must not wipe the cached coverage.
	m = send(m, discoverMsg{entries: []httpapi.DiscoverEntry{
		{RegistryEntry: httpapi.RegistryEntry{Assembly: "CVI0", Segments: 3_000_000}, Total: 3_000_000, CoverageKnown: false},
	}})
	if !m.discover[0].CoverageKnown || m.discover[0].Have != 1_500_000 {
		t.Fatalf("cached coverage lost on refresh: %+v", m.discover[0])
	}
}

func TestChromPicker(t *testing.T) {
	m := sized()
	m = send(m, chromsMsg{assembly: "ATHENA2", chroms: []httpapi.ChromCoverage{
		{Name: "chr1", Segments: 1, Have: 1},
		{Name: "chr4", Segments: 1, Have: 0},
	}})
	if !m.picking {
		t.Fatal("chromsMsg should open picker")
	}
	// default selection: chromosomes not fully held (chr4) preselected, chr1 not.
	if m.pickSel["chr1"] || !m.pickSel["chr4"] {
		t.Fatalf("default selection wrong: %+v", m.pickSel)
	}
	m = send(m, key(" ")) // toggle chr1 (cursor at 0)
	if !m.pickSel["chr1"] {
		t.Fatal("space should toggle chr1 on")
	}
	if v := m.View(); !strings.Contains(v, "Chromosomes") {
		t.Fatalf("picker view missing title: %q", v)
	}
	m = send(m, key("esc"))
	if m.picking {
		t.Fatal("esc should close picker")
	}
}

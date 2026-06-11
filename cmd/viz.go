package cmd

import (
	"fmt"
	"strings"

	"github.com/luizeduardocarvalho/genomehub/internal/index"
	"github.com/luizeduardocarvalho/genomehub/internal/manifest"
	"github.com/spf13/cobra"
)

const barWidth = 60

const (
	colorReset   = "\033[0m"
	colorCore    = "\033[42m\033[30m" // green  — shared by all
	colorPartial = "\033[43m\033[30m" // yellow — shared by subset
)

var uniqueColors = []string{
	"\033[41m\033[97m", // red
	"\033[44m\033[97m", // blue
	"\033[45m\033[97m", // magenta
	"\033[46m\033[30m", // cyan
	"\033[101m\033[30m", // bright red
}

var (
	vizVerbose bool
	vizLimit   int
)

var vizCmd = &cobra.Command{
	Use:   "viz [manifest...]",
	Short: "Visualize segment layout and deduplication across genomes",
	Args:  cobra.ArbitraryArgs,
	RunE:  runViz,
}

func init() {
	vizCmd.Flags().BoolVar(&vizVerbose, "verbose", false, "print per-segment table")
	vizCmd.Flags().IntVar(&vizLimit, "limit", 200, "max rows in verbose segment table")
	rootCmd.AddCommand(vizCmd)
}

func runViz(_ *cobra.Command, args []string) error {
	idx, err := index.Open(indexDir())
	if err != nil {
		return fmt.Errorf("open index: %w", err)
	}
	defer idx.Close()

	// ── Bar chart (only when manifests are provided) ──────────
	if len(args) > 0 {
		if err := printBarChart(idx, args); err != nil {
			return err
		}
	}

	// ── 3-tier summary from index ─────────────────────────────
	stats, err := idx.Summary()
	if err != nil {
		return fmt.Errorf("index summary: %w", err)
	}
	printSummary(stats)

	// ── Verbose segment table ─────────────────────────────────
	if vizVerbose && len(args) > 0 {
		if err := printSegmentTable(idx, args, vizLimit); err != nil {
			return err
		}
	}

	return nil
}

// printBarChart renders a per-genome colored segment bar for each manifest.
func printBarChart(idx *index.Index, manifestPaths []string) error {
	type genomeEntry struct {
		name     string
		segments []manifest.Segment
		color    string
	}

	genomes := make([]genomeEntry, 0, len(manifestPaths))
	for i, path := range manifestPaths {
		m, err := manifest.Read(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		var segs []manifest.Segment
		for _, c := range m.Chromosomes {
			segs = append(segs, c.Segments...)
		}
		name := m.Assembly
		if name == "" {
			name = path
		}
		genomes = append(genomes, genomeEntry{
			name:     name,
			segments: segs,
			color:    uniqueColors[i%len(uniqueColors)],
		})
	}

	// Build refCount from manifests passed — bar colors reflect these N genomes.
	refCount := map[string]int{}
	for _, g := range genomes {
		seen := map[string]bool{}
		for _, s := range g.segments {
			if !seen[s.Hash] {
				refCount[s.Hash]++
				seen[s.Hash] = true
			}
		}
	}
	n := len(genomes)

	maxBases := 0
	for _, g := range genomes {
		total := 0
		for _, s := range g.segments {
			total += s.Length
		}
		if total > maxBases {
			maxBases = total
		}
	}
	labelW := 0
	for _, g := range genomes {
		if len(g.name) > labelW {
			labelW = len(g.name)
		}
	}

	fmt.Println()
	for _, g := range genomes {
		total := 0
		bar := ""
		for _, seg := range g.segments {
			total += seg.Length
			w := max(1, barWidth*seg.Length/maxBases)
			rc := refCount[seg.Hash]
			var color, lbl string
			switch {
			case rc == n:
				color, lbl = colorCore, "COR"
			case rc > 1:
				color, lbl = colorPartial, "PAR"
			default:
				color, lbl = g.color, "UNQ"
			}
			bar += color + pad(lbl, w) + colorReset
		}
		fmt.Printf("  %-*s [%s] %s\n", labelW, g.name, bar, fmtBP(total))
	}

	fmt.Println()
	fmt.Printf("  %s COR %s  core    — shared by all %d\n", colorCore, colorReset, n)
	fmt.Printf("  %s PAR %s  partial — shared by subset\n", colorPartial, colorReset)
	for i, g := range genomes {
		fmt.Printf("  %s UNQ %s  unique  — %s only\n", uniqueColors[i%len(uniqueColors)], colorReset, g.name)
	}
	return nil
}

// printSummary renders the 3-tier summary from index.Stats.
func printSummary(s index.Stats) {
	n := len(s.Genomes)
	fmt.Println()
	if n == 0 {
		fmt.Println("  index is empty — run import or reindex first")
		return
	}
	fmt.Printf("  %d genome(s) in index: %s\n\n", n, strings.Join(s.Genomes, ", "))

	total := s.StoredBytes
	if total == 0 {
		return
	}

	printTier := func(label string, bytes int64) {
		pct := float64(bytes) / float64(total)
		filled := int(pct * barWidth)
		bar := strings.Repeat("█", filled) + strings.Repeat("░", barWidth-filled)
		fmt.Printf("  %-36s %9s  [%s]  %4.1f%%\n", label, fmtBytes(bytes), bar[:barWidth/3], pct*100)
	}

	printTier(fmt.Sprintf("core genome  (shared by all %d):", n), s.CoreBytes)
	printTier("partial      (shared by subset):", s.PartialBytes)
	printTier("unique       (genome-specific):", s.UniqueBytes)

	if len(s.UniquePerGenome) > 0 {
		fmt.Println()
		for _, g := range s.Genomes {
			fmt.Printf("    %-20s unique: %s\n", g, fmtBytes(s.UniquePerGenome[g]))
		}
	}

	saved := s.NaiveBytes - s.StoredBytes
	var savedPct float64
	if s.NaiveBytes > 0 {
		savedPct = float64(saved) / float64(s.NaiveBytes) * 100
	}
	fmt.Println()
	fmt.Printf("  total stored: %s  |  naive: %s  |  saved: %s (%.0f%%)\n",
		fmtBytes(s.StoredBytes), fmtBytes(s.NaiveBytes), fmtBytes(saved), savedPct)
	fmt.Println()
}

// printSegmentTable prints a per-segment detail table (--verbose).
func printSegmentTable(idx *index.Index, manifestPaths []string, limit int) error {
	type row struct {
		hash string
		size int64
		refs []string
	}

	seen := map[string]bool{}
	var rows []row
	for _, path := range manifestPaths {
		m, err := manifest.Read(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		for _, c := range m.Chromosomes {
			for _, seg := range c.Segments {
				if seen[seg.Hash] {
					continue
				}
				seen[seg.Hash] = true
				hash := strings.TrimPrefix(seg.Hash, "blake3:")
				refs, err := idx.ReferencedBy(hash)
				if err != nil {
					refs = []string{"?"}
				}
				rows = append(rows, row{seg.Hash, int64(seg.Length), refs})
				if len(rows) >= limit {
					break
				}
			}
			if len(rows) >= limit {
				break
			}
		}
		if len(rows) >= limit {
			break
		}
	}

	fmt.Printf("\n  %-54s  %9s  %s\n", "HASH", "SIZE", "REFERENCED BY")
	fmt.Printf("  %s\n", strings.Repeat("─", 90))
	for _, r := range rows {
		fmt.Printf("  %-54s  %9s  %s\n", r.hash, fmtBytes(r.size), strings.Join(r.refs, ", "))
	}
	if len(rows) == limit {
		fmt.Printf("  ... (use --limit to show more)\n")
	}
	fmt.Println()
	return nil
}

func pad(s string, width int) string {
	for len(s) < width {
		s += " "
	}
	return s[:width]
}

func fmtBytes(b int64) string {
	switch {
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(b)/float64(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(b)/float64(1<<10))
	default:
		return fmt.Sprintf("%d B", b)
	}
}

func fmtBP(b int) string {
	switch {
	case b >= 1_000_000:
		return fmt.Sprintf("%.1f Mbp", float64(b)/1_000_000)
	case b >= 1_000:
		return fmt.Sprintf("%.1f Kbp", float64(b)/1_000)
	default:
		return fmt.Sprintf("%d bp", b)
	}
}

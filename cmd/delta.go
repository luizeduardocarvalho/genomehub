package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/luizcarvalho/genome-hub/internal/aligner"
	"github.com/luizcarvalho/genome-hub/internal/delta"
	"github.com/luizcarvalho/genome-hub/internal/fasta"
	"github.com/spf13/cobra"
)

var (
	deltaReference   string
	deltaQuery       string
	deltaPreset      string
	deltaThreads     int
	deltaMinLen      int
	deltaOutput      string
	deltaJSON        bool
	deltaRefManifest string

	rdReference   string
	rdRefManifest string
	rdDelta       string
	rdOutput      string
)

var deltaCmd = &cobra.Command{
	Use:   "delta",
	Short: "Encode a near-identical genome as a patch against a reference (reference + diffs)",
	Long: `Aligns a query genome against a reference and stores only the differences:
shared sequence becomes copy(reference[a:b]) ops, divergent/novel sequence becomes
literals. Best for same-species accessions (similarity > ~0.95), where segment
dedup wastes space on millions of tiny SNP-split segments.
See docs/adr/0001-delta-vs-segment-dedup-routing.md.

Example:
  genomehub delta \
    --reference TAIR10:tmp/tair10.fa \
    --query Ler0:tmp/ler0.fna \
    --preset asm5 \
    --output Ler0.delta.json`,
	RunE: runDelta,
}

var reconstructDeltaCmd = &cobra.Command{
	Use:   "reconstruct-delta",
	Short: "Rebuild a genome FASTA from a delta and its reference",
	Long: `Applies a delta patch against the reference FASTA to reconstruct the query
genome, verifying each chromosome's hash. Fails if the reference does not match
the one the delta was built against.

Example:
  genomehub reconstruct-delta \
    --reference TAIR10:tmp/tair10.fa \
    --delta Ler0.delta.json \
    --output Ler0.reconstructed.fa`,
	RunE: runReconstructDelta,
}

func init() {
	deltaCmd.Flags().StringVar(&deltaReference, "reference", "", "reference genome as name:path (required)")
	deltaCmd.Flags().StringVar(&deltaQuery, "query", "", "query genome as name:path (required)")
	deltaCmd.Flags().StringVar(&deltaPreset, "preset", "asm5", "minimap2 preset (asm5 = same species)")
	deltaCmd.Flags().IntVar(&deltaThreads, "threads", 4, "minimap2 threads")
	deltaCmd.Flags().IntVar(&deltaMinLen, "min-len", 100, "minimum alignment block length to use")
	deltaCmd.Flags().StringVar(&deltaOutput, "output", "", "output delta path (default <query>.delta.ghd, or .json with --json)")
	deltaCmd.Flags().BoolVar(&deltaJSON, "json", false, "write verbose JSON instead of the compact binary format (debug)")
	deltaCmd.Flags().StringVar(&deltaRefManifest, "reference-manifest", "", "optional: reference manifest, recorded so the delta reconstructs from the store")
	deltaCmd.MarkFlagRequired("reference")
	deltaCmd.MarkFlagRequired("query")
	rootCmd.AddCommand(deltaCmd)

	reconstructDeltaCmd.Flags().StringVar(&rdReference, "reference", "", "reference genome FASTA as name:path or path")
	reconstructDeltaCmd.Flags().StringVar(&rdRefManifest, "reference-manifest", "", "reference manifest to rebuild the reference from the store (alternative to --reference)")
	reconstructDeltaCmd.Flags().StringVar(&rdDelta, "delta", "", "delta path (required)")
	reconstructDeltaCmd.Flags().StringVar(&rdOutput, "output", "", "output FASTA path (required)")
	reconstructDeltaCmd.MarkFlagRequired("delta")
	reconstructDeltaCmd.MarkFlagRequired("output")
	rootCmd.AddCommand(reconstructDeltaCmd)
}

func runDelta(_ *cobra.Command, _ []string) error {
	refName, refPath, err := parseNamePath(deltaReference)
	if err != nil {
		return fmt.Errorf("--reference: %w", err)
	}
	qName, qPath, err := parseNamePath(deltaQuery)
	if err != nil {
		return fmt.Errorf("--query: %w", err)
	}
	out := deltaOutput
	if out == "" {
		if deltaJSON {
			out = qName + ".delta.json"
		} else {
			out = qName + ".delta.ghd"
		}
	}

	fmt.Fprintln(os.Stderr, "loading FASTAs...")
	refChroms, err := fasta.Read(refPath)
	if err != nil {
		return fmt.Errorf("read reference %s: %w", refName, err)
	}
	queryChroms, err := fasta.Read(qPath)
	if err != nil {
		return fmt.Errorf("read query %s: %w", qName, err)
	}

	cfg := aligner.Config{
		MinLength:   deltaMinLen,
		MinIdentity: 0.0,
		Preset:      deltaPreset,
		Threads:     deltaThreads,
		WithCIGAR:   true,
	}
	fmt.Fprintf(os.Stderr, "aligning %s (query) against %s (reference)...\n", qName, refName)
	blocks, err := alignCached(refPath, qPath, cfg, map[string]string{})
	if err != nil {
		return fmt.Errorf("align: %w", err)
	}

	fmt.Fprintln(os.Stderr, "building delta...")
	d := delta.Build(qName, refName, refChroms, queryChroms, blocks)
	d.RefManifest = deltaRefManifest
	if deltaJSON {
		if err := d.WriteJSON(out); err != nil {
			return err
		}
	} else {
		if err := d.Write(out); err != nil {
			return err
		}
	}

	// ── Summary ───────────────────────────────────────────────
	copyBases := d.TotalBases - d.LiteralBases
	savedPct := 0.0
	if d.TotalBases > 0 {
		savedPct = float64(copyBases) / float64(d.TotalBases) * 100
	}
	var ops int
	for _, c := range d.Chromosomes {
		ops += len(c.Ops)
	}
	deltaSize := fileSize(out)

	fmt.Printf("\n%s vs reference %s\n", qName, refName)
	fmt.Printf("  query bases:    %s\n", fmtBP(d.TotalBases))
	fmt.Printf("  copied (ref):   %s  (%.2f%%)\n", fmtBP(copyBases), savedPct)
	fmt.Printf("  literal (new):  %s  (%.2f%%)\n", fmtBP(d.LiteralBases), 100-savedPct)
	fmt.Printf("  ops:            %d\n", ops)
	fmt.Printf("  delta file:     %s  (vs %s raw query ≈ %.1fx smaller)\n",
		fmtBytesInt(deltaSize), fmtBP(d.TotalBases), ratio(d.TotalBases, deltaSize))
	fmt.Printf("\ndelta: %s\n", out)
	return nil
}

func runReconstructDelta(_ *cobra.Command, _ []string) error {
	d, err := delta.Read(rdDelta)
	if err != nil {
		return fmt.Errorf("read delta: %w", err)
	}

	// Resolve the reference, in priority order:
	//   1. explicit --reference FASTA
	//   2. explicit --reference-manifest (reconstruct from store)
	//   3. the manifest recorded in the delta itself (reconstruct from store)
	refManifest := rdRefManifest
	if refManifest == "" && rdReference == "" {
		refManifest = d.RefManifest
	}

	var refChroms []fasta.Chromosome
	switch {
	case rdReference != "":
		refPath := rdReference
		if _, p, err := parseNamePath(rdReference); err == nil {
			refPath = p
		}
		refChroms, err = fasta.Read(refPath)
		if err != nil {
			return fmt.Errorf("read reference FASTA: %w", err)
		}
	case refManifest != "":
		fmt.Fprintf(os.Stderr, "rebuilding reference from store via %s...\n", refManifest)
		refChroms, err = reconstructFromManifest(refManifest, false)
		if err != nil {
			return fmt.Errorf("rebuild reference from store: %w", err)
		}
	default:
		return fmt.Errorf("no reference: pass --reference, --reference-manifest, or use a delta that records ref_manifest")
	}

	fmt.Fprintf(os.Stderr, "reconstructing %s from delta...\n", d.Assembly)
	chroms, err := delta.Apply(d, refChroms)
	if err != nil {
		return err
	}
	if err := fasta.Write(rdOutput, chroms); err != nil {
		return err
	}
	fmt.Printf("reconstructed %d chromosomes, hashes verified → %s\n", len(chroms), rdOutput)
	return nil
}

// parseNamePath splits "name:path" into its parts.
func parseNamePath(s string) (name, path string, err error) {
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("expected name:path, got %q", s)
	}
	return parts[0], parts[1], nil
}

func fileSize(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}

func fmtBytesInt(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

func ratio(bases int, size int64) float64 {
	if size == 0 {
		return 0
	}
	return float64(bases) / float64(size)
}

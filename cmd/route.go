package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/luizeduardocarvalho/genomehub/internal/fasta"
	"github.com/luizeduardocarvalho/genomehub/internal/sketch"
	"github.com/spf13/cobra"
)

var (
	sketchFasta    string
	sketchAssembly string
	sketchOutput   string
	sketchKmer     int
	sketchSize     int

	simA string
	simB string

	routeQuery    string
	routeCands    []string
	routeKmer     int
	routeSize     int
	routeDeltaThr float64
	routeSkipThr  float64
	routePreset   string
	routeExecute  bool

	similarAssembly string
	similarTop      int
)

var sketchCmd = &cobra.Command{
	Use:   "sketch",
	Short: "Compute a MinHash sketch (genome fingerprint) for a FASTA",
	RunE:  runSketch,
}

var similarityCmd = &cobra.Command{
	Use:   "similarity",
	Short: "Estimate similarity between two genomes from their MinHash sketches",
	Long:  "Computes sketches for two FASTAs and prints their Jaccard and approximate sequence identity (ANI).",
	RunE:  runSimilarity,
}

var routeCmd = &cobra.Command{
	Use:   "route",
	Short: "Pick the storage strategy (delta / reindex / skip) for a genome by similarity",
	Long: `Sketches the query and each candidate genome, finds the most similar candidate,
and recommends a storage path per docs/adr/0001-delta-vs-segment-dedup-routing.md:

  similarity > delta-threshold (default 0.95) → delta against the best candidate
  skip-threshold..delta-threshold              → segment reindex (gear+MEM)
  < skip-threshold (default 0.50)              → skip (too diverged to share)

With --execute, the recommended command is run.

Example:
  genomehub route \
    --query Cvi0:tmp/cvi0.fna \
    --candidate TAIR10:tmp/tair10.fa \
    --candidate Ler0:tmp/ler0.fna \
    --execute`,
	RunE: runRoute,
}

func init() {
	sketchCmd.Flags().StringVar(&sketchFasta, "fasta", "", "input FASTA (required)")
	sketchCmd.Flags().StringVar(&sketchAssembly, "assembly", "", "assembly name (default: file name)")
	sketchCmd.Flags().StringVar(&sketchOutput, "output", "", "output sketch JSON (default <assembly>.sketch.json)")
	sketchCmd.Flags().IntVar(&sketchKmer, "kmer", sketch.DefaultKmer, "k-mer length")
	sketchCmd.Flags().IntVar(&sketchSize, "size", sketch.DefaultSize, "number of bottom hashes")
	sketchCmd.MarkFlagRequired("fasta")
	rootCmd.AddCommand(sketchCmd)

	similarityCmd.Flags().StringVar(&simA, "a", "", "first FASTA (required)")
	similarityCmd.Flags().StringVar(&simB, "b", "", "second FASTA (required)")
	similarityCmd.MarkFlagRequired("a")
	similarityCmd.MarkFlagRequired("b")
	rootCmd.AddCommand(similarityCmd)

	routeCmd.Flags().StringVar(&routeQuery, "query", "", "query genome as name:path (required)")
	routeCmd.Flags().StringArrayVar(&routeCands, "candidate", nil, "candidate reference as name:path, repeatable (required)")
	routeCmd.Flags().IntVar(&routeKmer, "kmer", sketch.DefaultKmer, "k-mer length")
	routeCmd.Flags().IntVar(&routeSize, "size", sketch.DefaultSize, "sketch size")
	routeCmd.Flags().Float64Var(&routeDeltaThr, "delta-threshold", 0.95, "similarity above which to use delta")
	routeCmd.Flags().Float64Var(&routeSkipThr, "skip-threshold", 0.50, "similarity below which to skip")
	routeCmd.Flags().StringVar(&routePreset, "preset", "asm5", "minimap2 preset for the delta path")
	routeCmd.Flags().BoolVar(&routeExecute, "execute", false, "run the recommended command")
	routeCmd.MarkFlagRequired("query")
	routeCmd.MarkFlagRequired("candidate")
	rootCmd.AddCommand(routeCmd)

	similarCmd.Flags().StringVar(&similarAssembly, "assembly", "", "assembly to compare against all stored sketches (required)")
	similarCmd.Flags().IntVar(&similarTop, "top", 10, "show the N most similar genomes")
	similarCmd.MarkFlagRequired("assembly")
	rootCmd.AddCommand(similarCmd)
}

var similarCmd = &cobra.Command{
	Use:   "similar",
	Short: "Rank stored genomes by similarity to an assembly (uses persisted sketches)",
	Long: `Compares one assembly's sketch against every sketch persisted on import
(under <store>/../sketches) and lists the most similar genomes — a computational
nearest-neighbour query, no alignment. Answers "what are the N most similar
genomes to mine?".`,
	RunE: runSimilar,
}

func runSimilar(_ *cobra.Command, _ []string) error {
	dir := sketchDir()
	target, err := sketch.Read(filepath.Join(dir, similarAssembly+".sketch.json"))
	if err != nil {
		return fmt.Errorf("no stored sketch for %s (import it first): %w", similarAssembly, err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read sketch dir: %w", err)
	}
	var scores []candScore
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sketch.json") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".sketch.json")
		if name == similarAssembly {
			continue
		}
		s, err := sketch.Read(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		scores = append(scores, candScore{name: name, sim: sketch.Similarity(target, s)})
	}
	if len(scores) == 0 {
		return fmt.Errorf("no other sketches found in %s", dir)
	}
	sort.Slice(scores, func(i, j int) bool { return scores[i].sim > scores[j].sim })

	fmt.Printf("most similar to %s:\n", similarAssembly)
	for i, s := range scores {
		if i >= similarTop {
			break
		}
		fmt.Printf("  %-16s %.4f\n", s.name, s.sim)
	}
	return nil
}

// sketchFASTA reads a FASTA and computes its sketch.
func sketchFASTA(assembly, path string, kmer, size int) (sketch.Sketch, error) {
	chroms, err := fasta.Read(path)
	if err != nil {
		return sketch.Sketch{}, err
	}
	seqs := make([][]byte, len(chroms))
	for i, c := range chroms {
		seqs[i] = c.Sequence
	}
	return sketch.Compute(assembly, seqs, kmer, size), nil
}

// loadOrComputeSketch returns a persisted sketch for assembly if one exists under
// sketchDir() (the fast path — routing becomes a lookup), otherwise computes it
// from the FASTA at path.
func loadOrComputeSketch(assembly, path string, kmer, size int) (sketch.Sketch, error) {
	if s, err := sketch.Read(filepath.Join(sketchDir(), assembly+".sketch.json")); err == nil {
		fmt.Fprintf(os.Stderr, "  using stored sketch for %s\n", assembly)
		return s, nil
	}
	fmt.Fprintf(os.Stderr, "  sketching %s...\n", assembly)
	return sketchFASTA(assembly, path, kmer, size)
}

func runSketch(_ *cobra.Command, _ []string) error {
	assembly := sketchAssembly
	if assembly == "" {
		assembly = sketchFasta
	}
	out := sketchOutput
	if out == "" {
		out = assembly + ".sketch.json"
	}
	s, err := sketchFASTA(assembly, sketchFasta, sketchKmer, sketchSize)
	if err != nil {
		return err
	}
	if err := s.Write(out); err != nil {
		return err
	}
	fmt.Printf("sketch: %s  (%d hashes, k=%d) → %s\n", assembly, len(s.Hashes), s.Kmer, out)
	return nil
}

func runSimilarity(_ *cobra.Command, _ []string) error {
	a, err := sketchFASTA("a", simA, sketch.DefaultKmer, sketch.DefaultSize)
	if err != nil {
		return fmt.Errorf("sketch %s: %w", simA, err)
	}
	b, err := sketchFASTA("b", simB, sketch.DefaultKmer, sketch.DefaultSize)
	if err != nil {
		return fmt.Errorf("sketch %s: %w", simB, err)
	}
	fmt.Printf("jaccard:    %.4f\n", sketch.Jaccard(a, b))
	fmt.Printf("similarity: %.4f  (approx. sequence identity)\n", sketch.Similarity(a, b))
	return nil
}

type candScore struct {
	name string
	path string
	sim  float64
}

func runRoute(_ *cobra.Command, _ []string) error {
	qName, qPath, err := parseNamePath(routeQuery)
	if err != nil {
		return fmt.Errorf("--query: %w", err)
	}
	qSketch, err := loadOrComputeSketch(qName, qPath, routeKmer, routeSize)
	if err != nil {
		return fmt.Errorf("sketch query: %w", err)
	}

	var scores []candScore
	for _, c := range routeCands {
		cName, cPath, err := parseNamePath(c)
		if err != nil {
			return fmt.Errorf("--candidate %q: %w", c, err)
		}
		cSketch, err := loadOrComputeSketch(cName, cPath, routeKmer, routeSize)
		if err != nil {
			return fmt.Errorf("sketch candidate %s: %w", cName, err)
		}
		scores = append(scores, candScore{cName, cPath, sketch.Similarity(qSketch, cSketch)})
	}
	sort.Slice(scores, func(i, j int) bool { return scores[i].sim > scores[j].sim })

	fmt.Printf("\nquery: %s\n", qName)
	fmt.Println("similarity to candidates:")
	for _, s := range scores {
		fmt.Printf("  %-14s %.4f\n", s.name, s.sim)
	}

	best := scores[0]
	var decision string
	var cmdArgs []string
	switch {
	case best.sim >= routeDeltaThr:
		decision = fmt.Sprintf("DELTA against %s (similarity %.4f ≥ %.2f)", best.name, best.sim, routeDeltaThr)
		cmdArgs = []string{"delta",
			"--reference", best.name + ":" + best.path,
			"--query", qName + ":" + qPath,
			"--preset", routePreset,
			"--output", qName + ".delta.ghd"}
	case best.sim >= routeSkipThr:
		decision = fmt.Sprintf("REINDEX with %s (similarity %.4f in [%.2f,%.2f))", best.name, best.sim, routeSkipThr, routeDeltaThr)
		cmdArgs = []string{"reindex",
			"--genome", best.name + ":" + best.path,
			"--genome", qName + ":" + qPath}
	default:
		decision = fmt.Sprintf("SKIP (best similarity %.4f < %.2f — too diverged to share)", best.sim, routeSkipThr)
	}

	fmt.Printf("\ndecision: %s\n", decision)
	if cmdArgs != nil {
		fmt.Printf("command:  genomehub %s\n", joinArgs(cmdArgs))
	}

	if routeExecute {
		if cmdArgs == nil {
			fmt.Fprintln(os.Stderr, "\nnothing to execute (skip).")
			return nil
		}
		fmt.Fprintf(os.Stderr, "\nexecuting...\n")
		c := exec.Command(os.Args[0], cmdArgs...)
		c.Stdout = os.Stdout
		c.Stderr = os.Stderr
		return c.Run()
	}
	return nil
}

func joinArgs(args []string) string {
	out := ""
	for i, a := range args {
		if i > 0 {
			out += " "
		}
		out += a
	}
	return out
}

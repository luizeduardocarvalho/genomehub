package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/luizeduardocarvalho/genomehub/internal/chunker"
	"github.com/luizeduardocarvalho/genomehub/internal/events"
	"github.com/luizeduardocarvalho/genomehub/internal/fasta"
	"github.com/luizeduardocarvalho/genomehub/internal/index"
	"github.com/luizeduardocarvalho/genomehub/internal/manifest"
	"github.com/luizeduardocarvalho/genomehub/internal/sketch"
	"github.com/luizeduardocarvalho/genomehub/internal/store"
	"github.com/spf13/cobra"
)

var importCmd = &cobra.Command{
	Use:   "import",
	Short: "Import a local FASTA file into the segment store",
	RunE:  runImport,
}

var (
	importFasta    string
	importOrganism string
	importAssembly string
	importOutput   string
	importMinChunk int
	importMaxChunk int
)

func init() {
	importCmd.Flags().StringVar(&importFasta, "fasta", "", "input FASTA file (required)")
	importCmd.Flags().StringVar(&importOrganism, "organism", "", "organism name")
	importCmd.Flags().StringVar(&importAssembly, "assembly", "", "assembly name (default: FASTA filename without extension)")
	importCmd.Flags().StringVar(&importOutput, "output", "", "output manifest path (default: <assembly>.manifest.json)")
	importCmd.Flags().IntVar(&importMinChunk, "min-chunk", chunker.DefaultMinSize, "minimum chunk size in bytes")
	importCmd.Flags().IntVar(&importMaxChunk, "max-chunk", chunker.DefaultMaxSize, "maximum chunk size in bytes")
	importCmd.MarkFlagRequired("fasta")
	rootCmd.AddCommand(importCmd)
}

func runImport(_ *cobra.Command, _ []string) error {
	s, err := store.Open(storeDir)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer s.Close()

	idx, err := index.Open(indexDir())
	if err != nil {
		return fmt.Errorf("open index: %w", err)
	}
	defer idx.Close()

	chromosomes, err := fasta.Read(importFasta)
	if err != nil {
		return fmt.Errorf("read fasta: %w", err)
	}

	assembly := importAssembly
	if assembly == "" {
		base := filepath.Base(importFasta)
		assembly = strings.TrimSuffix(base, filepath.Ext(base))
	}

	var manifestChroms []manifest.Chromosome
	totalBases := 0

	cfg := chunker.Config{MinSize: importMinChunk, MaxSize: importMaxChunk}

	for _, chrom := range chromosomes {
		fmt.Fprintf(os.Stderr, "chunking %s (%d bp)...\n", chrom.Name, len(chrom.Sequence))
		chunks := chunker.Split(chrom.Sequence, cfg)

		chromHash := "blake3:" + store.HashBytes(chrom.Sequence)
		var segments []manifest.Segment
		for _, chunk := range chunks {
			hash, err := s.Put(chunk)
			if err != nil {
				return fmt.Errorf("store segment: %w", err)
			}
			segments = append(segments, manifest.Segment{
				Hash:   "blake3:" + hash,
				Length: len(chunk),
			})
		}

		manifestChroms = append(manifestChroms, manifest.Chromosome{
			Name:     chrom.Name,
			Length:   len(chrom.Sequence),
			Hash:     chromHash,
			Segments: segments,
		})
		totalBases += len(chrom.Sequence)
	}

	m := &manifest.Manifest{
		Version:      1,
		GraphVersion: 1,
		Organism:     importOrganism,
		Assembly:     assembly,
		TotalBases:   totalBases,
		Encoding:     "raw-ascii",
		Chunking: manifest.Chunking{
			Algorithm: "gear",
			MinSize:   cfg.MinSize,
			MaxSize:   cfg.MaxSize,
		},
		CreatedAt:    time.Now().UTC(),
		SegmentsRoot: manifest.ComputeSegmentsRoot(manifestChroms),
		Chromosomes:  manifestChroms,
	}

	outPath := importOutput
	if outPath == "" {
		outPath = assembly + ".manifest.json"
	}
	if err := m.Write(outPath); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}

	totalSegments := 0
	for _, c := range manifestChroms {
		for _, seg := range c.Segments {
			totalSegments++
			hash := strings.TrimPrefix(seg.Hash, "blake3:")
			if err := idx.Put(hash, seg.Length, assembly); err != nil {
				return fmt.Errorf("index segment: %w", err)
			}
		}
	}
	// Persist a MinHash sketch so similarity/routing is a lookup, not a re-scan.
	if err := persistSketch(assembly, chromosomes); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not write sketch: %v\n", err)
	}

	events.Append(eventsPath(), events.Event{
		Op:       events.Import,
		Assembly: assembly,
		Bytes:    int64(totalBases),
		Segments: totalSegments,
	})

	fmt.Printf("imported %d chromosomes, %d bp, %d segments\n", len(manifestChroms), totalBases, totalSegments)
	fmt.Printf("manifest: %s\n", outPath)
	return nil
}

// persistSketch computes and stores a genome's MinHash sketch under sketchDir().
func persistSketch(assembly string, chroms []fasta.Chromosome) error {
	seqs := make([][]byte, len(chroms))
	for i, c := range chroms {
		seqs[i] = c.Sequence
	}
	s := sketch.Compute(assembly, seqs, sketch.DefaultKmer, sketch.DefaultSize)
	if err := os.MkdirAll(sketchDir(), 0o755); err != nil {
		return err
	}
	return s.Write(filepath.Join(sketchDir(), assembly+".sketch.json"))
}

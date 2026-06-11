package cmd

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/luizeduardocarvalho/genomehub/internal/aligner"
	"github.com/spf13/cobra"
)

var alignCmd = &cobra.Command{
	Use:   "align",
	Short: "Find shared regions between two assemblies using minimap2",
	RunE:  runAlign,
}

var (
	alignTarget   string
	alignQuery    string
	alignOutput   string
	alignMinLen   int
	alignMinIdent float64
	alignPreset   string
	alignThreads  int
)

func init() {
	alignCmd.Flags().StringVar(&alignTarget, "target", "", "target FASTA (required)")
	alignCmd.Flags().StringVar(&alignQuery, "query", "", "query FASTA (required)")
	alignCmd.Flags().StringVar(&alignOutput, "output", "", "write blocks to JSON file (default: print summary)")
	alignCmd.Flags().IntVar(&alignMinLen, "min-len", 1000, "minimum aligned block length in bp")
	alignCmd.Flags().Float64Var(&alignMinIdent, "min-identity", 0.90, "minimum alignment identity (0.0–1.0)")
	alignCmd.Flags().StringVar(&alignPreset, "preset", "asm20", "minimap2 preset: asm5 (same species) or asm20 (cross species)")
	alignCmd.Flags().IntVar(&alignThreads, "threads", 4, "minimap2 threads")
	alignCmd.MarkFlagRequired("target")
	alignCmd.MarkFlagRequired("query")
	rootCmd.AddCommand(alignCmd)
}

func runAlign(_ *cobra.Command, _ []string) error {
	cfg := aligner.Config{
		MinLength:   alignMinLen,
		MinIdentity: alignMinIdent,
		Preset:      alignPreset,
		Threads:     alignThreads,
	}

	fmt.Fprintf(os.Stderr, "aligning with minimap2 (preset=%s, threads=%d)...\n", cfg.Preset, cfg.Threads)

	blocks, err := aligner.Run(alignTarget, alignQuery, cfg)
	if err != nil {
		return err
	}

	stats := aligner.Summarise(blocks)
	fmt.Fprintf(os.Stderr, "blocks: %d  target span: %d bp  avg identity: %.2f%%\n",
		stats.Blocks, stats.TargetSpan, stats.AvgIdentity*100)

	if alignOutput != "" {
		data, err := json.MarshalIndent(blocks, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(alignOutput, data, 0644); err != nil {
			return err
		}
		fmt.Printf("wrote %d blocks to %s\n", len(blocks), alignOutput)
		return nil
	}

	// default: TSV to stdout — easy to pipe/grep
	fmt.Printf("query\tquery_start\tquery_end\ttarget\ttarget_start\ttarget_end\tidentity\tblock_len\n")
	for _, b := range blocks {
		fmt.Printf("%s\t%d\t%d\t%s\t%d\t%d\t%.2f%%\t%d\n",
			b.QueryName, b.QueryStart, b.QueryEnd,
			b.TargetName, b.TargetStart, b.TargetEnd,
			b.Identity*100, b.BlockLen)
	}
	return nil
}

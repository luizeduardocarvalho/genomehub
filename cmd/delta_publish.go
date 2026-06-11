package cmd

import (
	"fmt"
	"os"

	"github.com/luizeduardocarvalho/genomehub/internal/chunker"
	"github.com/luizeduardocarvalho/genomehub/internal/delta"
	"github.com/luizeduardocarvalho/genomehub/internal/store"
	"github.com/spf13/cobra"
)

var (
	publishDelta  string
	publishOutput string
)

var deltaPublishCmd = &cobra.Command{
	Use:   "delta-publish",
	Short: "Chunk a delta blob into the segment store and write a transfer recipe",
	Long: `Splits a delta (.ghd) into content-addressed chunks stored in the segment
store, and writes a recipe (ordered chunk hashes). A serving node then offers the
recipe; clients pull the chunks through the same peer-parallel /segments path as
genome segments, so deltas swarm too. See docs/adr/0002-content-addressed-blobs-and-trust.md.

Example:
  genomehub delta-publish --delta Ler0.delta.ghd --output Ler0.deltarecipe.json`,
	RunE: runDeltaPublish,
}

func init() {
	deltaPublishCmd.Flags().StringVar(&publishDelta, "delta", "", "delta blob to publish (required)")
	deltaPublishCmd.Flags().StringVar(&publishOutput, "output", "", "output recipe path (default <assembly>.deltarecipe.json)")
	deltaPublishCmd.MarkFlagRequired("delta")
	rootCmd.AddCommand(deltaPublishCmd)
}

func runDeltaPublish(_ *cobra.Command, _ []string) error {
	blob, err := os.ReadFile(publishDelta)
	if err != nil {
		return fmt.Errorf("read delta: %w", err)
	}
	d, err := delta.Read(publishDelta)
	if err != nil {
		return fmt.Errorf("parse delta: %w", err)
	}

	s, err := store.Open(storeDir)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer s.Close()

	cfg := chunker.Config{MinSize: chunker.DefaultMinSize, MaxSize: chunker.DefaultMaxSize}
	pieces := chunker.Split(blob, cfg)

	recipe := &delta.Recipe{Assembly: d.Assembly, Reference: d.Reference, TotalSize: len(blob)}
	for _, p := range pieces {
		hash, err := s.Put(p)
		if err != nil {
			return fmt.Errorf("store chunk: %w", err)
		}
		recipe.Chunks = append(recipe.Chunks, delta.Chunk{Hash: hash, Length: len(p)})
	}

	out := publishOutput
	if out == "" {
		out = d.Assembly + ".deltarecipe.json"
	}
	if err := recipe.Write(out); err != nil {
		return err
	}

	fmt.Printf("published %s delta: %d bytes → %d chunks\n", d.Assembly, len(blob), len(recipe.Chunks))
	fmt.Printf("recipe: %s\n", out)
	return nil
}

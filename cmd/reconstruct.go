package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/luizcarvalho/genome-hub/internal/fasta"
	"github.com/luizcarvalho/genome-hub/internal/manifest"
	"github.com/luizcarvalho/genome-hub/internal/store"
	"github.com/spf13/cobra"
)

var reconstructCmd = &cobra.Command{
	Use:   "reconstruct",
	Short: "Reconstruct a FASTA from a manifest and the local segment store",
	RunE:  runReconstruct,
}

var (
	reconstructManifest string
	reconstructOutput   string
)

func init() {
	reconstructCmd.Flags().StringVar(&reconstructManifest, "manifest", "", "manifest JSON file (required)")
	reconstructCmd.Flags().StringVar(&reconstructOutput, "output", "", "output FASTA file (required)")
	reconstructCmd.MarkFlagRequired("manifest")
	reconstructCmd.MarkFlagRequired("output")
	rootCmd.AddCommand(reconstructCmd)
}

func runReconstruct(_ *cobra.Command, _ []string) error {
	chroms, err := reconstructFromManifest(reconstructManifest, true)
	if err != nil {
		return err
	}
	if err := fasta.Write(reconstructOutput, chroms); err != nil {
		return fmt.Errorf("write fasta: %w", err)
	}
	fmt.Printf("wrote %s\n", reconstructOutput)
	return nil
}

// reconstructFromManifest rebuilds chromosomes from a manifest using the local
// segment store, verifying each chromosome's integrity hash. Shared by
// `reconstruct` and the delta reference-from-store path.
func reconstructFromManifest(manifestPath string, verbose bool) ([]fasta.Chromosome, error) {
	m, err := manifest.Read(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}

	s, err := store.Open(storeDir)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	defer s.Close()

	var chroms []fasta.Chromosome
	for _, c := range m.Chromosomes {
		seq := make([]byte, 0, c.Length)
		for _, seg := range c.Segments {
			hash := strings.TrimPrefix(seg.Hash, "blake3:")
			data, err := s.Get(hash)
			if err != nil {
				return nil, fmt.Errorf("fetch segment %s: %w", seg.Hash, err)
			}
			seq = append(seq, data...)
		}

		got := "blake3:" + store.HashBytes(seq)
		if got != c.Hash {
			return nil, fmt.Errorf("chromosome %s: integrity check failed\n  got  %s\n  want %s", c.Name, got, c.Hash)
		}

		chroms = append(chroms, fasta.Chromosome{Name: c.Name, Sequence: seq})
		if verbose {
			fmt.Fprintf(os.Stderr, "reconstructed %s (%d bp)\n", c.Name, len(seq))
		}
	}
	return chroms, nil
}

package cmd

import (
	"bytes"
	"fmt"

	"github.com/luizeduardocarvalho/genomehub/internal/fasta"
	"github.com/spf13/cobra"
)

var verifyCmd = &cobra.Command{
	Use:   "verify",
	Short: "Verify two FASTA files have identical sequences",
	RunE:  runVerify,
}

var (
	verifyOriginal      string
	verifyReconstructed string
)

func init() {
	verifyCmd.Flags().StringVar(&verifyOriginal, "original", "", "original FASTA (required)")
	verifyCmd.Flags().StringVar(&verifyReconstructed, "reconstructed", "", "reconstructed FASTA (required)")
	verifyCmd.MarkFlagRequired("original")
	verifyCmd.MarkFlagRequired("reconstructed")
	rootCmd.AddCommand(verifyCmd)
}

func runVerify(_ *cobra.Command, _ []string) error {
	orig, err := fasta.Read(verifyOriginal)
	if err != nil {
		return fmt.Errorf("read original: %w", err)
	}
	recon, err := fasta.Read(verifyReconstructed)
	if err != nil {
		return fmt.Errorf("read reconstructed: %w", err)
	}

	if len(orig) != len(recon) {
		return fmt.Errorf("chromosome count mismatch: original=%d reconstructed=%d", len(orig), len(recon))
	}

	allOK := true
	for i := range orig {
		if orig[i].Name != recon[i].Name {
			fmt.Printf("FAIL  chr %d: name mismatch (%q vs %q)\n", i, orig[i].Name, recon[i].Name)
			allOK = false
			continue
		}
		if !bytes.Equal(orig[i].Sequence, recon[i].Sequence) {
			fmt.Printf("FAIL  %s: sequence mismatch (original=%d bp, reconstructed=%d bp)\n",
				orig[i].Name, len(orig[i].Sequence), len(recon[i].Sequence))
			allOK = false
		} else {
			fmt.Printf("OK    %s (%d bp)\n", orig[i].Name, len(orig[i].Sequence))
		}
	}

	if !allOK {
		return fmt.Errorf("verification failed")
	}
	fmt.Println("all chromosomes verified OK")
	return nil
}

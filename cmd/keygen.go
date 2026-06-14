package cmd

import (
	"fmt"
	"os"

	"github.com/luizeduardocarvalho/genomehub/internal/sign"
	"github.com/spf13/cobra"
)

var keygenOut string

var keygenCmd = &cobra.Command{
	Use:   "keygen",
	Short: "Generate an ed25519 signing keypair for an origin",
	Long: `Generates an ed25519 keypair used to sign manifests. Run this once per origin.

  genomehub keygen --out origin

writes origin.key (private, keep secret) and origin.pub (public, share freely).
Start the origin with --sign-key origin.key; downloaders verify with
--verify-key origin.pub (or the hex string printed below).`,
	RunE: runKeygen,
}

func init() {
	keygenCmd.Flags().StringVar(&keygenOut, "out", "genomehub", "output basename: writes <out>.key and <out>.pub")
	rootCmd.AddCommand(keygenCmd)
}

func runKeygen(_ *cobra.Command, _ []string) error {
	s, err := sign.Generate()
	if err != nil {
		return err
	}
	keyPath := keygenOut + ".key"
	pubPath := keygenOut + ".pub"
	if err := os.WriteFile(keyPath, []byte(s.PrivateHex()+"\n"), 0o600); err != nil {
		return fmt.Errorf("write private key: %w", err)
	}
	if err := os.WriteFile(pubPath, []byte(s.PublicHex()+"\n"), 0o644); err != nil {
		return fmt.Errorf("write public key: %w", err)
	}
	fmt.Printf("private key: %s (keep secret, mode 0600)\n", keyPath)
	fmt.Printf("public key:  %s\n", pubPath)
	fmt.Printf("public hex:  %s\n", s.PublicHex())
	return nil
}

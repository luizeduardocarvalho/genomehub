package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/luizeduardocarvalho/genomehub/internal/delta"
	"github.com/luizeduardocarvalho/genomehub/internal/httpapi"
	"github.com/luizeduardocarvalho/genomehub/internal/manifest"
	"github.com/luizeduardocarvalho/genomehub/internal/sign"
	"github.com/luizeduardocarvalho/genomehub/internal/store"
	"github.com/spf13/cobra"
)

var (
	mirrorCatalog string
	mirrorOut     string
	mirrorSignKey string
)

var mirrorCmd = &cobra.Command{
	Use:   "mirror",
	Short: "Export a catalog + store as a static object tree for a free-egress bucket/CDN",
	Long: `Writes the read API as plain files, keyed exactly as clients request them:

  segments/{hash}                 segment bytes
  genomes/{assembly}/manifest     manifest JSON
  genomes/{assembly}/manifest.sig detached signature (with --sign-key)
  deltas/{assembly}               delta blob or recipe
  pubkey                          signing public key (with --sign-key)

Because the content is immutable and content-addressed, the tree can be served
from object storage with free egress (Cloudflare R2, Backblaze B2) behind a CDN
— no origin process and no egress bill for reads. Upload the output directory
with rclone/aws-s3-sync/wrangler, then point clients at the bucket/CDN URL:

  genomehub mirror --catalog ./catalog --out ./static --sign-key origin.key
  rclone copy ./static r2:my-bucket
  genomehub download --server https://cdn.example.org --assembly TAIR10 \
    --output TAIR10.fa --verify-key origin.pub`,
	RunE: runMirror,
}

func init() {
	mirrorCmd.Flags().StringVar(&mirrorCatalog, "catalog", ".", "directory of *.manifest.json and *.delta.* files to mirror")
	mirrorCmd.Flags().StringVar(&mirrorOut, "out", "", "output directory for the static tree (required)")
	mirrorCmd.Flags().StringVar(&mirrorSignKey, "sign-key", "", "ed25519 private key (from `keygen`); sign manifests into the tree")
	mirrorCmd.MarkFlagRequired("out")
	rootCmd.AddCommand(mirrorCmd)
}

func runMirror(_ *cobra.Command, _ []string) error {
	s, err := store.Open(storeDir)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer s.Close()

	cat, err := httpapi.ScanCatalog(mirrorCatalog)
	if err != nil {
		return fmt.Errorf("scan catalog: %w", err)
	}

	var signer *sign.Signer
	if mirrorSignKey != "" {
		signer, err = sign.LoadSigner(mirrorSignKey)
		if err != nil {
			return fmt.Errorf("load sign key: %w", err)
		}
	}

	seen := map[string]struct{}{} // segment hashes already written (dedup across genomes)
	var segCount, segBytes int

	writeSeg := func(hash string) error {
		hash = strings.TrimPrefix(hash, "blake3:")
		if _, ok := seen[hash]; ok {
			return nil
		}
		data, err := s.Get(hash)
		if err != nil {
			return fmt.Errorf("segment %s not in store: %w", hash, err)
		}
		if err := writeMirror(mirrorOut, "segments/"+hash, data); err != nil {
			return err
		}
		seen[hash] = struct{}{}
		segCount++
		segBytes += len(data)
		return nil
	}

	// Manifests + their segments + (optional) signatures.
	for a, path := range cat.Manifests {
		raw, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read manifest %s: %w", a, err)
		}
		if err := writeMirror(mirrorOut, "genomes/"+a+"/manifest", raw); err != nil {
			return err
		}
		if sig, ok := mirrorSig(signer, path, raw); ok {
			if err := writeMirror(mirrorOut, "genomes/"+a+"/manifest.sig", sig); err != nil {
				return err
			}
		}
		m, err := manifest.Parse(raw)
		if err != nil {
			return fmt.Errorf("parse manifest %s: %w", a, err)
		}
		for _, c := range m.Chromosomes {
			for _, seg := range c.Segments {
				if err := writeSeg(seg.Hash); err != nil {
					return err
				}
			}
		}
	}

	// Raw delta blobs.
	for a, path := range cat.Deltas {
		raw, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read delta %s: %w", a, err)
		}
		if err := writeMirror(mirrorOut, "deltas/"+a, raw); err != nil {
			return err
		}
	}

	// Recipe-backed deltas: the recipe plus each of its chunk segments.
	for a, path := range cat.Recipes {
		raw, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read recipe %s: %w", a, err)
		}
		if err := writeMirror(mirrorOut, "deltas/"+a, raw); err != nil {
			return err
		}
		r, err := delta.ParseRecipe(raw)
		if err != nil {
			return fmt.Errorf("parse recipe %s: %w", a, err)
		}
		for _, ch := range r.Chunks {
			if err := writeSeg(ch.Hash); err != nil {
				return err
			}
		}
	}

	if signer != nil {
		if err := writeMirror(mirrorOut, "pubkey", []byte(signer.PublicHex()+"\n")); err != nil {
			return err
		}
	}

	fmt.Printf("mirrored %d manifests, %d deltas to %s\n", len(cat.Manifests), len(cat.Deltas)+len(cat.Recipes), mirrorOut)
	fmt.Printf("  %d unique segments, %s\n", segCount, fmtBytes(int64(segBytes)))
	if signer != nil {
		fmt.Printf("  signed with %s\n", signer.PublicHex())
	}
	fmt.Printf("upload with e.g.: rclone copy %s r2:your-bucket\n", mirrorOut)
	return nil
}

// mirrorSig returns the detached signature to write for a manifest: a fresh one
// when a signer is supplied (over the exact served bytes), else an existing
// <manifest>.sig beside the file if present.
func mirrorSig(signer *sign.Signer, manifestPath string, raw []byte) ([]byte, bool) {
	if signer != nil {
		return signer.Sign(raw), true
	}
	if existing, err := os.ReadFile(manifestPath + ".sig"); err == nil {
		return existing, true
	}
	return nil, false
}

func writeMirror(outDir, key string, data []byte) error {
	p := filepath.Join(outDir, filepath.FromSlash(key))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, data, 0o644)
}

package httpapi

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/luizeduardocarvalho/genomehub/internal/delta"
	"github.com/luizeduardocarvalho/genomehub/internal/fasta"
	"github.com/luizeduardocarvalho/genomehub/internal/manifest"
	"github.com/luizeduardocarvalho/genomehub/internal/store"
)

// ReconstructResult reports a written, integrity-verified genome.
type ReconstructResult struct {
	Assembly    string `json:"assembly"`
	Path        string `json:"path"`
	Chromosomes int    `json:"chromosomes"`
	Bases       int    `json:"bases"`
	Verified    bool   `json:"verified"`
}

// reconstructGenome rebuilds a genome's chromosomes from the local store,
// verifying integrity as it goes (each chromosome's hash for a manifest genome;
// the reference hash + each query chromosome's hash for a delta). It is the same
// guarantee as the CLI `reconstruct`/`verify`, just driven in the node.
func (srv *server) reconstructGenome(assembly string) ([]fasta.Chromosome, error) {
	cur := srv.cur()
	if p, ok := cur.Manifests[assembly]; ok {
		return reconstructManifest(srv.store, p)
	}
	if _, ok := cur.Recipes[assembly]; ok {
		return srv.reconstructDelta(assembly)
	}
	if _, ok := cur.Deltas[assembly]; ok {
		return srv.reconstructDelta(assembly)
	}
	return nil, fmt.Errorf("%s is not held by this node", assembly)
}

// reconstructManifest concatenates each chromosome's segments from the store and
// verifies the chromosome hash.
func reconstructManifest(s *store.Store, manifestPath string) ([]fasta.Chromosome, error) {
	m, err := manifest.Read(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	var chroms []fasta.Chromosome
	for _, c := range m.Chromosomes {
		seq := make([]byte, 0, c.Length)
		for _, seg := range c.Segments {
			data, err := s.Get(strings.TrimPrefix(seg.Hash, "blake3:"))
			if err != nil {
				return nil, fmt.Errorf("missing segment %s (download the genome first): %w", seg.Hash, err)
			}
			seq = append(seq, data...)
		}
		if got := "blake3:" + store.HashBytes(seq); got != c.Hash {
			return nil, fmt.Errorf("chromosome %s: integrity check failed", c.Name)
		}
		chroms = append(chroms, fasta.Chromosome{Name: c.Name, Sequence: seq})
	}
	return chroms, nil
}

// reconstructDelta rebuilds a delta-encoded genome: reassemble the delta blob
// from store chunks (recipe) or read the raw .ghd, resolve the reference from the
// store, and apply (delta.Apply verifies the reference hash and every query
// chromosome hash).
func (srv *server) reconstructDelta(assembly string) ([]fasta.Chromosome, error) {
	cur := srv.cur()

	ghdPath := ""
	if recipePath, ok := cur.Recipes[assembly]; ok {
		r, err := delta.ReadRecipe(recipePath)
		if err != nil {
			return nil, fmt.Errorf("read delta recipe: %w", err)
		}
		blob := make([]byte, 0, r.TotalSize)
		for _, c := range r.Chunks {
			data, err := srv.store.Get(strings.TrimPrefix(c.Hash, "blake3:"))
			if err != nil {
				return nil, fmt.Errorf("missing delta chunk %s (download the genome first): %w", c.Hash, err)
			}
			blob = append(blob, data...)
		}
		tmp, err := os.CreateTemp("", "ghd-*.delta")
		if err != nil {
			return nil, err
		}
		defer os.Remove(tmp.Name())
		if _, err := tmp.Write(blob); err != nil {
			tmp.Close()
			return nil, err
		}
		tmp.Close()
		ghdPath = tmp.Name()
	} else if p, ok := cur.Deltas[assembly]; ok {
		ghdPath = p
	} else {
		return nil, fmt.Errorf("%s has no delta", assembly)
	}

	d, err := delta.Read(ghdPath)
	if err != nil {
		return nil, fmt.Errorf("parse delta: %w", err)
	}
	refPath, ok := cur.Manifests[d.Reference]
	if !ok {
		return nil, fmt.Errorf("reference %s not held — download it first", d.Reference)
	}
	ref, err := reconstructManifest(srv.store, refPath)
	if err != nil {
		return nil, fmt.Errorf("reconstruct reference %s: %w", d.Reference, err)
	}
	return delta.Apply(d, ref)
}

// reconstructPath is where a reconstructed FASTA is written when the request does
// not specify an output: a "reconstructed" dir beside the node's data.
func (srv *server) reconstructPath(assembly, output string) (string, error) {
	if output != "" {
		return output, nil
	}
	if srv.manifestDir == "" {
		return "", fmt.Errorf("no output path given and no default directory configured")
	}
	dir := filepath.Join(filepath.Dir(srv.manifestDir), "reconstructed")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return filepath.Join(dir, assembly+".fa"), nil
}

package delta

import (
	"encoding/json"
	"os"
)

// Recipe is the content-addressed form of a delta for network transfer: the delta
// blob (a GHD1 file) split into segment-sized chunks, listed by hash. A peer
// fetches the recipe, then pulls the chunk hashes it lacks through the same
// /segments/{hash} machinery as genome segments — so deltas swarm and dedup like
// everything else (ADR 0002 §1). Chunking the *blob* (not per-op) keeps chunks at
// segment scale and avoids the micro-segment explosion ADR 0001 warned about.
type Recipe struct {
	Assembly  string  `json:"assembly"`
	Reference string  `json:"reference"`
	TotalSize int     `json:"total_size"`
	Chunks    []Chunk `json:"chunks"`
}

// Chunk is one piece of a chunked delta blob.
type Chunk struct {
	Hash   string `json:"hash"` // raw hex (a segment-store key)
	Length int    `json:"length"`
}

func (r *Recipe) Write(path string) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func ReadRecipe(path string) (*Recipe, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseRecipe(data)
}

// ParseRecipe decodes a recipe from its JSON bytes.
func ParseRecipe(data []byte) (*Recipe, error) {
	var r Recipe
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

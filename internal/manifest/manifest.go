package manifest

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/zeebo/blake3"
)

type Segment struct {
	Hash   string `json:"hash"`
	Length int    `json:"length"`
}

type Chromosome struct {
	Name     string    `json:"name"`
	Length   int       `json:"length"`
	Hash     string    `json:"hash"`
	Segments []Segment `json:"segments"`
}

type Chunking struct {
	Algorithm string `json:"algorithm"`
	MinSize   int    `json:"min_size"`
	MaxSize   int    `json:"max_size"`
}

type Manifest struct {
	Version      int          `json:"version"`
	GraphVersion int          `json:"graph_version"`
	Organism     string       `json:"organism"`
	Assembly     string       `json:"assembly"`
	TotalBases   int          `json:"total_bases"`
	Encoding     string       `json:"encoding"`
	Chunking     Chunking     `json:"chunking"`
	CreatedAt    time.Time    `json:"created_at"`
	SegmentsRoot string       `json:"segments_root"`
	Chromosomes  []Chromosome `json:"chromosomes"`
}

func (m *Manifest) Write(path string) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

func Read(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m Manifest
	return &m, json.Unmarshal(data, &m)
}

func ComputeSegmentsRoot(chroms []Chromosome) string {
	h := blake3.New()
	for _, c := range chroms {
		for _, s := range c.Segments {
			fmt.Fprint(h, s.Hash)
		}
	}
	return fmt.Sprintf("blake3:%x", h.Sum(nil))
}

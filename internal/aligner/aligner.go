// Package aligner defines the shared types and interface for sequence alignment.
// All MEM-finders implement Aligner; the rest of the pipeline is agnostic to
// whether you use minimap2 or the native FM-index backend.
package aligner

// ExactMatch is a byte-identical region between a target and a query sequence.
// All coordinates are 0-based, half-open: the match covers [Start, End).
type ExactMatch struct {
	TargetStart int
	TargetEnd   int
	QueryStart  int
	QueryEnd    int
}

// Len returns the length of the match in bases.
func (m ExactMatch) Len() int { return m.TargetEnd - m.TargetStart }

// Aligner finds all maximal exact matches between two sequences.
type Aligner interface {
	// FindMEMs returns every exact match of length >= minLen between
	// target and query, sorted by TargetStart then QueryStart.
	FindMEMs(target, query []byte, minLen int) ([]ExactMatch, error)
}

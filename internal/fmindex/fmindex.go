// Package fmindex implements an FM-index backed MEM-finder for genomic sequences.
//
// # Data flow
//
//	target (A), query (B)
//	        │
//	        ▼
//	text = A + '\x01' + B + '\x00'
//	  sentinels:  '\x00'=0 < '\x01'=1 < 'A'=65
//	        │
//	        ▼
//	Suffix array SA  (O(n log²n))
//	        │
//	        ▼
//	BWT[i] = text[SA[i]-1]
//	        │
//	        ▼
//	C[c]         = # chars in text lex-smaller than c
//	Occ(c, pos)  = # of c in BWT[0..pos)  (sampled every 128 rows)
//	        │
//	        ▼
//	MEM finding  (for each query start j):
//	  1. Extend rightward while backwardSearch range contains ≥1 TARGET hit.
//	  2. Left-maximality: drop hits where text[tp-1] == text[qOff+j-1].
//	  3. Right-maximality: guaranteed by (1).
//
// NOTES ON COMPLEXITY
// ─────────────────────────────────────────────────────────────────────────────
// • Each backwardSearch call is O(r), so the LCE loop at position j is O(lce²).
//   Total: O(qLen · lce²). For genomic data with minLen ≥ 50 and SNP density
//   ~1/115 bp, typical lce ≈ 115, giving ~qLen · 13000 ops.
// • For a true O(qLen · lce) implementation, use a bi-directional FM-index.
// • filterMaximal is O(k²) in the MEM count k. Use an interval tree for k > 100k.
// • buildSuffixArray is O(n log²n). Use SA-IS for n > 50 MB.
// • - strand MEMs require reverse-complementing the query and re-running FindMEMs.
//   NativeFinder.FindMEMs searches only the forward strand.
package fmindex

import (
	"sort"

	"github.com/luizcarvalho/genome-hub/internal/aligner"
)

// ─────────────────────────────────────────────────────────────────────────────
// Suffix array  (O(n log²n))
// ─────────────────────────────────────────────────────────────────────────────

func buildSuffixArray(t []byte) []int32 {
	n := len(t)
	sa := make([]int32, n)
	for i := range sa {
		sa[i] = int32(i)
	}
	sort.Slice(sa, func(a, b int) bool {
		ia, ib := sa[a], sa[b]
		for ia < int32(n) && ib < int32(n) {
			if t[ia] != t[ib] {
				return t[ia] < t[ib]
			}
			ia++
			ib++
		}
		// With sentinel '\x00' at position n-1, both suffixes always resolve
		// before exhausting bounds. This branch is unreachable for valid input
		// but is kept for correctness: if ia exhausted first it is the shorter
		// suffix, which sorts lower.
		return ia >= int32(n) && ib < int32(n)
	})
	return sa
}

// ─────────────────────────────────────────────────────────────────────────────
// BWT
// ─────────────────────────────────────────────────────────────────────────────

func buildBWT(t []byte, sa []int32) []byte {
	bwt := make([]byte, len(t))
	for i, s := range sa {
		if s == 0 {
			bwt[i] = t[len(t)-1]
		} else {
			bwt[i] = t[s-1]
		}
	}
	return bwt
}

// ─────────────────────────────────────────────────────────────────────────────
// FM-index
// ─────────────────────────────────────────────────────────────────────────────

const occSampleRate = 128

type fmIndex struct {
	C          [256]int32
	occSamples [][256]int32
	bwt        []byte
	sa         []int32
	n          int
}

func buildIndex(t []byte) *fmIndex {
	sa := buildSuffixArray(t)
	bwt := buildBWT(t, sa)
	n := len(t)
	idx := &fmIndex{bwt: bwt, sa: sa, n: n}

	var freq [256]int32
	for _, c := range t {
		freq[c]++
	}
	var cum int32
	for c := 0; c < 256; c++ {
		idx.C[c] = cum
		cum += freq[c]
	}

	nS := n/occSampleRate + 2
	idx.occSamples = make([][256]int32, nS)
	var running [256]int32
	for i, c := range bwt {
		if i%occSampleRate == 0 {
			idx.occSamples[i/occSampleRate] = running
		}
		running[c]++
	}
	idx.occSamples[nS-1] = running
	return idx
}

func (idx *fmIndex) occ(c byte, pos int) int32 {
	if pos <= 0 {
		return 0
	}
	k := (pos - 1) / occSampleRate
	cnt := idx.occSamples[k][c]
	for i := k * occSampleRate; i < pos; i++ {
		if idx.bwt[i] == c {
			cnt++
		}
	}
	return cnt
}

// backwardSearch returns the SA range [lo, hi) for all suffixes starting with p.
func (idx *fmIndex) backwardSearch(p []byte) (lo, hi int) {
	lo, hi = 0, idx.n
	for i := len(p) - 1; i >= 0 && lo < hi; i-- {
		c := p[i]
		lo = int(idx.C[c]) + int(idx.occ(c, lo))
		hi = int(idx.C[c]) + int(idx.occ(c, hi))
	}
	return
}

// ─────────────────────────────────────────────────────────────────────────────
// Sentinels
// ─────────────────────────────────────────────────────────────────────────────

const (
	sentinel0 byte = 0x00 // end-of-text (absolute lexicographic minimum)
	sentinel1 byte = 0x01 // target/query separator
)

// ─────────────────────────────────────────────────────────────────────────────
// Finder
// ─────────────────────────────────────────────────────────────────────────────

// Finder wraps an FM-index over the concatenated target+query.
type Finder struct {
	idx        *fmIndex
	text       []byte
	targetLen  int
	queryLen   int
	targetRows []int32 // sorted SA row indices where SA[row] < targetLen
}

// New builds a Finder for target and query.
// Time: O(n log²n), Space: O(n) where n = len(target)+len(query).
// For n > 50 MB, consider SA-IS construction.
func New(target, query []byte) *Finder {
	tLen, qLen := len(target), len(query)
	text := make([]byte, tLen+1+qLen+1)
	copy(text, target)
	text[tLen] = sentinel1
	copy(text[tLen+1:], query)
	text[tLen+1+qLen] = sentinel0

	idx := buildIndex(text)

	// Precompute sorted target-row index for O(log n) hasTargetHit checks.
	// SA rows are already in order 0..n-1; filter those pointing into target.
	targetRows := make([]int32, 0, tLen)
	for row, pos := range idx.sa {
		if int(pos) < tLen {
			targetRows = append(targetRows, int32(row))
		}
	}
	// targetRows is sorted because we iterate rows in ascending order.

	return &Finder{
		idx:        idx,
		text:       text,
		targetLen:  tLen,
		queryLen:   qLen,
		targetRows: targetRows,
	}
}

// hasTargetHit returns true if any SA row in [lo, hi) points into the target.
// O(log n) via binary search on the precomputed sorted target-row list.
func (f *Finder) hasTargetHit(lo, hi int) bool {
	i := sort.Search(len(f.targetRows), func(i int) bool {
		return int(f.targetRows[i]) >= lo
	})
	return i < len(f.targetRows) && int(f.targetRows[i]) < hi
}

// ─────────────────────────────────────────────────────────────────────────────
// FindMEMs
// ─────────────────────────────────────────────────────────────────────────────

// FindMEMs returns all maximal exact matches of length >= minLen.
// Only searches the forward strand. For reverse-strand MEMs, reverse-complement
// the query and call FindMEMs again, adjusting coordinates.
func (f *Finder) FindMEMs(minLen int) []aligner.ExactMatch {
	tLen := f.targetLen
	qLen := f.queryLen
	qOff := tLen + 1

	type cand struct{ tp, qp, l int }
	var cands []cand

	for j := 0; j < qLen; j++ {
		var bestLo, bestHi int
		lce := 0

		for r := 1; j+r <= qLen; r++ {
			last := f.text[qOff+j+r-1]
			if last == sentinel0 || last == sentinel1 {
				break
			}

			slo, shi := f.idx.backwardSearch(f.text[qOff+j : qOff+j+r])
			if slo >= shi {
				break
			}
			// O(log n): stop if no target-side hits remain in this SA range.
			if !f.hasTargetHit(slo, shi) {
				break
			}

			bestLo, bestHi = slo, shi
			lce = r
		}

		if lce < minLen {
			continue
		}

		for row := bestLo; row < bestHi; row++ {
			tp := int(f.idx.sa[row])
			if tp < 0 || tp+lce > tLen {
				continue
			}
			// Left-maximality: the character immediately to the left must differ.
			if j > 0 && tp > 0 && f.text[tp-1] == f.text[qOff+j-1] {
				continue
			}
			cands = append(cands, cand{tp, j, lce})
		}
	}

	type k3 [3]int
	seen := make(map[k3]bool, len(cands))
	matches := make([]aligner.ExactMatch, 0, len(cands))
	for _, c := range cands {
		if seen[k3{c.tp, c.qp, c.l}] {
			continue
		}
		seen[k3{c.tp, c.qp, c.l}] = true
		matches = append(matches, aligner.ExactMatch{
			TargetStart: c.tp,
			TargetEnd:   c.tp + c.l,
			QueryStart:  c.qp,
			QueryEnd:    c.qp + c.l,
		})
	}

	matches = filterMaximal(matches)

	sort.Slice(matches, func(i, j int) bool {
		if matches[i].TargetStart != matches[j].TargetStart {
			return matches[i].TargetStart < matches[j].TargetStart
		}
		return matches[i].QueryStart < matches[j].QueryStart
	})
	return matches
}

// filterMaximal removes any match fully contained in a longer one on both axes.
// O(k²) — use an interval tree for k > 100k.
func filterMaximal(matches []aligner.ExactMatch) []aligner.ExactMatch {
	n := len(matches)
	if n == 0 {
		return nil
	}
	dominated := make([]bool, n)
	for i := 0; i < n; i++ {
		if dominated[i] {
			continue
		}
		m := matches[i]
		for j := 0; j < n; j++ {
			if i == j || dominated[i] {
				continue
			}
			o := matches[j]
			if o.TargetStart <= m.TargetStart && o.TargetEnd >= m.TargetEnd &&
				o.QueryStart <= m.QueryStart && o.QueryEnd >= m.QueryEnd &&
				o.Len() > m.Len() {
				dominated[i] = true
			}
		}
	}
	// In-place compaction: write index always trails read index, so safe.
	out := matches[:0]
	for i, m := range matches {
		if !dominated[i] {
			out = append(out, m)
		}
	}
	return out
}

// ─────────────────────────────────────────────────────────────────────────────
// NativeFinder — implements aligner.Aligner
// ─────────────────────────────────────────────────────────────────────────────

// NativeFinder implements aligner.Aligner using the native FM-index backend.
// Best suited for sequences up to ~50 MB. For full genomes, call per-chromosome.
type NativeFinder struct{}

func (NativeFinder) FindMEMs(target, query []byte, minLen int) ([]aligner.ExactMatch, error) {
	return New(target, query).FindMEMs(minLen), nil
}

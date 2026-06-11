// Package sketch implements bottom-k MinHash genome fingerprinting (the Mash
// model) in pure Go, so GenomeHub stays a single dependency-free binary.
//
// A sketch is the k smallest hash values among all canonical k-mers of a genome
// (~8 KB regardless of genome size). The Jaccard similarity of two sketches
// estimates k-mer set overlap; a Mash-style transform converts that to an
// approximate sequence identity (ANI). This is the cheap signal used to route
// genome pairs to the right storage strategy — see
// docs/adr/0001-delta-vs-segment-dedup-routing.md.
package sketch

import (
	"encoding/json"
	"math"
	"os"
	"sort"
)

const (
	DefaultKmer = 21   // k-mer length
	DefaultSize = 1000 // number of bottom hashes kept
)

// Sketch is a genome fingerprint: the Size smallest hashes of its canonical
// k-mers, sorted ascending.
type Sketch struct {
	Assembly string   `json:"assembly"`
	Kmer     int      `json:"kmer"`
	Size     int      `json:"size"`
	Hashes   []uint64 `json:"hashes"`
}

// Compute builds a sketch from one or more sequences (e.g. all chromosomes of a
// genome). N-containing or other non-ACGT k-mers are skipped.
func Compute(assembly string, seqs [][]byte, kmer, size int) Sketch {
	if kmer <= 0 {
		kmer = DefaultKmer
	}
	if size <= 0 {
		size = DefaultSize
	}
	h := newHeap(size)
	for _, seq := range seqs {
		streamKmers(seq, kmer, func(canonical uint64) {
			h.push(canonical)
		})
	}
	hashes := h.sorted()
	return Sketch{Assembly: assembly, Kmer: kmer, Size: size, Hashes: hashes}
}

// Jaccard estimates the Jaccard similarity of two sketches using the bottom-k
// merge estimator. Sketches must share the same k-mer length.
func Jaccard(a, b Sketch) float64 {
	k := a.Size
	if b.Size < k {
		k = b.Size
	}
	if len(a.Hashes) < k {
		k = len(a.Hashes)
	}
	if len(b.Hashes) < k {
		k = len(b.Hashes)
	}
	if k == 0 {
		return 0
	}
	var i, j, common, seen int
	for i < len(a.Hashes) && j < len(b.Hashes) && seen < k {
		switch {
		case a.Hashes[i] < b.Hashes[j]:
			i++
		case b.Hashes[j] < a.Hashes[i]:
			j++
		default:
			common++
			i++
			j++
		}
		seen++
	}
	if seen == 0 {
		return 0
	}
	return float64(common) / float64(seen)
}

// Similarity converts Jaccard to an approximate sequence identity (ANI) via the
// Mash distance transform: d = -1/k * ln(2j/(1+j)); identity = 1 - d. Returns a
// value in [0,1]. j=0 → 0, j=1 → 1.
func Similarity(a, b Sketch) float64 {
	j := Jaccard(a, b)
	if j <= 0 {
		return 0
	}
	if j >= 1 {
		return 1
	}
	kmer := a.Kmer
	d := -1.0 / float64(kmer) * math.Log(2*j/(1+j))
	sim := 1 - d
	if sim < 0 {
		return 0
	}
	return sim
}

// Write/Read persist a sketch as JSON.
func (s Sketch) Write(path string) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func Read(path string) (Sketch, error) {
	var s Sketch
	data, err := os.ReadFile(path)
	if err != nil {
		return s, err
	}
	err = json.Unmarshal(data, &s)
	return s, err
}

// ── k-mer streaming ───────────────────────────────────────────────────────────

// base2bit maps A/C/G/T to 0..3; returns ok=false for anything else.
func base2bit(b byte) (uint64, bool) {
	switch b {
	case 'A', 'a':
		return 0, true
	case 'C', 'c':
		return 1, true
	case 'G', 'g':
		return 2, true
	case 'T', 't':
		return 3, true
	}
	return 0, false
}

// streamKmers calls fn with the hash of each canonical k-mer (min of forward and
// reverse-complement 2-bit encodings). Windows containing non-ACGT bytes are
// skipped. Uses a rolling 2-bit encoding for both strands.
func streamKmers(seq []byte, k int, fn func(uint64)) {
	if len(seq) < k || k <= 0 || k > 32 {
		return
	}
	var fwd, rev uint64
	mask := uint64(math.MaxUint64)
	if k < 32 {
		mask = (uint64(1) << uint(2*k)) - 1
	}
	revShift := uint(2 * (k - 1))
	valid := 0
	for i := 0; i < len(seq); i++ {
		c, ok := base2bit(seq[i])
		if !ok {
			valid = 0
			fwd, rev = 0, 0
			continue
		}
		fwd = ((fwd << 2) | c) & mask
		rev = (rev >> 2) | ((3 - c) << revShift)
		valid++
		if valid >= k {
			canon := fwd
			if rev < canon {
				canon = rev
			}
			fn(hash64(canon))
		}
	}
}

// hash64 is a fast integer mixer (SplitMix64 finalizer) so adjacent k-mers land
// far apart in hash space.
func hash64(x uint64) uint64 {
	x += 0x9e3779b97f4a7c15
	x = (x ^ (x >> 30)) * 0xbf58476d1ce4e5b9
	x = (x ^ (x >> 27)) * 0x94d049bb133111eb
	return x ^ (x >> 31)
}

// ── bottom-k max-heap of smallest hashes ──────────────────────────────────────

// minHashHeap keeps the `size` smallest distinct hashes seen. It is a max-heap of
// those, so the largest kept hash is at the root and easy to evict.
type minHashHeap struct {
	size int
	data []uint64
	seen map[uint64]struct{}
}

func newHeap(size int) *minHashHeap {
	return &minHashHeap{size: size, data: make([]uint64, 0, size), seen: make(map[uint64]struct{}, size)}
}

func (h *minHashHeap) push(v uint64) {
	if _, dup := h.seen[v]; dup {
		return
	}
	if len(h.data) < h.size {
		h.seen[v] = struct{}{}
		h.data = append(h.data, v)
		h.up(len(h.data) - 1)
		return
	}
	if v >= h.data[0] {
		return // not smaller than current largest-kept
	}
	delete(h.seen, h.data[0])
	h.seen[v] = struct{}{}
	h.data[0] = v
	h.down(0)
}

func (h *minHashHeap) up(i int) {
	for i > 0 {
		p := (i - 1) / 2
		if h.data[p] >= h.data[i] {
			break
		}
		h.data[p], h.data[i] = h.data[i], h.data[p]
		i = p
	}
}

func (h *minHashHeap) down(i int) {
	n := len(h.data)
	for {
		l, r, largest := 2*i+1, 2*i+2, i
		if l < n && h.data[l] > h.data[largest] {
			largest = l
		}
		if r < n && h.data[r] > h.data[largest] {
			largest = r
		}
		if largest == i {
			break
		}
		h.data[i], h.data[largest] = h.data[largest], h.data[i]
		i = largest
	}
}

func (h *minHashHeap) sorted() []uint64 {
	out := append([]uint64(nil), h.data...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

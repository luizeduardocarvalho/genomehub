// Package delta stores a genome as a patch against a reference genome, rather
// than as deduplicated segments. It is the right model for near-identical
// genomes (same-species accessions) where novelty is ~1% smeared across 100% of
// the sequence — see docs/adr/0001-delta-vs-segment-dedup-routing.md.
//
// A query chromosome is represented as an ordered list of operations:
//
//	copy(refChrom, a, b)  — take reference[refChrom][a:b] verbatim
//	literal(bytes)        — emit these bytes (divergent / novel sequence)
//
// Reconstruction concatenates the result of each op. Only literal bytes cost
// storage; shared sequence is referenced, not copied. Round-trip is exact.
package delta

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/luizeduardocarvalho/genomehub/internal/aligner"
	"github.com/luizeduardocarvalho/genomehub/internal/fasta"
	"github.com/luizeduardocarvalho/genomehub/internal/store"
)

// OpType is the kind of a reconstruction operation.
type OpType string

const (
	OpCopy    OpType = "copy"
	OpLiteral OpType = "literal"
)

// Op is one reconstruction instruction. For OpCopy, RefChrom/RefStart/RefEnd are
// set. For OpLiteral, Bytes is set (JSON-encoded as base64).
type Op struct {
	Type     OpType `json:"type"`
	RefChrom string `json:"ref_chrom,omitempty"`
	RefStart int    `json:"ref_start,omitempty"`
	RefEnd   int    `json:"ref_end,omitempty"`
	Bytes    []byte `json:"bytes,omitempty"`
}

// ChromDelta is the patch for a single query chromosome.
type ChromDelta struct {
	Name   string `json:"name"`
	Length int    `json:"length"`
	Hash   string `json:"hash"` // blake3 of the full query chromosome sequence
	Ops    []Op   `json:"ops"`
}

// Delta is the patch for a whole query genome against a reference genome.
type Delta struct {
	Version       int          `json:"version"`
	Assembly      string       `json:"assembly"`               // query assembly name
	Reference     string       `json:"reference"`              // reference assembly name
	ReferenceHash string       `json:"reference_hash"`         // blake3 of reference sequence (see ReferenceHash)
	RefManifest   string       `json:"ref_manifest,omitempty"` // optional: manifest that reconstructs the reference from the store
	TotalBases    int          `json:"total_bases"`            // total query bases
	LiteralBases  int          `json:"literal_bases"`          // bases stored as literals (the cost)
	CreatedAt     time.Time    `json:"created_at"`
	Chromosomes   []ChromDelta `json:"chromosomes"`
}

// ReferenceHash returns a content hash over reference chromosomes (name+sequence,
// in the given order), used to confirm a delta is applied against the right
// reference at reconstruction time.
func ReferenceHash(refChroms []fasta.Chromosome) string {
	h := make([]byte, 0, 1<<20)
	for _, c := range refChroms {
		h = append(h, c.Name...)
		h = append(h, 0)
		h = append(h, c.Sequence...)
		h = append(h, 0)
	}
	return "blake3:" + store.HashBytes(h)
}

// Build constructs a Delta for queryChroms against refChroms using alignment
// blocks (target = reference, query = the genome being encoded). Only +-strand
// blocks become copy ops; X/I operations, unaligned query regions and -strand
// blocks become literals. The result is always a correct round-trip; compression
// improves as more sequence is expressed as copy ops.
func Build(queryAssembly, refAssembly string, refChroms, queryChroms []fasta.Chromosome, blocks []aligner.Block) *Delta {
	refMap := chromMap(refChroms)

	// Group +-strand blocks by query chromosome.
	byQuery := map[string][]aligner.Block{}
	for _, b := range blocks {
		if b.Strand != "+" || b.CIGAR == "" {
			continue
		}
		if _, ok := refMap[b.TargetName]; !ok {
			continue
		}
		byQuery[b.QueryName] = append(byQuery[b.QueryName], b)
	}

	d := &Delta{
		Version:       1,
		Assembly:      queryAssembly,
		Reference:     refAssembly,
		ReferenceHash: ReferenceHash(refChroms),
		CreatedAt:     time.Now().UTC(),
	}

	for _, qc := range queryChroms {
		cd := buildChrom(qc, byQuery[qc.Name], refMap)
		d.Chromosomes = append(d.Chromosomes, cd)
		d.TotalBases += len(qc.Sequence)
		for _, op := range cd.Ops {
			if op.Type == OpLiteral {
				d.LiteralBases += len(op.Bytes)
			}
		}
	}
	return d
}

func buildChrom(qc fasta.Chromosome, blocks []aligner.Block, refMap map[string][]byte) ChromDelta {
	seq := qc.Sequence
	cd := ChromDelta{
		Name:   qc.Name,
		Length: len(seq),
		Hash:   "blake3:" + store.HashBytes(seq),
	}

	// Greedily select non-overlapping blocks (by query coords), longest first so
	// a long alignment wins over a short overlapping one.
	sorted := append([]aligner.Block(nil), blocks...)
	sortBlocksByQueryStart(sorted)

	var b stringBuilderOps
	qpos := 0
	for _, blk := range sorted {
		if blk.QueryStart < qpos {
			continue // overlaps already-consumed query region
		}
		if blk.QueryStart > qpos {
			b.literal(seq[qpos:blk.QueryStart])
		}
		walkBlock(&b, blk, seq, refMap)
		qpos = blk.QueryEnd
	}
	if qpos < len(seq) {
		b.literal(seq[qpos:])
	}
	cd.Ops = b.ops
	return cd
}

// walkBlock translates one +-strand alignment block's CIGAR into copy/literal ops.
func walkBlock(b *stringBuilderOps, blk aligner.Block, seq []byte, refMap map[string][]byte) {
	ref := refMap[blk.TargetName]
	tpos := blk.TargetStart
	qp := blk.QueryStart
	for _, op := range aligner.ParseCIGAR(blk.CIGAR) {
		switch op.Op {
		case '=', 'M':
			// Exact (or unspecified) match: copy from reference.
			end := tpos + op.Len
			if end <= len(ref) {
				b.copy(blk.TargetName, tpos, end)
			} else {
				b.literal(seq[qp : qp+op.Len])
			}
			tpos += op.Len
			qp += op.Len
		case 'X':
			b.literal(seq[qp : qp+op.Len])
			tpos += op.Len
			qp += op.Len
		case 'I', 'S':
			b.literal(seq[qp : qp+op.Len])
			qp += op.Len
		case 'D', 'N':
			tpos += op.Len
		case 'H':
			// hard clip: no query/ref consumption
		}
	}
}

// Apply reconstructs the query genome from a Delta and the reference chromosomes.
// It verifies each chromosome's hash and returns an error on mismatch.
func Apply(d *Delta, refChroms []fasta.Chromosome) ([]fasta.Chromosome, error) {
	refMap := chromMap(refChroms)
	if got := ReferenceHash(refChroms); got != d.ReferenceHash {
		return nil, fmt.Errorf("reference mismatch: delta built against %s, got %s", d.ReferenceHash, got)
	}

	out := make([]fasta.Chromosome, 0, len(d.Chromosomes))
	for _, cd := range d.Chromosomes {
		seq := make([]byte, 0, cd.Length)
		for _, op := range cd.Ops {
			switch op.Type {
			case OpCopy:
				ref, ok := refMap[op.RefChrom]
				if !ok {
					return nil, fmt.Errorf("chrom %s: copy from unknown reference chromosome %q", cd.Name, op.RefChrom)
				}
				if op.RefStart < 0 || op.RefEnd > len(ref) || op.RefStart > op.RefEnd {
					return nil, fmt.Errorf("chrom %s: copy range [%d:%d] out of bounds for %s (len %d)",
						cd.Name, op.RefStart, op.RefEnd, op.RefChrom, len(ref))
				}
				seq = append(seq, ref[op.RefStart:op.RefEnd]...)
			case OpLiteral:
				seq = append(seq, op.Bytes...)
			default:
				return nil, fmt.Errorf("chrom %s: unknown op type %q", cd.Name, op.Type)
			}
		}
		if got := "blake3:" + store.HashBytes(seq); got != cd.Hash {
			return nil, fmt.Errorf("chrom %s: reconstruction hash mismatch (got %s, want %s)", cd.Name, got, cd.Hash)
		}
		out = append(out, fasta.Chromosome{Name: cd.Name, Sequence: seq})
	}
	return out, nil
}

// Write serialises the delta to disk in the compact binary format.
func (d *Delta) Write(path string) error { return d.WriteBinary(path) }

// Read loads a delta from disk, auto-detecting the binary ("GHD1") or JSON form.
func Read(path string) (*Delta, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	hdr := make([]byte, 4)
	n, _ := io.ReadFull(f, hdr)
	f.Close()
	if n == 4 && string(hdr) == string(magic) {
		return ReadBinary(path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var d Delta
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, fmt.Errorf("delta is neither GHD1 binary nor valid JSON: %w", err)
	}
	return &d, nil
}

// WriteJSON serialises the delta to an indented JSON file (debug/inspection).
func (d *Delta) WriteJSON(path string) error {
	data, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// ── helpers ───────────────────────────────────────────────────────────────────

func chromMap(chroms []fasta.Chromosome) map[string][]byte {
	m := make(map[string][]byte, len(chroms))
	for _, c := range chroms {
		m[c.Name] = c.Sequence
	}
	return m
}

func sortBlocksByQueryStart(blocks []aligner.Block) {
	// insertion-free simple sort (small N per chromosome typically)
	for i := 1; i < len(blocks); i++ {
		for j := i; j > 0 && blocks[j-1].QueryStart > blocks[j].QueryStart; j-- {
			blocks[j-1], blocks[j] = blocks[j], blocks[j-1]
		}
	}
}

// stringBuilderOps accumulates ops, merging adjacent literals and adjacent
// contiguous copies from the same reference chromosome to keep the op list small.
type stringBuilderOps struct {
	ops []Op
}

func (b *stringBuilderOps) literal(p []byte) {
	if len(p) == 0 {
		return
	}
	if n := len(b.ops); n > 0 && b.ops[n-1].Type == OpLiteral {
		b.ops[n-1].Bytes = append(b.ops[n-1].Bytes, p...)
		return
	}
	cp := make([]byte, len(p))
	copy(cp, p)
	b.ops = append(b.ops, Op{Type: OpLiteral, Bytes: cp})
}

func (b *stringBuilderOps) copy(refChrom string, start, end int) {
	if start >= end {
		return
	}
	if n := len(b.ops); n > 0 {
		last := &b.ops[n-1]
		if last.Type == OpCopy && last.RefChrom == refChrom && last.RefEnd == start {
			last.RefEnd = end
			return
		}
	}
	b.ops = append(b.ops, Op{Type: OpCopy, RefChrom: refChrom, RefStart: start, RefEnd: end})
}

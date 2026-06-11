package delta

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"
)

func parseTime(s string) (time.Time, error) {
	return time.Parse(time.RFC3339, s)
}

// Binary on-disk format ("GHD1"). The JSON representation (WriteJSON) is verbose
// — ~80 bytes per op — which for SNP-dense genomes (millions of ops) dwarfs the
// actual information. The binary format packs the same op model compactly:
//
//   - copy ops: reference chromosome names are interned into a table; each copy
//     stores a table index, a zigzag-varint of (refStart - previousRefEnd) so
//     monotonic walks cost ~1 byte, and a varint length.
//   - literal ops: a varint length, an encoding byte, then either raw bytes or
//     2-bit-packed bases (A/C/G/T) — 0.25 B/base instead of 1 B (or 1.33 B in
//     base64 JSON). Literals containing anything outside ACGT (e.g. N) fall back
//     to raw.
//
// The result is byte-for-byte equivalent to the JSON form after a round-trip.

var magic = []byte("GHD1")

const formatVersion = 1

// metaHeader is the per-file metadata, stored as a small JSON blob in the binary
// container (it is tiny and benefits from staying human-readable).
type metaHeader struct {
	Version       int    `json:"version"`
	Assembly      string `json:"assembly"`
	Reference     string `json:"reference"`
	ReferenceHash string `json:"reference_hash"`
	RefManifest   string `json:"ref_manifest,omitempty"`
	TotalBases    int    `json:"total_bases"`
	LiteralBases  int    `json:"literal_bases"`
	CreatedAt     string `json:"created_at"`
}

// WriteBinary serialises the delta in the packed "GHD1" format.
func (d *Delta) WriteBinary(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriterSize(f, 4*1024*1024)

	if _, err := w.Write(magic); err != nil {
		return err
	}
	if err := w.WriteByte(formatVersion); err != nil {
		return err
	}

	meta := metaHeader{
		Version: d.Version, Assembly: d.Assembly, Reference: d.Reference,
		ReferenceHash: d.ReferenceHash, RefManifest: d.RefManifest, TotalBases: d.TotalBases,
		LiteralBases: d.LiteralBases, CreatedAt: d.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
	}
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	writeBytes(w, metaJSON)

	// Intern reference chromosome names.
	refIdx := map[string]int{}
	var refNames []string
	for _, c := range d.Chromosomes {
		for _, op := range c.Ops {
			if op.Type == OpCopy {
				if _, ok := refIdx[op.RefChrom]; !ok {
					refIdx[op.RefChrom] = len(refNames)
					refNames = append(refNames, op.RefChrom)
				}
			}
		}
	}
	writeUvarint(w, uint64(len(refNames)))
	for _, n := range refNames {
		writeBytes(w, []byte(n))
	}

	writeUvarint(w, uint64(len(d.Chromosomes)))
	for _, c := range d.Chromosomes {
		writeBytes(w, []byte(c.Name))
		writeUvarint(w, uint64(c.Length))
		writeBytes(w, []byte(c.Hash))
		writeUvarint(w, uint64(len(c.Ops)))

		var prevRefEnd int
		for _, op := range c.Ops {
			switch op.Type {
			case OpCopy:
				w.WriteByte(0)
				writeUvarint(w, uint64(refIdx[op.RefChrom]))
				writeSvarint(w, int64(op.RefStart-prevRefEnd))
				writeUvarint(w, uint64(op.RefEnd-op.RefStart))
				prevRefEnd = op.RefEnd
			case OpLiteral:
				w.WriteByte(1)
				writeUvarint(w, uint64(len(op.Bytes)))
				if packed, ok := pack2bit(op.Bytes); ok {
					w.WriteByte(1)
					w.Write(packed)
				} else {
					w.WriteByte(0)
					w.Write(op.Bytes)
				}
			default:
				return fmt.Errorf("chrom %s: unknown op type %q", c.Name, op.Type)
			}
		}
	}
	return w.Flush()
}

// ReadBinary parses the packed "GHD1" format.
func ReadBinary(path string) (*Delta, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 4*1024*1024)

	hdr := make([]byte, 4)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return nil, err
	}
	if string(hdr) != string(magic) {
		return nil, fmt.Errorf("not a GHD1 delta file (bad magic %q)", hdr)
	}
	ver, err := r.ReadByte()
	if err != nil {
		return nil, err
	}
	if ver != formatVersion {
		return nil, fmt.Errorf("unsupported delta format version %d", ver)
	}

	metaJSON, err := readBytes(r)
	if err != nil {
		return nil, err
	}
	var meta metaHeader
	if err := json.Unmarshal(metaJSON, &meta); err != nil {
		return nil, err
	}

	nRef, err := readUvarint(r)
	if err != nil {
		return nil, err
	}
	refNames := make([]string, nRef)
	for i := range refNames {
		b, err := readBytes(r)
		if err != nil {
			return nil, err
		}
		refNames[i] = string(b)
	}

	d := &Delta{
		Version: meta.Version, Assembly: meta.Assembly, Reference: meta.Reference,
		ReferenceHash: meta.ReferenceHash, RefManifest: meta.RefManifest,
		TotalBases: meta.TotalBases, LiteralBases: meta.LiteralBases,
	}
	if t, err := parseTime(meta.CreatedAt); err == nil {
		d.CreatedAt = t
	}

	nChrom, err := readUvarint(r)
	if err != nil {
		return nil, err
	}
	for i := uint64(0); i < nChrom; i++ {
		nameB, err := readBytes(r)
		if err != nil {
			return nil, err
		}
		length, err := readUvarint(r)
		if err != nil {
			return nil, err
		}
		hashB, err := readBytes(r)
		if err != nil {
			return nil, err
		}
		nOps, err := readUvarint(r)
		if err != nil {
			return nil, err
		}

		cd := ChromDelta{Name: string(nameB), Length: int(length), Hash: string(hashB)}
		cd.Ops = make([]Op, 0, nOps)
		var prevRefEnd int
		for j := uint64(0); j < nOps; j++ {
			tag, err := r.ReadByte()
			if err != nil {
				return nil, err
			}
			switch tag {
			case 0: // copy
				idx, err := readUvarint(r)
				if err != nil {
					return nil, err
				}
				if idx >= uint64(len(refNames)) {
					return nil, fmt.Errorf("chrom %s: ref index %d out of range", cd.Name, idx)
				}
				delta, err := readSvarint(r)
				if err != nil {
					return nil, err
				}
				length, err := readUvarint(r)
				if err != nil {
					return nil, err
				}
				start := prevRefEnd + int(delta)
				end := start + int(length)
				cd.Ops = append(cd.Ops, Op{Type: OpCopy, RefChrom: refNames[idx], RefStart: start, RefEnd: end})
				prevRefEnd = end
			case 1: // literal
				length, err := readUvarint(r)
				if err != nil {
					return nil, err
				}
				enc, err := r.ReadByte()
				if err != nil {
					return nil, err
				}
				var bs []byte
				switch enc {
				case 0:
					bs = make([]byte, length)
					if _, err := io.ReadFull(r, bs); err != nil {
						return nil, err
					}
				case 1:
					packed := make([]byte, (length+3)/4)
					if _, err := io.ReadFull(r, packed); err != nil {
						return nil, err
					}
					bs = unpack2bit(packed, int(length))
				default:
					return nil, fmt.Errorf("chrom %s: unknown literal encoding %d", cd.Name, enc)
				}
				cd.Ops = append(cd.Ops, Op{Type: OpLiteral, Bytes: bs})
			default:
				return nil, fmt.Errorf("chrom %s: unknown op tag %d", cd.Name, tag)
			}
		}
		d.Chromosomes = append(d.Chromosomes, cd)
	}
	return d, nil
}

// ── 2-bit packing ─────────────────────────────────────────────────────────────

func base2bit(b byte) (byte, bool) {
	switch b {
	case 'A':
		return 0, true
	case 'C':
		return 1, true
	case 'G':
		return 2, true
	case 'T':
		return 3, true
	}
	return 0, false
}

var bit2base = [4]byte{'A', 'C', 'G', 'T'}

// pack2bit packs ACGT bytes 4-per-byte. Returns ok=false if any byte is not ACGT.
func pack2bit(seq []byte) ([]byte, bool) {
	out := make([]byte, (len(seq)+3)/4)
	for i, b := range seq {
		code, ok := base2bit(b)
		if !ok {
			return nil, false
		}
		out[i/4] |= code << uint((i%4)*2)
	}
	return out, true
}

func unpack2bit(packed []byte, n int) []byte {
	out := make([]byte, n)
	for i := 0; i < n; i++ {
		code := (packed[i/4] >> uint((i%4)*2)) & 0x3
		out[i] = bit2base[code]
	}
	return out
}

// ── varint helpers ────────────────────────────────────────────────────────────

func writeUvarint(w *bufio.Writer, v uint64) {
	var buf [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(buf[:], v)
	w.Write(buf[:n])
}

func writeSvarint(w *bufio.Writer, v int64) {
	var buf [binary.MaxVarintLen64]byte
	n := binary.PutVarint(buf[:], v)
	w.Write(buf[:n])
}

func readUvarint(r *bufio.Reader) (uint64, error) { return binary.ReadUvarint(r) }
func readSvarint(r *bufio.Reader) (int64, error)  { return binary.ReadVarint(r) }

func writeBytes(w *bufio.Writer, b []byte) {
	writeUvarint(w, uint64(len(b)))
	w.Write(b)
}

func readBytes(r *bufio.Reader) ([]byte, error) {
	n, err := readUvarint(r)
	if err != nil {
		return nil, err
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return nil, err
	}
	return b, nil
}

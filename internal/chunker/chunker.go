package chunker

import "math/bits"

const (
	DefaultMinSize = 262144  // 256 KiB
	DefaultMaxSize = 1048576 // 1 MiB
)

// Config controls chunk size boundaries.
// Average chunk size targets (MinSize+MaxSize)/2 via the gear hash mask.
type Config struct {
	MinSize int
	MaxSize int
}

// Default returns the standard chunking config.
func Default() Config {
	return Config{MinSize: DefaultMinSize, MaxSize: DefaultMaxSize}
}

// mask derives a bit-mask targeting an average chunk size of (MinSize+MaxSize)/2.
func (c Config) mask() uint64 {
	avg := (c.MinSize + c.MaxSize) / 2
	b := bits.Len(uint(avg)) - 1 // floor(log2(avg))
	return uint64((1 << b) - 1)
}

// gearTable is a fixed, deterministic lookup table for the gear hash.
var gearTable [256]uint64

func init() {
	v := uint64(1)
	for i := range gearTable {
		v = v*6364136223846793005 + 1442695040888963407
		gearTable[i] = v
	}
}

// Split divides data into variable-size chunks using gear hash content-defined chunking.
// A boundary is cut when the rolling hash matches the mask, guaranteeing chunks stay
// within [MinSize, MaxSize]. Insertions only shift the one or two chunks around them.
func Split(data []byte, cfg Config) [][]byte {
	if len(data) == 0 {
		return nil
	}
	mask := cfg.mask()
	var chunks [][]byte
	start := 0
	var h uint64
	for i, b := range data {
		h = (h << 1) + gearTable[b]
		size := i - start + 1
		if size < cfg.MinSize {
			continue
		}
		if size >= cfg.MaxSize || h&mask == 0 {
			chunks = append(chunks, data[start:i+1])
			start = i + 1
			h = 0
		}
	}
	if start < len(data) {
		chunks = append(chunks, data[start:])
	}
	return chunks
}

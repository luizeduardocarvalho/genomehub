package aligner

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// Block is one aligned region from a PAF record.
type Block struct {
	QueryName   string  `json:"query_name"`
	QueryStart  int     `json:"query_start"`
	QueryEnd    int     `json:"query_end"`
	TargetName  string  `json:"target_name"`
	TargetStart int     `json:"target_start"`
	TargetEnd   int     `json:"target_end"`
	Matches     int     `json:"matches"`
	BlockLen    int     `json:"block_len"`
	Identity    float64 `json:"identity"`
	Strand      string  `json:"strand"`
	CIGAR       string  `json:"cigar,omitempty"`
}

// Config controls how minimap2 is invoked.
type Config struct {
	MinLength   int
	MinIdentity float64
	Preset      string // asm5 = same species, asm20 = cross species
	Threads     int
	WithCIGAR   bool // add -c --eqx for exact match extraction
}

func DefaultConfig() Config {
	return Config{
		MinLength:   1000,
		MinIdentity: 0.90,
		Preset:      "asm20",
		Threads:     4,
	}
}

// Run executes minimap2 and returns aligned blocks filtered by
// cfg.MinLength / cfg.MinIdentity.
func Run(targetFA, queryFA string, cfg Config) ([]Block, error) {
	return RunContext(context.Background(), targetFA, queryFA, cfg)
}

// RunContext is Run bound to a context: cancelling ctx kills the minimap2
// subprocess (so a worker shutting down does not orphan it).
func RunContext(ctx context.Context, targetFA, queryFA string, cfg Config) ([]Block, error) {
	raw, err := RunRawContext(ctx, targetFA, queryFA, cfg)
	if err != nil {
		return nil, err
	}
	return Filter(raw, cfg.MinLength, cfg.MinIdentity), nil
}

// RunRaw executes minimap2 and returns every parsed block, unfiltered.
// Only cfg.Preset, cfg.Threads and cfg.WithCIGAR affect the minimap2 invocation;
// MinLength / MinIdentity are applied later by Filter. This makes the raw result
// cacheable independent of the post-filter thresholds.
func RunRaw(targetFA, queryFA string, cfg Config) ([]Block, error) {
	return RunRawContext(context.Background(), targetFA, queryFA, cfg)
}

// RunRawContext is RunRaw bound to a context; cancelling ctx kills minimap2.
func RunRawContext(ctx context.Context, targetFA, queryFA string, cfg Config) ([]Block, error) {
	if _, err := exec.LookPath("minimap2"); err != nil {
		return nil, fmt.Errorf("minimap2 not found in PATH (brew install minimap2)")
	}

	args := []string{"-x", cfg.Preset, "-t", strconv.Itoa(cfg.Threads)}
	if cfg.WithCIGAR {
		args = append(args, "-c", "--eqx")
	}
	args = append(args, targetFA, queryFA)

	cmd := exec.CommandContext(ctx, "minimap2", args...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	blocks, parseErr := streamPAF(stdout)

	if err := cmd.Wait(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			return nil, fmt.Errorf("minimap2 failed: %s", ee.Stderr)
		}
		return nil, err
	}
	return blocks, parseErr
}

// Filter keeps blocks with BlockLen >= minLen and Identity >= minIdent,
// reporting filter counts to stderr.
func Filter(blocks []Block, minLen int, minIdent float64) []Block {
	kept := make([]Block, 0, len(blocks))
	shortFiltered, identFiltered := 0, 0
	for _, b := range blocks {
		if b.BlockLen < minLen {
			shortFiltered++
			continue
		}
		if b.Identity < minIdent {
			identFiltered++
			continue
		}
		kept = append(kept, b)
	}
	fmt.Fprintf(os.Stderr, "raw PAF blocks: %d  filtered (too short: %d, low identity: %d)  kept: %d\n",
		len(blocks), shortFiltered, identFiltered, len(kept))
	return kept
}

func streamPAF(r io.Reader) ([]Block, error) {
	var blocks []Block

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024*1024), 64*1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		b, err := parseLine(line)
		if err != nil {
			continue
		}
		blocks = append(blocks, b)
	}
	return blocks, scanner.Err()
}

func parseLine(line string) (Block, error) {
	f := strings.Split(line, "\t")
	if len(f) < 12 {
		return Block{}, fmt.Errorf("short PAF line: %d fields", len(f))
	}
	qstart, _ := strconv.Atoi(f[2])
	qend, _ := strconv.Atoi(f[3])
	tstart, _ := strconv.Atoi(f[7])
	tend, _ := strconv.Atoi(f[8])
	matches, _ := strconv.Atoi(f[9])
	blockLen, _ := strconv.Atoi(f[10])

	// matches/block_len is misleading — block_len spans gaps between chains.
	// Use the dv (divergence) tag for accurate identity when available.
	identity := 0.0
	if blockLen > 0 {
		identity = float64(matches) / float64(blockLen)
	}
	var cigar string
	for _, tag := range f[12:] {
		if strings.HasPrefix(tag, "dv:f:") {
			if dv, err := strconv.ParseFloat(strings.TrimPrefix(tag, "dv:f:"), 64); err == nil {
				identity = 1.0 - dv
			}
		}
		if strings.HasPrefix(tag, "cg:Z:") {
			cigar = strings.TrimPrefix(tag, "cg:Z:")
		}
	}

	return Block{
		QueryName:   f[0],
		QueryStart:  qstart,
		QueryEnd:    qend,
		TargetName:  f[5],
		TargetStart: tstart,
		TargetEnd:   tend,
		Matches:     matches,
		BlockLen:    blockLen,
		Identity:    identity,
		Strand:      f[4],
		CIGAR:       cigar,
	}, nil
}

// ── CIGAR parsing ─────────────────────────────────────────────────────────────

// CigarOp is one operation of a CIGAR string: Len bases of operation Op
// (one of =, X, I, D, N, S, H, M).
type CigarOp struct {
	Len int
	Op  byte
}

// ParseCIGAR splits a CIGAR string into its operations.
func ParseCIGAR(s string) []CigarOp {
	var ops []CigarOp
	i := 0
	for i < len(s) {
		j := i
		for j < len(s) && s[j] >= '0' && s[j] <= '9' {
			j++
		}
		if j >= len(s) {
			break
		}
		n, _ := strconv.Atoi(s[i:j])
		ops = append(ops, CigarOp{Len: n, Op: s[j]})
		i = j + 1
	}
	return ops
}

// ExtractExactMatches parses the CIGAR string of a block and returns runs of
// exact matches (= operations) >= minLen bases.
// Both + and - strand alignments are supported.
// For - strand: PAF query coordinates are forward-strand; the CIGAR is in
// the target direction, so qPos decrements from QueryEnd toward QueryStart.
func ExtractExactMatches(b Block, minLen int) []ExactMatch {
	if b.CIGAR == "" {
		return nil
	}

	rev := b.Strand == "-"
	var result []ExactMatch
	tPos := b.TargetStart
	qPos := b.QueryStart
	if rev {
		qPos = b.QueryEnd
	}
	runTStart, runQStart, runLen := 0, 0, 0

	advQ := func(n int) {
		if rev {
			qPos -= n
		} else {
			qPos += n
		}
	}

	flush := func() {
		if runLen >= minLen {
			var qStart, qEnd int
			if rev {
				// qPos has already decremented past the run; runQStart was set
				// before the first decrement, so the query region is
				// [runQStart-runLen, runQStart).
				qStart = runQStart - runLen
				qEnd = runQStart
			} else {
				qStart = runQStart
				qEnd = runQStart + runLen
			}
			result = append(result, ExactMatch{
				TargetStart: runTStart,
				TargetEnd:   runTStart + runLen,
				QueryStart:  qStart,
				QueryEnd:    qEnd,
			})
		}
		runLen = 0
	}

	for _, op := range ParseCIGAR(b.CIGAR) {
		switch op.Op {
		case '=':
			if runLen == 0 {
				runTStart = tPos
				runQStart = qPos
			}
			runLen += op.Len
			tPos += op.Len
			advQ(op.Len)
		case 'X':
			flush()
			tPos += op.Len
			advQ(op.Len)
		case 'I', 'S':
			flush()
			advQ(op.Len)
		case 'D', 'N':
			flush()
			tPos += op.Len
		case 'H':
			flush()
		}
	}
	flush()
	return result
}

// ── Summary ───────────────────────────────────────────────────────────────────

type Stats struct {
	Blocks      int
	TargetSpan  int
	AvgIdentity float64
}

func Summarise(blocks []Block) Stats {
	s := Stats{Blocks: len(blocks)}
	identitySum := 0.0
	for _, b := range blocks {
		s.TargetSpan += b.TargetEnd - b.TargetStart
		identitySum += b.Identity
	}
	if len(blocks) > 0 {
		s.AvgIdentity = identitySum / float64(len(blocks))
	}
	return s
}

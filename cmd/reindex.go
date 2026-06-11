package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/luizcarvalho/genome-hub/internal/aligner"
	"github.com/luizcarvalho/genome-hub/internal/chunker"
	"github.com/luizcarvalho/genome-hub/internal/fasta"
	"github.com/luizcarvalho/genome-hub/internal/index"
	"github.com/luizcarvalho/genome-hub/internal/manifest"
	"github.com/luizcarvalho/genome-hub/internal/store"
	"github.com/spf13/cobra"
)

var reindexCmd = &cobra.Command{
	Use:   "reindex",
	Short: "Rechunk N genomes at shared exact-match boundaries to maximise deduplication",
	Long: `Aligns every pair of genomes with minimap2, collects exact-match intervals for
each genome across all pairs, then rechunks each genome at the union of those
boundaries. Two or more --genome flags required.

Example (3-way):
  genomehub reindex \
    --genome TAIR10:tmp/tair10.fa \
    --genome Ler0:tmp/ler0.fna   \
    --genome Cvi0:tmp/cvi0.fna   \
    --min-exact 500`,
	RunE: runReindex,
}

var (
	reindexGenomes  []string
	reindexMinExact int
	reindexPreset   string
	reindexThreads  int
	reindexMinChunk int
	reindexMaxChunk int
	reindexNoCache  bool
)

func init() {
	reindexCmd.Flags().StringArrayVar(&reindexGenomes, "genome", nil, "name:path pair, repeat for each genome (minimum 2)")
	reindexCmd.Flags().IntVar(&reindexMinExact, "min-exact", 500, "minimum exact match length to use as a shared boundary")
	reindexCmd.Flags().StringVar(&reindexPreset, "preset", "asm20", "minimap2 preset")
	reindexCmd.Flags().IntVar(&reindexThreads, "threads", 4, "minimap2 threads per alignment")
	reindexCmd.Flags().IntVar(&reindexMinChunk, "min-chunk", chunker.DefaultMinSize, "minimum chunk size for non-shared regions")
	reindexCmd.Flags().IntVar(&reindexMaxChunk, "max-chunk", chunker.DefaultMaxSize, "maximum chunk size")
	reindexCmd.Flags().BoolVar(&reindexNoCache, "no-cache", false, "ignore cached alignments and re-run minimap2 (still updates cache)")
	reindexCmd.MarkFlagRequired("genome")
	rootCmd.AddCommand(reindexCmd)
}

// genomeEntry holds per-genome state accumulated during multi-way reindex.
type genomeEntry struct {
	name   string
	path   string
	chroms []fasta.Chromosome
	seqMap map[string][]byte
	cuts   map[string]map[int]bool // chr → propagated boundary positions
	cover  map[string][][2]int     // chr → exact-match intervals (for stats)
}

// matchPair records a pairwise exact-match correspondence used for boundary propagation.
// After propagation, any cut inside genome i's interval is mirrored into genome j and
// vice versa, so corresponding sub-regions produce the same gear-hash chunks.
type matchPair struct {
	iIdx         int
	iChrom       string
	iStart, iEnd int
	jIdx         int
	jChrom       string
	jStart, jEnd int
}

// region is a contiguous slice of a chromosome sequence.
type region struct {
	Start  int
	End    int
	Shared bool
}

func runReindex(_ *cobra.Command, _ []string) error {
	// ── Parse --genome flags ──────────────────────────────────
	genomes, err := parseGenomeFlags(reindexGenomes)
	if err != nil {
		return err
	}
	if len(genomes) < 2 {
		return fmt.Errorf("need at least 2 --genome flags, got %d", len(genomes))
	}

	chunkCfg := chunker.Config{MinSize: reindexMinChunk, MaxSize: reindexMaxChunk}
	alnCfg := aligner.Config{
		MinLength:   reindexMinExact,
		MinIdentity: 0.0,
		Preset:      reindexPreset,
		Threads:     reindexThreads,
		WithCIGAR:   true,
	}

	// ── Load all FASTAs ───────────────────────────────────────
	fmt.Fprintln(os.Stderr, "loading FASTAs...")
	for i := range genomes {
		chroms, err := fasta.Read(genomes[i].path)
		if err != nil {
			return fmt.Errorf("read %s: %w", genomes[i].name, err)
		}
		genomes[i].chroms = chroms
		genomes[i].seqMap = chromMap(chroms)
		genomes[i].cuts = map[string]map[int]bool{}
		genomes[i].cover = map[string][][2]int{}
	}

	// ── All-pairs alignment ───────────────────────────────────
	// Each pair contributes exact-match endpoints to both genomes' cut sets,
	// and the full intervals to their coverage maps (used only for stats).
	var allPairs []matchPair
	fileHashes := map[string]string{} // path → content hash, memoised across pairs
	pairs := len(genomes) * (len(genomes) - 1) / 2
	pairNum := 0
	for i := 0; i < len(genomes); i++ {
		for j := i + 1; j < len(genomes); j++ {
			pairNum++
			fmt.Fprintf(os.Stderr, "\naligning %s vs %s (%d/%d)...\n",
				genomes[i].name, genomes[j].name, pairNum, pairs)

			blocks, err := alignCached(genomes[i].path, genomes[j].path, alnCfg, fileHashes)
			if err != nil {
				return fmt.Errorf("align %s vs %s: %w", genomes[i].name, genomes[j].name, err)
			}

			for _, b := range blocks {
				for _, em := range aligner.ExtractExactMatches(b, reindexMinExact) {
					allPairs = append(allPairs, matchPair{
						iIdx: i, iChrom: b.TargetName, iStart: em.TargetStart, iEnd: em.TargetEnd,
						jIdx: j, jChrom: b.QueryName, jStart: em.QueryStart, jEnd: em.QueryEnd,
					})
					addCut(genomes[i].cuts, b.TargetName, em.TargetStart)
					addCut(genomes[i].cuts, b.TargetName, em.TargetEnd)
					addCut(genomes[j].cuts, b.QueryName, em.QueryStart)
					addCut(genomes[j].cuts, b.QueryName, em.QueryEnd)
					genomes[i].cover[b.TargetName] = append(genomes[i].cover[b.TargetName],
						[2]int{em.TargetStart, em.TargetEnd})
					genomes[j].cover[b.QueryName] = append(genomes[j].cover[b.QueryName],
						[2]int{em.QueryStart, em.QueryEnd})
				}
			}
		}
	}

	// ── Propagate cuts across match correspondences ───────────
	// Ensures that if genome A has a cut at x inside match A[a:b]↔B[c:d],
	// genome B gets a cut at c+(x-a). Without this, different pairwise
	// alignments can extend a genome's interval beyond what its partner sees,
	// causing the chunks to have different lengths and thus different hashes.
	fmt.Fprintln(os.Stderr, "\npropagating cuts...")
	propagateCuts(genomes, allPairs)

	// ── Open store + index ────────────────────────────────────
	s, err := store.Open(storeDir)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer s.Close()

	idx, err := index.Open(indexDir())
	if err != nil {
		return fmt.Errorf("open index: %w", err)
	}
	defer idx.Close()

	// ── Clear stale index entries for these genomes ──────────────
	for _, g := range genomes {
		if err := idx.RemoveGenome(g.name); err != nil {
			return fmt.Errorf("clear index for %s: %w", g.name, err)
		}
	}

	// ── Rechunk each genome at its propagated boundaries ─────────
	type result struct {
		m     *manifest.Manifest
		stats rechunkStats
		out   string
	}
	results := make([]result, len(genomes))

	for i, g := range genomes {
		fmt.Fprintf(os.Stderr, "\nrechunking %s...\n", g.name)
		m, stats, err := rechunkGenome(g.chroms, g.seqMap, g.cuts, g.cover, g.name, s, chunkCfg)
		if err != nil {
			return fmt.Errorf("rechunk %s: %w", g.name, err)
		}
		if err := indexManifest(idx, m); err != nil {
			return fmt.Errorf("index %s: %w", g.name, err)
		}
		out := g.name + ".manifest.json"
		if err := m.Write(out); err != nil {
			return err
		}
		results[i] = result{m, stats, out}
	}

	// ── Summary ───────────────────────────────────────────────
	fmt.Println()
	var outNames []string
	for i, g := range genomes {
		r := results[i]
		exactBP := countExactBP(g.cover)
		pct := 0.0
		if r.stats.bases > 0 {
			pct = float64(exactBP) / float64(r.stats.bases) * 100
		}
		fmt.Printf("%-12s  %6d segments  %s  exact match: %s (%.1f%%)\n",
			g.name, r.stats.segments,
			fmtBP(r.stats.bases),
			fmtBP(exactBP), pct)
		outNames = append(outNames, r.out)
	}
	fmt.Printf("\nmanifests: %s\n", strings.Join(outNames, "  "))
	return nil
}

// parseGenomeFlags parses ["name:path", ...] into genomeEntry slice.
func parseGenomeFlags(flags []string) ([]genomeEntry, error) {
	var genomes []genomeEntry
	seen := map[string]bool{}
	for _, f := range flags {
		parts := strings.SplitN(f, ":", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return nil, fmt.Errorf("--genome %q: expected name:path", f)
		}
		name, path := parts[0], parts[1]
		if seen[name] {
			return nil, fmt.Errorf("duplicate genome name %q", name)
		}
		seen[name] = true
		genomes = append(genomes, genomeEntry{name: name, path: path})
	}
	return genomes, nil
}

// alignCached returns minimap2 blocks for a genome pair, reusing a cached raw
// alignment when one exists. The cache key covers only what changes the minimap2
// output (file contents, preset, CIGAR flag); MinLength / MinIdentity are applied
// after load via aligner.Filter, so tuning --min-exact never forces a re-align.
func alignCached(targetFA, queryFA string, cfg aligner.Config, hashes map[string]string) ([]aligner.Block, error) {
	th, err := fileHash(targetFA, hashes)
	if err != nil {
		return nil, err
	}
	qh, err := fileHash(queryFA, hashes)
	if err != nil {
		return nil, err
	}

	cig := "nocig"
	if cfg.WithCIGAR {
		cig = "cig"
	}
	key := fmt.Sprintf("%s_%s_%s_%s", th, qh, cfg.Preset, cig)
	path := filepath.Join(alnCacheDir(), key+".json")

	if !reindexNoCache {
		if data, err := os.ReadFile(path); err == nil {
			var raw []aligner.Block
			if json.Unmarshal(data, &raw) == nil {
				fmt.Fprintf(os.Stderr, "  alignment cache hit (%d blocks)\n", len(raw))
				return aligner.Filter(raw, cfg.MinLength, cfg.MinIdentity), nil
			}
			// corrupt cache entry: fall through and recompute
		}
	}

	raw, err := aligner.RunRaw(targetFA, queryFA, cfg)
	if err != nil {
		return nil, err
	}
	if err := writeAlnCache(path, raw); err != nil {
		fmt.Fprintf(os.Stderr, "  warning: could not write alignment cache: %v\n", err)
	}
	return aligner.Filter(raw, cfg.MinLength, cfg.MinIdentity), nil
}

// fileHash returns the BLAKE3 hash of a file's contents, memoised in hashes.
func fileHash(path string, hashes map[string]string) (string, error) {
	if h, ok := hashes[path]; ok {
		return h, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	h := store.HashBytes(data)
	hashes[path] = h
	return h, nil
}

// writeAlnCache atomically writes raw blocks to the cache path.
func writeAlnCache(path string, raw []aligner.Block) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// addCut adds pos to the cut-point set for chrom.
func addCut(cuts map[string]map[int]bool, chrom string, pos int) {
	if cuts[chrom] == nil {
		cuts[chrom] = map[int]bool{}
	}
	cuts[chrom][pos] = true
}

// cutRef identifies one cut point: which genome, which chromosome, what position.
type cutRef struct {
	idx   int
	chrom string
	pos   int
}

// sideRef points at one side of a matchPair from the perspective of a (genome,chrom)
// key, so a cut on that side can be mirrored onto the partner side in O(1).
type sideRef struct {
	p     *matchPair
	mineI bool // true: this key is the pair's i side; false: the j side
}

// propagateCuts mirrors interior cut points through pairwise match correspondences
// until no new cuts are introduced. For a match A[a:b]↔B[c:d], any cut at x
// inside A's interval maps to c+(x-a) in B, guaranteeing both genomes produce
// sub-regions of equal length and identical bytes — and therefore the same hash.
//
// Implemented as a worklist over individual cuts rather than a re-scan of every
// cut on every round. Each cut is dequeued once; only the pairs that actually
// touch that (genome,chrom) are inspected, via the byKey index. New cuts are
// enqueued as they are discovered. This is O(newCuts × pairsPerChrom) instead of
// the O(rounds × P × C²) of the naive fixpoint.
func propagateCuts(genomes []genomeEntry, pairs []matchPair) {
	// Index pairs by the (genome,chrom) key on each side.
	byKey := map[cutRef][]sideRef{}
	keyOf := func(idx int, chrom string) cutRef { return cutRef{idx: idx, chrom: chrom} }
	for k := range pairs {
		p := &pairs[k]
		ik := keyOf(p.iIdx, p.iChrom)
		jk := keyOf(p.jIdx, p.jChrom)
		byKey[ik] = append(byKey[ik], sideRef{p: p, mineI: true})
		byKey[jk] = append(byKey[jk], sideRef{p: p, mineI: false})
	}

	// Seed worklist with every existing cut.
	queue := make([]cutRef, 0)
	for idx := range genomes {
		for chrom, set := range genomes[idx].cuts {
			for pos := range set {
				queue = append(queue, cutRef{idx, chrom, pos})
			}
		}
	}

	processed := 0
	for len(queue) > 0 {
		c := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		processed++
		if processed%500000 == 0 {
			fmt.Fprintf(os.Stderr, "  propagated %d cuts, %d queued...\n", processed, len(queue))
		}

		for _, sr := range byKey[keyOf(c.idx, c.chrom)] {
			p := sr.p
			var srcStart, srcEnd, dstStart int
			var dstIdx int
			var dstChrom string
			if sr.mineI {
				srcStart, srcEnd, dstStart = p.iStart, p.iEnd, p.jStart
				dstIdx, dstChrom = p.jIdx, p.jChrom
			} else {
				srcStart, srcEnd, dstStart = p.jStart, p.jEnd, p.iStart
				dstIdx, dstChrom = p.iIdx, p.iChrom
			}
			if c.pos <= srcStart || c.pos >= srcEnd {
				continue // not interior to this match
			}
			y := dstStart + (c.pos - srcStart)
			dm := genomes[dstIdx].cuts[dstChrom]
			if dm != nil && dm[y] {
				continue // already present
			}
			addCut(genomes[dstIdx].cuts, dstChrom, y)
			queue = append(queue, cutRef{dstIdx, dstChrom, y})
		}
	}
}

// countExactBP returns the total base-pairs covered by merged exact-match intervals.
func countExactBP(cover map[string][][2]int) int {
	total := 0
	for _, ivs := range cover {
		for _, iv := range mergeIntervals(ivs) {
			total += iv[1] - iv[0]
		}
	}
	return total
}

type rechunkStats struct {
	segments int
	bases    int
}

func rechunkGenome(
	chroms []fasta.Chromosome,
	seqMap map[string][]byte,
	cutsMap map[string]map[int]bool,
	coverMap map[string][][2]int,
	assembly string,
	s *store.Store,
	cfg chunker.Config,
) (*manifest.Manifest, rechunkStats, error) {
	var manifestChroms []manifest.Chromosome
	var stats rechunkStats

	for _, chrom := range chroms {
		seq := seqMap[chrom.Name]
		chromHash := "blake3:" + store.HashBytes(seq)

		regions := buildRegionsFromCuts(len(seq), cutsMap[chrom.Name], coverMap[chrom.Name])
		fmt.Fprintf(os.Stderr, "  %s: %d regions (%d exact match)\n",
			chrom.Name, len(regions), countShared(regions))

		var segments []manifest.Segment
		for _, r := range regions {
			sub := seq[r.Start:r.End]
			chunks := chunker.Split(sub, cfg)
			for _, chunk := range chunks {
				hash, err := s.Put(chunk)
				if err != nil {
					return nil, stats, err
				}
				segments = append(segments, manifest.Segment{
					Hash:   "blake3:" + hash,
					Length: len(chunk),
				})
			}
		}

		manifestChroms = append(manifestChroms, manifest.Chromosome{
			Name:     chrom.Name,
			Length:   len(seq),
			Hash:     chromHash,
			Segments: segments,
		})
		stats.segments += len(segments)
		stats.bases += len(seq)
	}

	m := &manifest.Manifest{
		Version:      1,
		GraphVersion: 2,
		Assembly:     assembly,
		TotalBases:   stats.bases,
		Encoding:     "raw-ascii",
		Chunking: manifest.Chunking{
			Algorithm: "gear+mem",
			MinSize:   cfg.MinSize,
			MaxSize:   cfg.MaxSize,
		},
		CreatedAt:    time.Now().UTC(),
		SegmentsRoot: manifest.ComputeSegmentsRoot(manifestChroms),
		Chromosomes:  manifestChroms,
	}
	return m, stats, nil
}

// buildRegionsFromCuts builds regions from propagated cut points.
// Sub-intervals fully contained within a covered interval are marked Shared.
func buildRegionsFromCuts(chrLen int, cuts map[int]bool, covered [][2]int) []region {
	pts := make([]int, 0, len(cuts)+2)
	pts = append(pts, 0)
	for p := range cuts {
		if p > 0 && p < chrLen {
			pts = append(pts, p)
		}
	}
	pts = append(pts, chrLen)
	sort.Ints(pts)
	// deduplicate
	deduped := pts[:1]
	for _, p := range pts[1:] {
		if p != deduped[len(deduped)-1] {
			deduped = append(deduped, p)
		}
	}
	pts = deduped

	merged := mergeIntervals(covered)
	mi := 0
	var regions []region
	for k := 0; k+1 < len(pts); k++ {
		start, end := pts[k], pts[k+1]
		for mi < len(merged) && merged[mi][1] <= start {
			mi++
		}
		shared := mi < len(merged) && merged[mi][0] <= start && end <= merged[mi][1]
		regions = append(regions, region{start, end, shared})
	}
	return regions
}

// mergeIntervals merges overlapping [start,end) intervals sorted by start.
func mergeIntervals(ivs [][2]int) [][2]int {
	if len(ivs) == 0 {
		return nil
	}
	sort.Slice(ivs, func(i, j int) bool { return ivs[i][0] < ivs[j][0] })
	merged := [][2]int{ivs[0]}
	for _, iv := range ivs[1:] {
		last := &merged[len(merged)-1]
		if iv[0] <= last[1] {
			if iv[1] > last[1] {
				last[1] = iv[1]
			}
		} else {
			merged = append(merged, iv)
		}
	}
	return merged
}

func chromMap(chroms []fasta.Chromosome) map[string][]byte {
	m := map[string][]byte{}
	for _, c := range chroms {
		m[c.Name] = c.Sequence
	}
	return m
}

func countShared(regions []region) int {
	n := 0
	for _, r := range regions {
		if r.Shared {
			n++
		}
	}
	return n
}

// indexManifest records all segments in m into the index under m.Assembly.
func indexManifest(idx *index.Index, m *manifest.Manifest) error {
	for _, chrom := range m.Chromosomes {
		for _, seg := range chrom.Segments {
			hash := strings.TrimPrefix(seg.Hash, "blake3:")
			if err := idx.Put(hash, seg.Length, m.Assembly); err != nil {
				return err
			}
		}
	}
	return nil
}

// normName returns the first word of a FASTA header (minimap2 naming convention).
func normName(name string) string {
	return strings.Fields(name)[0]
}

package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/luizcarvalho/genome-hub/internal/chunker"
	"github.com/luizcarvalho/genome-hub/internal/fasta"
	"github.com/luizcarvalho/genome-hub/internal/store"
	"github.com/spf13/cobra"
)

var benchmarkCmd = &cobra.Command{
	Use:   "benchmark",
	Short: "Measure deduplication ratio and import speed across chunk sizes",
	RunE:  runBenchmark,
}

var (
	benchFastaA string
	benchFastaB string
	benchSizes  string
	benchOutput string
)

func init() {
	benchmarkCmd.Flags().StringVar(&benchFastaA, "fasta-a", "", "first FASTA file (required)")
	benchmarkCmd.Flags().StringVar(&benchFastaB, "fasta-b", "", "second FASTA file (required)")
	benchmarkCmd.Flags().StringVar(&benchSizes, "chunk-sizes",
		"4096,8192,16384,32768,65536,131072,262144,524288,1048576",
		"comma-separated max chunk sizes in bytes")
	benchmarkCmd.Flags().StringVar(&benchOutput, "output", "benchmark.json", "output JSON file")
	benchmarkCmd.MarkFlagRequired("fasta-a")
	benchmarkCmd.MarkFlagRequired("fasta-b")
	rootCmd.AddCommand(benchmarkCmd)
}

type BenchmarkResult struct {
	MaxChunkSize   int     `json:"max_chunk_size"`
	MinChunkSize   int     `json:"min_chunk_size"`
	SegmentsA      int     `json:"segments_a"`
	SegmentsB      int     `json:"segments_b"`
	UniqueSegments int     `json:"unique_segments"`
	SharedSegments int     `json:"shared_segments"`
	DedupRatio     float64 `json:"dedup_ratio"`
	AvgChunkBytesA int     `json:"avg_chunk_bytes_a"`
	AvgChunkBytesB int     `json:"avg_chunk_bytes_b"`
	ImportAMs      int64   `json:"import_a_ms"`
	ImportBMs      int64   `json:"import_b_ms"`
}

func runBenchmark(_ *cobra.Command, _ []string) error {
	sizes, err := parseBenchChunkSizes(benchSizes)
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "loading FASTAs...\n")
	chromsA, err := fasta.Read(benchFastaA)
	if err != nil {
		return fmt.Errorf("read fasta-a: %w", err)
	}
	chromsB, err := fasta.Read(benchFastaB)
	if err != nil {
		return fmt.Errorf("read fasta-b: %w", err)
	}

	fmt.Fprintf(os.Stderr, "%-12s  %-10s  %-10s  %-10s  %-10s  %s\n",
		"max_chunk", "segs_a", "segs_b", "shared", "dedup%", "time_a+b")

	var results []BenchmarkResult
	for _, maxSize := range sizes {
		minSize := maxSize / 4
		if minSize < 1 {
			minSize = 1
		}
		cfg := chunker.Config{MinSize: minSize, MaxSize: maxSize}

		t0 := time.Now()
		hashesA, bytesA := benchChunkHashes(chromsA, cfg)
		importAMs := time.Since(t0).Milliseconds()

		t0 = time.Now()
		hashesB, bytesB := benchChunkHashes(chromsB, cfg)
		importBMs := time.Since(t0).Milliseconds()

		shared := benchIntersectCount(hashesA, hashesB)
		unique := len(hashesA) + len(hashesB) - shared

		avgA, avgB := 0, 0
		if len(hashesA) > 0 {
			avgA = bytesA / len(hashesA)
		}
		if len(hashesB) > 0 {
			avgB = bytesB / len(hashesB)
		}

		dedup := 0.0
		if len(hashesA)+len(hashesB) > 0 {
			dedup = float64(shared) / float64(len(hashesA)+len(hashesB))
		}

		results = append(results, BenchmarkResult{
			MaxChunkSize:   maxSize,
			MinChunkSize:   minSize,
			SegmentsA:      len(hashesA),
			SegmentsB:      len(hashesB),
			UniqueSegments: unique,
			SharedSegments: shared,
			DedupRatio:     dedup,
			AvgChunkBytesA: avgA,
			AvgChunkBytesB: avgB,
			ImportAMs:      importAMs,
			ImportBMs:      importBMs,
		})

		fmt.Fprintf(os.Stderr, "%-12d  %-10d  %-10d  %-10d  %-9.1f%%  %dms+%dms\n",
			maxSize, len(hashesA), len(hashesB), shared, dedup*100, importAMs, importBMs)
	}

	data, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(benchOutput, data, 0644); err != nil {
		return fmt.Errorf("write output: %w", err)
	}
	fmt.Printf("wrote %s\n", benchOutput)
	return nil
}

// benchChunkHashes hashes all chunks for all chromosomes without writing to disk.
// Returns (hashes, total bytes processed).
func benchChunkHashes(chroms []fasta.Chromosome, cfg chunker.Config) ([]string, int) {
	var hashes []string
	total := 0
	for _, c := range chroms {
		for _, chunk := range chunker.Split(c.Sequence, cfg) {
			hashes = append(hashes, store.HashBytes(chunk))
			total += len(chunk)
		}
	}
	return hashes, total
}

// benchIntersectCount returns how many of b's hashes exist in a.
func benchIntersectCount(a, b []string) int {
	set := make(map[string]struct{}, len(a))
	for _, h := range a {
		set[h] = struct{}{}
	}
	count := 0
	for _, h := range b {
		if _, ok := set[h]; ok {
			count++
		}
	}
	return count
}

func parseBenchChunkSizes(s string) ([]int, error) {
	var sizes []int
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, fmt.Errorf("invalid chunk size %q", p)
		}
		if n < 1 {
			return nil, fmt.Errorf("chunk size must be >= 1, got %d", n)
		}
		sizes = append(sizes, n)
	}
	return sizes, nil
}

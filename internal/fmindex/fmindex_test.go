package fmindex_test

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/luizcarvalho/genome-hub/internal/fmindex"
)

func must(target, query string, minLen int) string {
	f := fmindex.New([]byte(target), []byte(query))
	ms := f.FindMEMs(minLen)
	parts := make([]string, len(ms))
	for i, m := range ms {
		parts[i] = fmt.Sprintf("T[%d:%d] Q[%d:%d] len=%d seq=%q",
			m.TargetStart, m.TargetEnd,
			m.QueryStart, m.QueryEnd,
			m.Len(), target[m.TargetStart:m.TargetEnd])
	}
	return strings.Join(parts, "\n")
}

func matchCount(target, query string, minLen int) int {
	f := fmindex.New([]byte(target), []byte(query))
	return len(f.FindMEMs(minLen))
}

func TestIdenticalSequences(t *testing.T) {
	seq := "ACGTACGTACGT"
	f := fmindex.New([]byte(seq), []byte(seq))
	ms := f.FindMEMs(len(seq))
	if len(ms) == 0 {
		t.Fatal("expected at least one full-length MEM for identical sequences")
	}
	for _, m := range ms {
		if m.Len() != len(seq) {
			continue
		}
		if m.TargetStart == 0 && m.QueryStart == 0 {
			return
		}
	}
	t.Fatalf("did not find full-length identity match; got:\n%s", must(seq, seq, len(seq)))
}

func TestSingleSNP(t *testing.T) {
	target := "ACGTACGTACGT"
	query := "ACGTAXGTACGT" // X is a mismatch at pos 5
	f := fmindex.New([]byte(target), []byte(query))
	ms := f.FindMEMs(3)
	t.Logf("matches:\n%s", must(target, query, 3))

	found5, found6 := false, false
	for _, m := range ms {
		if m.TargetStart == 0 && m.TargetEnd == 5 {
			found5 = true
		}
		if m.TargetStart == 6 && m.TargetEnd == 12 {
			found6 = true
		}
	}
	if !found5 {
		t.Error("expected MEM T[0:5] / Q[0:5]")
	}
	if !found6 {
		t.Error("expected MEM T[6:12] / Q[6:12]")
	}
}

func TestNoMatch(t *testing.T) {
	if matchCount("AAAAAAAAAA", "TTTTTTTTTT", 4) != 0 {
		t.Error("expected zero matches between A-only and T-only sequences")
	}
}

func TestMinLenFiltering(t *testing.T) {
	target := "ACGTACGT"
	query := "ACGTXXXX"
	n4 := matchCount(target, query, 4)
	n5 := matchCount(target, query, 5)
	if n5 > n4 {
		t.Errorf("longer minLen should produce <= matches: minLen=4 gave %d, minLen=5 gave %d", n4, n5)
	}
}

func TestMaximalityNotDominated(t *testing.T) {
	target := "ACGTACGTACGTACGT"
	query := "ACGTACGTACGTACGT"
	f := fmindex.New([]byte(target), []byte(query))
	ms := f.FindMEMs(4)
	for i, a := range ms {
		for j, b := range ms {
			if i == j {
				continue
			}
			if b.QueryStart <= a.QueryStart && b.QueryEnd >= a.QueryEnd &&
				b.TargetStart <= a.TargetStart && b.TargetEnd >= a.TargetEnd &&
				b.Len() > a.Len() {
				t.Errorf("match %v is dominated by %v — not maximal", a, b)
			}
		}
	}
}

func TestSorted(t *testing.T) {
	target := "GCATGCATGCAT"
	query := "GCATGCATGCAT"
	f := fmindex.New([]byte(target), []byte(query))
	ms := f.FindMEMs(3)
	if !sort.SliceIsSorted(ms, func(i, j int) bool {
		if ms[i].TargetStart != ms[j].TargetStart {
			return ms[i].TargetStart < ms[j].TargetStart
		}
		return ms[i].QueryStart < ms[j].QueryStart
	}) {
		t.Error("FindMEMs result is not sorted")
	}
}

func TestBoundsWithinSequences(t *testing.T) {
	target := "ACGTACGT"
	query := "TACGTAAA"
	f := fmindex.New([]byte(target), []byte(query))
	ms := f.FindMEMs(3)
	for _, m := range ms {
		if m.TargetStart < 0 || m.TargetEnd > len(target) {
			t.Errorf("target bounds out of range: %v", m)
		}
		if m.QueryStart < 0 || m.QueryEnd > len(query) {
			t.Errorf("query bounds out of range: %v", m)
		}
		ts := target[m.TargetStart:m.TargetEnd]
		qs := query[m.QueryStart:m.QueryEnd]
		if ts != qs {
			t.Errorf("content mismatch at %v: target=%q query=%q", m, ts, qs)
		}
	}
}

func TestRepeats(t *testing.T) {
	unit := "ACGTAC"
	target := strings.Repeat(unit, 3)
	query := strings.Repeat(unit, 2)
	f := fmindex.New([]byte(target), []byte(query))
	ms := f.FindMEMs(len(unit))
	if len(ms) == 0 {
		t.Fatal("expected MEMs for repeated unit sequence")
	}
	t.Logf("repeat MEMs (%d):\n%s", len(ms), must(target, query, len(unit)))
}

func TestGenomicSNPPattern(t *testing.T) {
	const seqLen = 1000
	const snpInterval = 115
	const minMEM = 50

	bases := []byte("ACGT")
	target := make([]byte, seqLen)
	for i := range target {
		target[i] = bases[i%4]
	}
	query := make([]byte, seqLen)
	copy(query, target)
	for pos := snpInterval; pos < seqLen; pos += snpInterval {
		query[pos] = bases[(int(query[pos]-'A')+1)%4]
	}

	f := fmindex.New(target, query)
	ms := f.FindMEMs(minMEM)
	t.Logf("genomic SNP pattern: %d MEMs >= %d bp", len(ms), minMEM)

	for _, m := range ms {
		if string(target[m.TargetStart:m.TargetEnd]) != string(query[m.QueryStart:m.QueryEnd]) {
			t.Errorf("content mismatch at %v", m)
		}
	}
	if len(ms) == 0 {
		t.Error("expected at least some MEMs in 87%-identity synthetic sequences")
	}
}

func BenchmarkFMIndexBuild(b *testing.B) {
	seq := make([]byte, 100_000)
	bases := []byte("ACGT")
	for i := range seq {
		seq[i] = bases[i%4]
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		fmindex.New(seq, seq[:50_000])
	}
}

func BenchmarkFindMEMs(b *testing.B) {
	const seqLen = 100_000
	target := make([]byte, seqLen)
	query := make([]byte, seqLen)
	bases := []byte("ACGT")
	for i := range target {
		target[i] = bases[i%4]
	}
	copy(query, target)
	for i := 100; i < seqLen; i += 115 {
		query[i] = bases[(i/115+1)%4]
	}
	f := fmindex.New(target, query)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		f.FindMEMs(50)
	}
}

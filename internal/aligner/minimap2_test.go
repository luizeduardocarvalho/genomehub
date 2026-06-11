package aligner

import (
	"testing"
)

func TestParseCIGARStr(t *testing.T) {
	ops := ParseCIGAR("50=1X30=2I10D")
	want := []CigarOp{{50, '='}, {1, 'X'}, {30, '='}, {2, 'I'}, {10, 'D'}}
	if len(ops) != len(want) {
		t.Fatalf("got %d ops, want %d: %v", len(ops), len(want), ops)
	}
	for i, o := range ops {
		if o != want[i] {
			t.Errorf("op[%d]: got %v, want %v", i, o, want[i])
		}
	}
}

func TestParseCIGARStrEmpty(t *testing.T) {
	if ops := ParseCIGAR(""); len(ops) != 0 {
		t.Errorf("empty CIGAR: got %v", ops)
	}
}

func TestExtractExactMatches(t *testing.T) {
	tests := []struct {
		name   string
		block  Block
		minLen int
		want   []ExactMatch
	}{
		{
			name:   "no CIGAR",
			block:  Block{TargetStart: 0, QueryStart: 0, QueryEnd: 100, Strand: "+"},
			minLen: 10,
			want:   nil,
		},
		{
			name: "all exact forward",
			block: Block{
				TargetStart: 0, TargetEnd: 100,
				QueryStart: 0, QueryEnd: 100,
				Strand: "+", CIGAR: "100=",
			},
			minLen: 10,
			want:   []ExactMatch{{0, 100, 0, 100}},
		},
		{
			name: "SNP splits into two matches",
			block: Block{
				TargetStart: 0, TargetEnd: 101,
				QueryStart: 0, QueryEnd: 101,
				Strand: "+", CIGAR: "50=1X50=",
			},
			minLen: 10,
			want:   []ExactMatch{{0, 50, 0, 50}, {51, 101, 51, 101}},
		},
		{
			name: "matches below minLen filtered",
			block: Block{
				TargetStart: 0, TargetEnd: 21,
				QueryStart: 0, QueryEnd: 21,
				Strand: "+", CIGAR: "10=1X10=",
			},
			minLen: 50,
			want:   nil,
		},
		{
			name: "insertion shifts query not target",
			block: Block{
				TargetStart: 0, TargetEnd: 50,
				QueryStart: 0, QueryEnd: 60,
				Strand: "+", CIGAR: "20=10I30=",
			},
			minLen: 10,
			want:   []ExactMatch{{0, 20, 0, 20}, {20, 50, 30, 60}},
		},
		{
			name: "deletion shifts target not query",
			block: Block{
				TargetStart: 0, TargetEnd: 60,
				QueryStart: 0, QueryEnd: 50,
				Strand: "+", CIGAR: "20=10D30=",
			},
			minLen: 10,
			want:   []ExactMatch{{0, 20, 0, 20}, {30, 60, 20, 50}},
		},
		{
			name: "reverse strand reverses query coordinates",
			block: Block{
				TargetStart: 0, TargetEnd: 51,
				QueryStart: 0, QueryEnd: 51,
				Strand: "-", CIGAR: "30=1X20=",
			},
			minLen: 10,
			want:   []ExactMatch{{0, 30, 21, 51}, {31, 51, 0, 20}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractExactMatches(tt.block, tt.minLen)
			if len(got) != len(tt.want) {
				t.Fatalf("got %d matches, want %d\n  got:  %v\n  want: %v",
					len(got), len(tt.want), got, tt.want)
			}
			for i, g := range got {
				if g != tt.want[i] {
					t.Errorf("match[%d]: got %v, want %v", i, g, tt.want[i])
				}
			}
		})
	}
}

func TestParseLine(t *testing.T) {
	line := "read1\t1000\t0\t500\t+\tchr1\t5000\t100\t600\t490\t500\t60"
	b, err := parseLine(line)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if b.QueryName != "read1" {
		t.Errorf("QueryName: got %q", b.QueryName)
	}
	if b.TargetName != "chr1" {
		t.Errorf("TargetName: got %q", b.TargetName)
	}
	if b.QueryStart != 0 || b.QueryEnd != 500 {
		t.Errorf("query coords: %d-%d", b.QueryStart, b.QueryEnd)
	}
	if b.TargetStart != 100 || b.TargetEnd != 600 {
		t.Errorf("target coords: %d-%d", b.TargetStart, b.TargetEnd)
	}
	if b.Strand != "+" {
		t.Errorf("strand: %q", b.Strand)
	}
	if want := 490.0 / 500.0; b.Identity != want {
		t.Errorf("identity: got %f, want %f", b.Identity, want)
	}
}

func TestParseLineDivergenceTag(t *testing.T) {
	line := "read1\t1000\t0\t500\t+\tchr1\t5000\t100\t600\t490\t500\t60\tdv:f:0.05"
	b, err := parseLine(line)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := 0.95; b.Identity != want {
		t.Errorf("identity: got %f, want %f", b.Identity, want)
	}
}

func TestParseLineCIGARTag(t *testing.T) {
	line := "read1\t1000\t0\t100\t+\tchr1\t5000\t0\t100\t99\t100\t60\tcg:Z:50=1X49="
	b, err := parseLine(line)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if b.CIGAR != "50=1X49=" {
		t.Errorf("CIGAR: got %q", b.CIGAR)
	}
}

func TestParseLineShortFails(t *testing.T) {
	if _, err := parseLine("only\ta\tfew\tfields"); err == nil {
		t.Error("expected error for short PAF line")
	}
}

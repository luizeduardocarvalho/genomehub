#!/usr/bin/env python3
"""Generate synthetic FASTA genomes with controlled overlap for the GenomeHub
demo. Deterministic (fixed seed) so runs are reproducible.

Relationships engineered on purpose:
  ATHENA   — base genome, 5 chromosomes of random sequence.
  ATHENA2  — related strain: chr1..chr3 are ATHENA's verbatim (so their chunks
             are byte-identical and dedup against ATHENA), chr4..chr5 are novel.
             A peer that holds ATHENA therefore already has ~3/5 of ATHENA2's
             segments → shows as a *partial* seed without downloading ATHENA2.
  ECOLI    — unrelated species, single chromosome, no shared content.
  HSAP     — another unrelated species.

Writes <outdir>/<ASSEMBLY>.fa.
"""
import os, random, sys

OUTDIR = sys.argv[1] if len(sys.argv) > 1 else "tmp/synth"
CHROM_BP = int(os.environ.get("CHROM_BP", "80000"))


def rng(seed):
    r = random.Random(seed)
    return r


def chrom(r, n):
    return "".join(r.choice("ACGT") for _ in range(n))


def write_fa(path, chroms):
    with open(path, "w") as f:
        for name, seq in chroms:
            f.write(f">{name}\n")
            for i in range(0, len(seq), 70):
                f.write(seq[i:i + 70] + "\n")


def main():
    os.makedirs(OUTDIR, exist_ok=True)

    # ATHENA: 5 independent chromosomes.
    ra = rng(1001)
    athena = [(f"chr{i+1}", chrom(ra, CHROM_BP)) for i in range(5)]
    write_fa(os.path.join(OUTDIR, "ATHENA.fa"), athena)

    # ATHENA2: reuse ATHENA chr1..3 verbatim (shared), add 2 novel chromosomes.
    rb = rng(2002)
    athena2 = athena[:3] + [(f"chr{i+4}", chrom(rb, CHROM_BP)) for i in range(2)]
    write_fa(os.path.join(OUTDIR, "ATHENA2.fa"), athena2)

    # ECOLI: unrelated, one longer chromosome.
    re_ = rng(3003)
    write_fa(os.path.join(OUTDIR, "ECOLI.fa"), [("chr1", chrom(re_, CHROM_BP * 2))])

    # HSAP: unrelated, 3 chromosomes.
    rh = rng(4004)
    hsap = [(f"chr{i+1}", chrom(rh, CHROM_BP)) for i in range(3)]
    write_fa(os.path.join(OUTDIR, "HSAP.fa"), hsap)

    print(f"wrote ATHENA, ATHENA2, ECOLI, HSAP to {OUTDIR}/ ({CHROM_BP} bp/chrom)")
    print("ATHENA2 shares chr1..chr3 with ATHENA (≈60% overlap)")


if __name__ == "__main__":
    main()

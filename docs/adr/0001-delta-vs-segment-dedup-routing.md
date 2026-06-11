# ADR 0001 — Route storage by similarity: delta encoding for near-identical genomes, segment dedup for diverged ones

- Status: Accepted
- Date: 2026-06-10

## Context

GenomeHub's storage model is content-addressable segment deduplication: gear-hash
chunking, optionally rechunked at MEM (exact-match) boundaries discovered by
minimap2 (`reindex`, `chunking.algorithm = "gear+mem"`). Identical segments across
genomes are stored once.

We measured this model on three *Arabidopsis thaliana* accessions (TAIR10, Ler-0,
Cvi-0), which are ~99% identical but carry a SNP roughly every ~150 bp. We swept
`--min-exact` (the minimum exact-match length used as a shared boundary):

| min-exact | exact-match coverage | storage saved | core (shared by all 3) | total segments (3 genomes) | reindex time |
|-----------|----------------------|---------------|------------------------|----------------------------|--------------|
| 100       | 68–83%               | (viz could not complete) | n/a               | ~6.2M                      | 12.5 min + hang |
| 500       | 37–54%               | **27%**       | 11.9%                  | ~1.27M                     | 8 min        |
| 2048      | 6–16%                | 6%            | 0.7%                   | ~520k                      | 16 s         |
| 4096      | 1–7%                 | 2%            | 0.1%                   | ~5k                        | 6 s          |
| 8192      | 0–3%                 | 1%            | 0.0%                   | ~1.4k                      | 4 s          |

### Findings

1. **Dedup is bounded by exact-match coverage, which is bounded by SNP density.**
   Lower `min-exact` captures more coverage and more dedup, but exact runs cannot
   cross a SNP, so for SNP-dense pairs the shared regions are inherently short.
2. **Capturing that sharing explodes segment count super-linearly.** Going from
   `min-exact` 500 → 100 (5× shorter anchors) multiplied total segments ~5×
   (1.27M → 6.2M) for only ~1.5× coverage gain.
3. **The operational wall arrives before the storage win.** At 6.2M segments,
   index summarisation (`viz`) did not complete; manifests reach ~12.8 MB; per-segment
   metadata (32-byte hash + length) becomes a large fraction of a ~200 B segment.
4. **Practical peak ≈ 27% saved** at `min-exact` 500 — modest, and paid for with
   1.27M segments and 12.8 MB manifests.

### Root cause

A different accession of the same species is not "10% novel in one contiguous
block." It is ~1% novelty **smeared across 100% of the genome**. Segment dedup can
only exploit *clumped* novelty (large shared/private blocks). It cannot exploit
*sprayed* novelty. Delta (reference + variant) encoding is purpose-built for sprayed
novelty and is what the field already uses (CRAM, bcftools/VCF, 1001 Genomes).

## Decision

Route storage strategy by genome-pair similarity, using the same MinHash/Mash
sketch signal already planned for MEM-job prioritisation:

| similarity | relationship | storage path |
|------------|--------------|--------------|
| > ~0.95    | same species, different accession | **delta** (reference + per-genome diff) |
| ~0.5–0.95  | same genus / structural differences | **segment dedup** (gear+MEM, existing) |
| < ~0.5     | too diverged | skip (shared content too small to matter) |

- **Delta path:** pick a reference genome stored once; store each near-identical
  genome as an ordered list of `copy(reference[a:b])` and `literal(bytes)` operations.
  Reconstruction applies the ops against the reference. Only divergent (~1%) bytes
  are stored as literals; shared sequence is referenced, not copied.
- **Segment path:** unchanged. Best for the diverged backbone where shared regions
  are long and SNP-sparse.

This is a hybrid: segment model for diverged genomes, delta model for the
SNP-dense leaves. The routing threshold is tunable and will be calibrated with
more empirical data.

## Consequences

- `reindex`/gear+MEM is **not** deprecated; it is repositioned to its strong case.
  README messaging must stop implying same-species accessions are a strong case for
  segment dedup (they are ~27%, not >90%).
- New artifact type: a **delta manifest** distinct from a segment manifest. The
  download/reconstruct protocol must handle both. A delta references a reference
  genome by content hash; that reference must be resolvable (stored as a normal
  segment manifest or fetched).
- Adds a `delta` and `reconstruct-delta` command path. First implementation:
  whole-genome, `+`-strand alignment blocks become `copy` ops, everything else
  (`X`/`I`, unaligned query regions, `-`-strand blocks) becomes `literal`. Correct
  by construction (round-trip verified); compression improves as more is expressed
  as `copy`.
- `viz` slowness is treated as a diagnostic signal, not a blocker — it is not on the
  storage path.

## Alternatives considered

- **Push `min-exact` lower (< 500).** Rejected: improves dedup but segment count and
  metadata overhead grow faster than the saving; operationally untenable (~6.2M
  segments at min-exact 100).
- **Single model for everything (segments only).** Rejected: leaves >60 percentage
  points of achievable savings on the table for same-species genomes, the most
  common real-world redundancy.
- **Single model for everything (delta only).** Rejected: delta against one linear
  reference degrades for diverged/structurally-variant genomes — exactly where the
  pangenome segment graph is the right representation.

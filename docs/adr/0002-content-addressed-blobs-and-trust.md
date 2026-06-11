# ADR 0002 — Everything is a content-addressed blob; trust splits into correctness and availability

- Status: Accepted (amended 2026-06-11 — added contribution as a third reputation axis, the O(N²) scaling boundary, and distributed MEM-finding)
- Date: 2026-06-11
- Related: [ADR 0001](0001-delta-vs-segment-dedup-routing.md), [ADR 0003](0003-node-session-tracker-daemon.md)

## Context

GenomeHub now has three storage/transfer artifacts:

1. **Segments** — content-addressed (BLAKE3) sequence chunks. Already peer-ready:
   a hash is a self-verifying address; any node can serve `blake3:…` and the
   receiver re-hashes to confirm.
2. **Manifests** — ordered lists of segment hashes (the "recipe" for a genome).
3. **Deltas** — a genome encoded as copy/literal ops against a reference
   (ADR 0001). Currently a **monolithic file**, *not* in the content-addressed
   world.

The same underlying question surfaced three separate times while designing the
distribution, MEM, and graph-evolution layers:

- **Peer transfer** — what is the unit a peer serves, and how do we trust a
  stranger's bytes?
- **MEM discovery/validation** — how do contributors submit findings without us
  trusting them?
- **Graph mutation** — how do we re-segment without corrupting published
  manifests, and how is sharing tracked across >2 genomes?

They are the same insight viewed three ways. This ADR records the unifying model
so the distribution, contributor, and graph layers are built on one foundation
rather than three ad-hoc ones.

## Decision

### 1. Everything peers move is a content-addressed blob

The unit of transfer is always **hash → bytes**, re-verified by the receiver.
Segments already are this. Manifests and deltas must become this too:

- **Deltas are chunked.** Pack the delta's literals and op stream into a blob,
  then split that blob at a fixed size (256 KB–1 MB) into content-addressed
  chunks. A delta is then described by a small recipe (ordered chunk hashes), just
  like a genome manifest. **Do not** content-address individual literals or ops —
  that recreates the millions-of-tiny-segments explosion ADR 0001 escaped. Chunk
  the *blob*, not the *ops*.
- **Copy ops already reference reference-segment ranges**, which resolve to
  existing segment hashes a peer may already hold. Uniform.
- The download client then has **one code path**: fetch a recipe, fetch the
  chunk/segment hashes it lacks, verify, assemble. It does not care whether the
  thing being assembled is a segment genome or a delta.

Consequence: origin and peer are indistinguishable to a client, and the
`GET /segments/{hash}` (later `/blobs/{hash}`) endpoint is the only transfer
primitive. Transport (HTTP/1.1 → h2c → QUIC) swaps behind it without touching the
model.

### 2. Trust splits into three axes: correctness, availability, and contribution

Content addressing makes **correctness** non-negotiable and free: a blob either
hashes to its address or it is rejected. A node — origin, peer, or contributor —
**cannot serve bad data**, regardless of intent. So correctness needs **no**
reputation machinery at all.

Two things *do* need scoring, and they are different from correctness and from
each other:

- **Availability** — does a node reliably have and serve the blobs it advertises,
  with acceptable uptime and latency? Used to choose good peers and drop dead ones.
- **Contribution** — how much *useful work* has a node donated: MEMs discovered,
  alignment compute, bandwidth served. This is the **incentive layer** — it earns
  priority downloads, bandwidth credits, and attribution in graph-version metadata.
  "Better results make a peer more reputable" lives here. Weight it by **impact,
  not count**: a MEM that increases sharing across many genomes (bytes saved ×
  genomes affected) is worth more than a trivial one — which also makes it a
  publishable scientific-credit metric.

The hard rule that keeps this safe: **contribution and availability reputation
must never bypass verification.** A high-reputation node still has every blob
re-hashed and every MEM re-checked. Reputation only buys *rewards and
prioritization*, never "skip the hash check." The moment reputation can bypass
verification, the attack surface that hashing closed is reopened.

This collapses the heavy validation scheme sketched in the README:

- **MEM validation is one cheap re-check, not N-redundancy.** A MEM is a
  deterministic claim: "genome A [a:b] equals genome B [c:d], hash H." The server
  re-derives it once — pull those coordinates, hash, compare exact match — instead
  of requiring N independent contributors to agree. Exact matches are
  self-verifying; approximate alignments would not be, which is why the original
  redundancy design existed. We don't need it for *correctness* (see §5 for why a
  weaker form of redundancy still helps *coverage*).
- **A wrong MEM is simply ignored** (it fails the re-check), not punished through a
  reputation tribunal. Reputation is for rewarding good contribution, not
  policing bad data.

### 3. A MEM is a graph node with placements; materialize it only when needed

A validated MEM is almost the same object as a shared content-addressed segment:
the segment store holds the sequence, and the index records *which* genomes
reference it. What the index lacks is **per-genome coordinates and strand** —
i.e. *where* the shared sequence sits in each genome.

The full record is a pangenome-graph node:

```
node := { seq_hash, length, placements: [ {genome, chrom, start, strand}, ... ] }
```

This is a GFA segment plus its paths/walks. It enables the **split operation**:
when a new genome forces a finer boundary inside a node, split the node and
rewrite every walk that used it — so multi-way sharing is never silently dropped
by a pairwise cut.

Decision: **do not build the explicit node/placement store yet.** Through segment
download (Milestone 2) and delta routing (ADR 0001), the segment store + index
(membership without coordinates) is sufficient. Build the explicit graph only when
implementing **incremental graph updates** (Milestone 4), and when you do, it
*is* the GFA graph — not a parallel "MEM table". The node/placement schema above is
the target representation.

### 4. Reindex is batch-only; re-segmentation produces a new graph version

A MEM is pairwise but a cut is global: cutting genome A at an A–B boundary affects
what A can share with C, D, …. The current `reindex` is only correct as an
**all-pairs batch** (it aligns every pair and reconciles cuts via `propagateCuts`).
Running it on a subset produces boundaries inconsistent with a previous run.

Guardrails:

- `reindex` is **batch-only, and only for bootstrapping a small seed set.** Subset
  or incremental runs are not safe against an existing segmentation.
- Re-segmentation never mutates a published manifest. It produces **graph version
  N+1**; old manifests remain valid against version N (the graph-version lifecycle
  already in the README). This is what makes adding genomes safe without the
  node-split machinery — until incremental updates (Milestone 4) make splitting
  first-class.

**The O(N²) scaling boundary (why batch reindex is seed-only).** All-pairs reindex
costs C(N,2) alignments. At N=200 that is ~19,900 minimap2 runs (~weeks of
compute) plus a `propagateCuts` cut-union blow-up. It does **not** scale, and
nobody should ever run all-pairs reindex on a large corpus. Production scale comes
from two mechanisms, both already decided:

- **Sketch routing (ADR 0001) removes most of the N².** Dissimilar pairs are
  skipped. Dense same-species clusters route to **delta**, which is **O(N), not
  O(N²)**: 150 accessions = 150 alignments against one reference, not ~11,000
  pairwise. The delta path is therefore not just a storage win — it is what makes
  same-species clusters tractable at all.
- **Incremental graph construction handles the diverged backbone.** A new genome
  is aligned against cluster representatives / the existing graph and spliced in
  via node-split (§3) — **O(1) amortized per genome**, not O(N²). This is exactly
  what the node/placement table enables, and why Milestone 4 is load-bearing for
  any corpus past a handful of genomes, not optional polish.

So: routing + delta + incremental fixes the **asymptotic** pair count; distributed
MEM-finding (§5) fixes the **per-alignment** cost. They are complementary —
distributing compute is never a licence to brute-force all-pairs.

### 5. MEM-finding is distributed to peers; the output, not the worker, is trusted

The expensive work — minimap2 alignment to discover MEMs — is donated by
contributor nodes, not run only on the origin. minimap2 is embarrassingly parallel
by query, so the unit of distribution is a **(target, query-window) tile**:

```
peer A: align target × query[chr1]            → MEMs
peer B: align target × query[chr2 0–15Mb]     → MEMs   (windows overlap by the
...                                                      max expected MEM length so
server: merge + dedupe + verify → graph update          boundary MEMs aren't lost)
```

This is MapReduce: *map* = align a tile → MEMs; *reduce* = merge/validate.

- **Compute rides the blob layer.** A peer fetches the target and its query window
  as content-addressed blobs (the same transfer primitive as §1), runs minimap2
  locally, and returns MEMs. No separate data plane for compute.
- **Untrusted workers are safe because the output is self-verifying.** A returned
  MEM is a claim the server re-checks in O(1) (§2). A lying or buggy peer's false
  MEMs fail the re-check and are dropped. minimap2 non-determinism (threads,
  versions) therefore does not matter — every accepted MEM is verified.
- **Redundancy is for coverage, not correctness.** Re-check guarantees no *false*
  MEMs, but not *completeness* — a lazy peer can return fewer MEMs. A missing MEM
  costs a little dedup, not correctness, so the system degrades gracefully.
  Overlap-assigning tiles to catch missed MEMs is a tunable quality knob, **not** a
  trust gate (contrast the README's "N peers must agree").
- **Practicalities:** amortize target indexing (ship the prebuilt `.mmi` as a blob,
  or shard coarse by chromosome); the job queue claims/heartbeats/reassigns tiles
  so one slow peer can't stall a batch; reward = compute donated weighted by MEM
  impact (the contribution axis of §2).

## Consequences

- **Distribution layer** has a single transfer primitive (content-addressed
  blob) and a single client assembly loop. Deltas stop being a special case once
  chunked. The tracker is a stateless `hash → [nodes]` map needing zero semantic
  knowledge.
- **Contributor layer** is much lighter than the README implies for *trust* (no
  N-redundancy, no honesty reputation — just re-check-on-submit), while keeping a
  real **incentive layer**: availability + contribution scoring, the latter
  weighted by MEM impact. MEM-finding itself is distributed to peers as tiles.
- **Graph layer** has a defined target (GFA node + placements) and a safety rule
  (batch-only reindex, version-on-resegment) without building it prematurely.
- **Delta blob format (ADR 0001)** gains a follow-up: define the chunked
  container so a delta is a recipe of chunk hashes. The current monolithic
  `GHD1` file remains valid as the un-chunked local form; the networked form is
  chunked.

## Alternatives considered

- **Keep deltas monolithic for transfer.** Rejected: forces a second transfer
  path and breaks dedup of shared delta regions across accessions. Chunking the
  blob unifies it at negligible cost.
- **Per-op / per-literal content addressing for deltas.** Rejected: reintroduces
  the micro-segment explosion (millions of ~150 bp objects) that ADR 0001 showed
  is operationally fatal.
- **N-redundancy MEM validation + honesty reputation (original README design).**
  Rejected for *trust*: unnecessary because exact matches are self-verifying. A
  weak form of redundancy is retained only as a tunable *coverage* knob (§5), and
  reputation is retained only as *availability + contribution* scoring (§2) — never
  as a gate on whether data is accepted.
- **Build the node/placement graph store now.** Rejected as premature: not needed
  until incremental graph updates; would add a parallel format if built before the
  GFA direction is implemented.

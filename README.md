# GenomeHub — Project Design Document

> Living document. Add to each section as decisions are made or ideas evolve.

---

## What Is This?

A content-addressable, graph-backed genomic data protocol with local deduplication and optional peer distribution.

The core insight: genomic sequences across related organisms are massively redundant. Instead of every lab downloading the same data independently, we store shared sequence segments once and let everyone reconstruct any genome from those shared building blocks — only downloading what they don't already have.

Think BitTorrent meets git objects, built specifically for biological sequence data.

---

## Install

GenomeHub is a single static Go binary. The core researcher workflow — `import`,
`reconstruct`, `verify`, `download`, `viz` — has **no external dependencies**.
`minimap2` is only needed for the contributor/server commands (`align`,
`reindex`, `work`); install it (`brew install minimap2`) only if you run those.

**Prebuilt release** (recommended) — grab the archive for your OS/arch from the
[releases page](https://github.com/luizeduardocarvalho/genomehub/releases),
unpack, and put `genomehub` on your `PATH`:

```bash
tar xzf genomehub_*_$(uname -s)_$(uname -m).tar.gz
sudo mv genomehub /usr/local/bin/
genomehub version
```

**With the Go toolchain** (Go 1.24+):

```bash
go install github.com/luizeduardocarvalho/genomehub@latest
```

**From source:**

```bash
git clone https://github.com/luizeduardocarvalho/genomehub
cd genomehub
make install            # builds with version metadata → /usr/local/bin
# or: make build        # just produces ./genomehub
```

Verify: `genomehub version` should print a version, commit, and build date.

---

## Quickstart (5 minutes, fully offline)

No server, no network, no minimap2 — this proves the core round-trip and the
deduplication payoff on genomes you already have. All state lives under
`~/.genomehub` (override with `--store`).

**1. Ingest a FASTA** — chunk it into content-addressed segments and write a manifest:

```bash
genomehub import --fasta genome.fa --organism "Arabidopsis thaliana" \
  --assembly TAIR10 --output TAIR10.manifest.json
```

**2. Reconstruct it** — rebuild the FASTA from the manifest + stored segments:

```bash
genomehub reconstruct --manifest TAIR10.manifest.json --output rebuilt.fa
```

**3. Verify** — confirm the round-trip is byte-identical at the sequence level:

```bash
genomehub verify --original genome.fa --reconstructed rebuilt.fa
# exits non-zero on any mismatch
```

**4. See the dedup** — import a *related* genome (same species accession, a prior
version, anything sharing sequence) and compare. Shared segments are stored once:

```bash
genomehub import --fasta other.fa --organism "Arabidopsis thaliana" \
  --assembly Ler0 --output Ler0.manifest.json
genomehub viz TAIR10.manifest.json Ler0.manifest.json
# bar chart of shared vs unique, bytes stored vs naive, savings %
```

For near-identical genomes (same species), `delta` beats segment dedup — see the
Delta encoding section. For multi-machine distribution, see Distribution below.

---

## Goals

- **Reduce bandwidth costs** for institutions that publicly distribute genomic data (NCBI, Ensembl, PLAZA, Embrapa)
- **Reduce download costs** for labs, especially underfunded ones in developing countries
- **Make genomic data access faster** through local deduplication and peer distribution
- **Improve over time** — the system gets smarter and more efficient as more genomes are added
- **Be a tool researchers actually want to use** — great CLI, reliable, no dependency hell, respects their compute

---

## What This Is Not

- A replacement for existing bioinformatics pipelines (output must be compatible with FASTA/FASTQ)
- A biological analysis tool — we store and distribute sequence, we don't interpret it
- A centralized database — origin server is source of truth but nodes share the load

---

## Core Concepts

### Content-Addressable Segments
Every sequence segment is identified by the hash of its content (BLAKE3). Identical sequence in two different genomes produces the same hash — automatic deduplication. No coordination needed.

### Manifests
A manifest is the recipe for a genome. It contains an ordered list of segment hashes per chromosome. Reconstruction = fetch each hash in order, concatenate. The manifest is the central artifact everything else builds on.

### Pangenome Graph
As more genomes are added, shared segments form a graph. Each genome is a walk through that graph. Shared nodes are stored once. Private nodes (unique sequence) are stored per genome. The graph improves over time as better shared boundaries are discovered.

### Graph Versions
The graph is a living structure. When the optimizer finds better shared boundaries, it publishes a new graph version. Manifests reference the graph version they were built against. Old versions remain valid until deprecated.

### Sketches
Cheap genome fingerprints (MinHash over k-mers) used to estimate similarity between genomes without comparing them fully. Used to prioritize which genome pairs are worth expensive MEM-finding work.

### MEMs (Maximal Exact Matches)
Long stretches of sequence that appear identically across multiple genomes. Finding MEMs is the expensive optimization work that improves graph sharing quality. MEMs are discovered by contributor nodes and validated by the server.

---

## Architecture

### Layers

```
CLI (researcher interface)
    ↓
QUIC transport (parallel segment fetching, resumable)
    ↓
Manifest API (HTTP/2, JSON, curl-friendly)
    ↓
Segment store (BadgerDB → RocksDB when needed)
    ↓
FM-index layer (fast substring search without full decompression)
    ↓
GFA ingestion (pangenome graph input/output)
    ↓
Observability (OpenTelemetry → Prometheus + Grafana)
```

### What Lives Where

| Data | Location | Reason |
|---|---|---|
| All segments | Origin server | Source of truth, never lose data |
| All MEMs | Origin server | Scientific findings, permanent record |
| All manifests + headers | Origin server | Authoritative |
| Graph versions + diffs | Origin server | Version history |
| Job queue + results | Origin server | Coordination |
| Downloaded segments | Nodes (cache) | Serve to peers |
| Locally computed segments | Nodes (cache) | Contribute to network |

Nodes are never the sole holder of anything. Origin is the archive. Nodes reduce load and improve speed.

### Who Runs What (Critical Design Point)

**Researchers never run alignment or reindex.** That is server/contributor work.

```
SERVER (runs once, benefits everyone forever)
  new genome arrives
    → import (gear hash, seconds) → genome available immediately
    → background: reindex against similar genomes (minutes to hours, server-side)
    → publish new graph version with optimal shared boundaries

RESEARCHER (what they actually run)
  genomehub download --assembly TAIR10
    → fetch manifest (tiny JSON, <1s)
    → check which hashes already in local store
    → fetch ONLY missing segments (parallelised, from peers or origin)
    → reconstruct FASTA
```

For a 32GB genome at 90% similarity to something the researcher already has:
- Naive: download 32 GB
- With GenomeHub: download ~3.2 GB (the 10% that is new)

The slow minimap2/MEM-finding work happens once per genome pair on a server. Every researcher benefits from it forever.

---

## Manifest Format

### Structure (JSON representation — wire format is MessagePack)

```json
{
  "version": 1,
  "graph_version": 12,
  "organism": "Arabidopsis thaliana",
  "assembly": "TAIR10",
  "total_bases": 119667750,
  "encoding": "raw-ascii",
  "chunking": {
    "algorithm": "gear",
    "min_size": 262144,
    "max_size": 1048576
  },
  "created_at": "2026-06-10T00:00:00Z",
  "segments_root": "blake3:af3c...",
  "chromosomes": [
    {
      "name": "Chr1",
      "length": 30427671,
      "hash": "blake3:9f2a...",
      "segments": [
        { "hash": "blake3:1a2b...", "length": 524288 },
        { "hash": "blake3:3c4d...", "length": 524288 },
        { "hash": "blake3:5e6f...", "length": 487095 }
      ]
    }
  ]
}
```

### Design Decisions

- **graph_version**: which graph version this manifest was built against — needed for diff/upgrade protocol. v1 = gear hash only. v2+ = MEM-aligned.
- **chunking.algorithm**: `"gear"` for phase 1 ingestion, `"gear+mem"` after MEM-boundary reindexing
- **segments_root**: BLAKE3 hash over all segment hashes in order — verifies full reconstructed sequence
- **chromosome hash**: BLAKE3 of full chromosome sequence — lets a researcher verify one chromosome independently
- **chunking params**: min/max only (avg dropped — not useful for verification), needed to reproduce identical boundaries
- **encoding**: explicit so future formats (2-bit, compressed) are unambiguous
- **manifest hash**: computed over the full manifest bytes after writing, stored in the header (not self-referential)

### Integrity Chain

```
TLS connection (trust the transport)            ← serve/node --tls-cert/--tls-key
  └── header (trusted because of TLS)
        └── manifest_hash in header (verifies manifest wasn't tampered)
              └── segments_root in manifest (verifies reconstructed sequence)
                    └── per-chromosome hash (verify any chromosome independently)
```

TLS anchors the chain: it authenticates the origin so the manifest (and the
hashes inside it) can be trusted. Segments themselves are content-addressed and
re-hashed on arrival, so they need no transport trust — only the manifest does.
Enable TLS with `--tls-cert`/`--tls-key` (see [TLS](#tls)). ed25519 manifest
signing — for manifests served over untrusted peers, not just a trusted origin —
is a planned follow-up.

---

## Chunking Strategy

### Phase 1 — Gear Hash (Content-Defined Chunking)

A rolling hash window slides across the sequence one base at a time. When the rolling hash matches a target bit pattern AND chunk size >= min_size, a boundary is cut. If chunk reaches max_size with no natural boundary, force a cut.

```
ATCGGCTATCG | ATCGATCGGCT | ATCGATCG...
             ↑             ↑
           cut!           cut!
```

Boundaries are content-driven — an insertion only shifts the one or two chunks around it. This is what makes deduplication work across related genomes.

**Default parameters:**
- min_size: 262144 (256 KB)
- max_size: 1048576 (1 MB)
- target average: ~512 KB (19-bit mask)

**Configurable per import:**
```bash
genomehub import --fasta genome.fa --min-chunk 65536 --max-chunk 262144
```

**Important:** both genomes in a pair MUST use identical chunking parameters for cross-genome dedup to work. Different min/max → different boundaries → zero shared segments.

### Phase 2 — MEM-Boundary Reindexing

Gear hash alone cannot find shared segments between diverged species. A SNP every few hundred bases constantly shifts the rolling hash, preventing boundary alignment.

**The border contamination problem:**
```
genome_a: [shared_start 3MB][unique_a 2MB][shared_end 3MB]
genome_b: [shared_start 3MB][unique_b 2MB][shared_end 3MB]

Gear hash result:  [SHA SHA | UNQ UNQ UNQ | SHA SHA SHA]
                                 ↑
                   border chunk: shared_start_tail + unique_head
                   Different content in each genome → unique, not shared
                   MEM-finding fixes this by discovering the true boundary
```

**MEM-boundary reindexing:**
1. Run minimap2 with `-c --eqx` to find aligned blocks with per-base CIGAR
2. Extract `=` (exact match) runs from CIGAR that are >= min_exact bp
3. For each genome, cut at exact match boundaries, gear-hash chunk each region independently
4. Same exact match content in both genomes → same gear hash progression → same segment hashes → stored once

### Preset Selection by Species Divergence

| Species relationship | Divergence | Preset |
|---|---|---|
| Same species, different accessions | < 1% | `asm5` |
| Same genus (e.g. thaliana vs lyrata) | 1–15% | `asm20` |
| Same family | 15–30% | `asm20` (may miss some alignments) |
| More diverged | > 30% | Exact matches too short, dedup marginal |

### Key Discovery: PAF Identity Calculation

minimap2's PAF field 9 (matches) / field 10 (block_len) gives misleading identity because block_len spans the full alignment span including large internal gaps. Use the `dv:f:` optional tag instead:

```
dv:f:0.0107  →  divergence = 1.07%  →  identity = 98.93%  ← correct
matches/block_len = 2.57M/10.85M = 23.7%                  ← wrong
```

---

## MEM-Finding Backends

Two implementations behind a common `aligner.Aligner` interface:

```go
type Aligner interface {
    FindMEMs(target, query []byte, minLen int) ([]ExactMatch, error)
}
```

### Backend 1: minimap2 (production)

Wraps `minimap2` subprocess. Used in `genomehub reindex` for full-genome pairs.

- Handles full genomes (120MB–200MB+)
- PAF output streamed in real-time with progress counter
- CIGAR parsed for exact match extraction (`=` operations)
- Both `+` and `-` strand alignments supported
- Identity computed from `dv:f:` tag, not `matches/block_len`

```bash
genomehub align --target a.fa --query b.fa --preset asm20 --output blocks.json
genomehub reindex --target a.fa --query b.fa --target-assembly A_v2 --query-assembly B_v2
```

### Backend 2: Native FM-index (internal/fmindex)

Pure Go implementation. Used for testing and per-chromosome analysis.

**Data structure:**
```
text = target + '\x01' + query + '\x00'
  sentinels: '\x00'=0 < '\x01'=1 < 'A'=65

Suffix array (SA)     — O(n log²n) construction, swap SA-IS for n > 50 MB
BWT[i] = text[SA[i]-1]
C[c]   = # chars in text lex-smaller than c
Occ(c, pos) = # of c in BWT[0..pos)  — sampled every 128 rows
targetRows  = sorted SA row indices where SA[row] < len(target)
              → enables O(log n) hasTargetHit via binary search
```

**FindMEMs algorithm:**
1. For each query position j, extend right via `backwardSearch`
2. At each extension, check `hasTargetHit` (O(log n)) to stop when only query-side hits remain
3. Left-maximality: drop candidates where `text[tp-1] == text[qOff+j-1]`
4. Right-maximality: guaranteed by step 2
5. Deduplicate and filter dominated matches

**Known complexity limits (noted in package docs):**
- SA construction: O(n log²n) — use SA-IS for n > 50 MB
- MEM extension: O(qLen × lce²) — bi-directional FM-index gives O(qLen × lce)
- filterMaximal: O(k²) — use interval tree for k > 100k MEMs
- `-` strand: FindMEMs searches forward strand only; caller must reverse-complement query for `-` strand MEMs

**Practical limit:** per-chromosome sequences up to ~50 MB. For full genomes, call per chromosome.

---

## Code Structure

```
genome-hub/
├── main.go
├── cmd/                          — one file per command group
│   ├── root.go                   — cobra root, --store flag, dir helpers
│   ├── import / reconstruct / verify / viz / align / reindex / benchmark
│   ├── delta / delta_publish / reconstruct_delta   — delta encode/chunk/decode
│   ├── route / jobs_cmd          — sketch routing; job enqueue/list
│   ├── serve / download          — HTTP node + client (parallel, peer-aware)
│   ├── tracker / node            — peer discovery + long-lived node daemon
│   ├── coordinator / work        — MEM job queue + worker
│   ├── control.go                — operator pane: origin + tracker + coordinator (ANSI)
│   └── tui.go                    — top / status live views (ANSI)
└── internal/
    ├── aligner/      — minimap2 wrapper (+ context), PAF/CIGAR, FM-index iface
    ├── chunker/      — gear-hash content-defined chunker
    ├── fmindex/      — native FM-index MEM-finder
    ├── fasta/        — FASTA reader/writer
    ├── manifest/     — Manifest struct, segments_root
    ├── store/        — BadgerDB: Put/Get/Has/ListHashes, BLAKE3
    ├── index/        — hash → genomes membership (for viz)
    ├── sketch/       — bottom-k MinHash (genome fingerprint, Jaccard/ANI)
    ├── delta/        — copy/literal patch, GHD1 binary codec, transfer recipe
    ├── httpapi/      — node HTTP surface (segments/manifests/deltas/status)
    ├── tracker/      — stateless hash→nodes registry + liveness
    └── jobs/         — MEM job queue + coordinator (verifies submitted MEMs)
```

---

## CLI Reference

All commands accept `--store <path>` (default: `~/.genomehub/segments`).

### `import` — ingest a FASTA locally
```bash
genomehub import \
  --fasta genome.fa \
  --organism "Arabidopsis thaliana" \
  --assembly TAIR10 \
  --min-chunk 262144 \    # optional, default 256 KB
  --max-chunk 1048576 \   # optional, default 1 MB
  --output TAIR10.manifest.json
```
Gear-hash chunks the sequence, stores segments in BadgerDB, writes manifest (graph_version=1).

### `reconstruct` — rebuild FASTA from manifest
```bash
genomehub reconstruct \
  --manifest TAIR10.manifest.json \
  --output reconstructed.fa
```
Fetches each segment hash from store in order. Verifies per-chromosome BLAKE3 hash before writing.

### `verify` — confirm round-trip integrity
```bash
genomehub verify \
  --original original.fa \
  --reconstructed reconstructed.fa
```
Sequence-level comparison (ignores FASTA line wrapping differences). Exits non-zero on mismatch.

### `viz` — visualise dedup across genomes
```bash
genomehub viz manifest_a.json manifest_b.json [manifest_c.json ...]
```
Outputs:
- Proportional bar chart (green=shared, per-genome colors for unique)
- Hash table: hash | size | store ✓/✗ | referenced by
- Summary: unique segments, bytes stored vs naive, savings %

`✗ MISS` in store column = segment referenced in manifest but not locally — tells you what to fetch.

### `align` — find aligned blocks with minimap2
```bash
genomehub align \
  --target a.fa \
  --query b.fa \
  --preset asm20 \         # asm5 = same species, asm20 = cross species
  --min-identity 0.95 \    # uses dv tag, not matches/block_len
  --min-len 1000 \
  --output alignments.json
```

### `reindex` — rechunk N genomes at shared MEM boundaries
```bash
genomehub reindex \
  --genome TAIR10:a.fa --genome Ler0:b.fa --genome Cvi0:c.fa \
  --min-exact 500 --preset asm20 --threads 4
```
All-pairs align, propagate cuts, rechunk each genome at the union of MEM boundaries.
Alignments are cached (`--no-cache` to force). **Batch-only and seed-scale only** —
all-pairs is O(N²); past a handful of genomes use routing + delta + incremental
([ADR 0002 §4](docs/adr/0002-content-addressed-blobs-and-trust.md)).

### Delta encoding + similarity routing

```bash
# fingerprint + nearest-neighbour (uses sketches persisted on import)
genomehub sketch --fasta g.fa --assembly G
genomehub similarity --a a.fa --b b.fa
genomehub similar --assembly TAIR10 --top 10

# pick storage strategy by similarity (>0.95 delta, 0.5–0.95 reindex, else skip)
genomehub route --query Cvi0:c.fna --candidate TAIR10:a.fa --candidate Ler0:b.fna --execute

# encode a near-identical genome as a patch; rebuild it
genomehub delta --reference TAIR10:a.fa --query Ler0:b.fna --preset asm5 \
  --reference-manifest TAIR10.manifest.json --output Ler0.delta.ghd
genomehub delta-publish --delta Ler0.delta.ghd       # chunk it into the store (so it swarms)
genomehub reconstruct-delta --delta Ler0.delta.ghd --output Ler0.fa
```

### Distribution — server, peers, tracker

```bash
genomehub serve --catalog ./catalog --addr :8080            # read-only node (segments/manifests/deltas)
genomehub tracker --addr :9000                              # stateless hash→nodes index + liveness
genomehub node --tracker http://tracker:9000 --addr :8080 \ # long-lived peer: serve + announce + heartbeat
  --advertise http://me:8080 --catalog ./catalog
genomehub download --server http://origin:8080 --tracker http://tracker:9000 \
  --assembly Ler0 --output Ler0.fa --parallel 8             # peer-first, parallel, re-hash-verified
```

#### TLS

`serve` and `node` speak plain HTTP by default (fine on a trusted LAN). For
distribution over an untrusted network, enable TLS — this is what authenticates
the manifest you fetch (every *segment* is already re-hashed end-to-end, so the
remaining trust question is "is this the real origin's manifest?").

```bash
genomehub serve --catalog ./catalog --addr :8443 \
  --tls-cert cert.pem --tls-key key.pem                     # HTTPS origin
genomehub node  --tracker https://tracker:9000 --addr :8443 \
  --advertise https://me:8443 --catalog ./catalog \
  --tls-cert cert.pem --tls-key key.pem                     # HTTPS peer
genomehub download --server https://origin:8443 --assembly Ler0 --output Ler0.fa
```

- Clients verify certificates against the system trust store — use a real cert
  (Let's Encrypt, your institution's CA) in production.
- For self-signed certs in dev/testing, add the global `--insecure` flag to
  *client* commands to skip verification. Never use it against a real origin.
- Prefer terminating TLS at a reverse proxy (Caddy/nginx/Traefik) if you already
  run one — point it at the plain-HTTP `serve`/`node` on localhost. The built-in
  flags exist so a single binary needs no proxy.

### Distributed MEM-finding

```bash
genomehub coordinator --catalog ./catalog --mem-dir mems --addr :9100   # queue + re-verifies every MEM
genomehub job-enqueue --coordinator http://coord:9100 --target TAIR10 --query Ler0 --tile
genomehub work --coordinator http://coord:9100 --server http://origin:8080  # claim → align → submit
```

### Observability (live TUIs)

Our infrastructure — one operator pane over origin + tracker + coordinator:

```bash
genomehub control --origin http://origin:8080 --tracker http://tracker:9000 \
                  --coordinator http://coord:9100   # archive + swarm + job queue, one screen
```

Per-process / participant views:

```bash
genomehub top    --tracker http://tracker:9000    # the swarm: nodes, liveness, held
genomehub status --server  http://node:8080 --tracker http://tracker:9000 \
                 --id http://node:8080            # one node + how the swarm sees it (standing)
genomehub jobs   --coordinator http://coord:9100 --watch   # MEM jobs: queue, tiles, MEMs verified
```

`control` is the dashboard for the layer we own (origin archive + tracker + coordinator);
each panel degrades on its own, so a down endpoint never blanks the others.

`status` is the participant's self-view — what a peer or worker watches on their own box:

- **SEEDING** — every genome you can serve with a coverage bar: full seed
  (`▓▓▓▓ 100%`), partial cache (`▓▓░░ 60%`), or file-served delta. Answers
  "am I seeding X?" A pure cache peer (started with an empty catalog) still
  reports coverage: `download` saves each fetched manifest beside the store, and
  a node merges that cache into its catalog — so a box that downloaded TAIR10
  shows `TAIR10 100%` and attributes served segments to it, with no manifest
  hand-placed in the catalog.
- **SERVING** — live upload rate (req/s · bytes/s, last 10s) and the last
  requests you served, each attributed to its genome. Answers "am I a source
  right now?"
- **RECENT** — your last imports/downloads, read from a local `events.jsonl`
  the `import`/`download` commands append to (so history survives even though
  those are separate one-shot processes).
- **SWARM STANDING** (with `--tracker`) — how the tracker sees you: online,
  segments reseeded for peers, heartbeat age.

---

## Empirical Findings (Arabidopsis thaliana TAIR10 vs lyrata v1.0)

These are real measurements from the implementation, useful for calibrating expectations.

| Experiment | Result |
|---|---|
| Same genome imported twice (100% identity) | 100% shared, 0 bytes wasted |
| Synthetic genomes, 75% shared content, default chunk params | 17–30% dedup (gear hash only) |
| Thaliana vs lyrata, gear hash only | ~0% cross-species dedup (border contamination) |
| Thaliana vs lyrata, `align --min-identity 0.95 --min-len 1000` | 4866 blocks, 104 MB target span, 98.02% avg identity |
| Intra-genome dedup (repetitive elements, TEs) | ~2–7 MB savings per genome automatically |
| TAIR10 at min-chunk=4096, max-chunk=65536 | 2104 segments, 119 MB |
| 3 thaliana accessions, multi-way reindex (best) | ~27% saved, but 1.27M segments / 12.8 MB manifests — segment dedup is the wrong tool here |
| **Ler0 as a delta against TAIR10 (asm5)** | **130 MB → 13.1 MB (90% saved), byte-identical** — delta is the right tool for same-species |
| Warm peer downloading Ler0 (has TAIR10) | 15.9 MB over the wire for a 130 MB genome |
| Parallel multi-peer download (3-node swarm) | 128 segments spread ~47/35/42 MB across nodes |
| Distributed MEM-finding, TAIR10×Ler0 (tiled) | 46,692 MEMs, 46,692 verified (100%) across 5 chromosome tiles |

**Key insight on species comparison:**
Arabidopsis thaliana vs lyrata have ~13% nucleotide divergence. Raw gear hash cannot find cross-species shared segments because a SNP every ~100 bp constantly shifts the rolling hash. minimap2 alignment at 98% average identity across 104 MB of syntenic sequence provides the boundaries needed to extract exact match runs.

---

## Headers and Versioning

### Header Structure

The header is the stable public entry point for a genome. URL never changes, content evolves.

```json
{
  "assembly": "TAIR10",
  "organism": "Arabidopsis thaliana",
  "latest_graph_version": 12,
  "versions": [
    {
      "graph_version": 12,
      "manifest_hash": "blake3:ff1a...",
      "total_segments": 4821,
      "total_bases": 119667750,
      "published_at": "2026-06-10T00:00:00Z",
      "changelog": "Re-segmented Chr3 region 10M-15M, +340 shared segments"
    },
    {
      "graph_version": 7,
      "manifest_hash": "blake3:af3c...",
      "deprecated_at": "2026-05-01T00:00:00Z"
    }
  ]
}
```

### Version Lifecycle

```
published → active → deprecated → sunset → deleted
```

Segments only become GC-eligible when no active manifest references them (reference counting).

### Version Diffs

When graph version changes, server computes a diff:

```json
{
  "genome": "TAIR10",
  "from_version": 7,
  "to_version": 12,
  "segment_changes": {
    "added": ["blake3:ff1a...", "blake3:cc2b..."],
    "removed": ["blake3:af3c...", "blake3:1d4e..."]
  }
}
```

One diff blob per version transition, shared across all subscribers. Client applies diff against local inventory to determine what to fetch.

### Partial Upgrades

Manifests track version per chromosome, not per genome. A researcher can have Chr1 at v7 and Chr3 at v12. Each is valid and reconstructable independently.

---

## Notification Protocol (Fanout)

Inspired by social feed fanout. When graph optimizer publishes a new version:

**Hot genomes** (many subscribers) → write-time fanout
- Push new header to all watchers immediately via SSE

**Cold genomes** (few subscribers) → read-time fanout
- Mark as stale, notify on next access

### SSE Stream

```bash
curl https://genomehub.org/events/TAIR10
# streams:
# data: {"event":"new_version","graph_version":12,"changelog":"..."}
```

### Why This Is Simpler Than Social Fanout

Every subscriber watching the same genome version transition gets the **identical payload**. No per-user personalization. The diff is computed once, broadcast to all. Clients do their own local inventory diff to determine what to fetch.

---

## API Design

```
GET /genomes/{organism}/{assembly}           → latest header
GET /genomes/{organism}/{assembly}/v/{n}     → specific version header
GET /genomes/{organism}/{assembly}/manifest  → latest full manifest
GET /segments/{hash}                         → raw segment bytes
GET /events/{organism}/{assembly}            → SSE stream
```

Clean, predictable, curl-friendly. A bioinformatician should be able to figure out the API from the URL structure alone.

---

## Peer Distribution

### P2P Discovery

```
Client needs hash blake3:1a2b...
→ asks tracker: "who has this?"
→ tracker responds: ["lab-a-node", "embrapa-node"]
→ client fetches from fastest peer
→ falls back to origin if no peer available
```

Tracker maintains a hash → peer list mapping. Lightweight — no knowledge of genome structure needed.

### Bootstrap Options

1. **Seed from public databases** — import from NCBI/Ensembl/PLAZA on launch
2. **Researcher upload** — lab publishes their own genome FASTA
3. **Local import** — researcher already has FASTA locally, client chunks and registers

```bash
genomehub seed --source ensembl --organism arabidopsis-thaliana
genomehub publish --fasta my-soybean.fa --organism "Glycine max" --assembly "MySoy-1.0"
genomehub import --fasta tair10.fa
```

---

## Contributor Network

### What Contributors Can Do

| Contribution type | Minimum resource | Value |
|---|---|---|
| Serve segments (bandwidth) | Any amount | Reduces origin load |
| Compute sketches | Minutes, negligible CPU | Improves job prioritization |
| MEM-finding jobs | ~1-15 min compute | Improves graph quality |

### Job Queue (Message Broker Pattern)

Each MEM-finding job is a message with explicit state:

```
pending → claimed → in_progress → submitted → validating → validated / failed
```

- Workers claim jobs, send heartbeats every 30s
- Heartbeat timeout (2min) → job released back to pending
- Any worker can pick up an abandoned job
- Results require N independent submissions before validation (redundancy parameter)

### Work Distribution

Genome pairs are compared in spatial chunks with overlapping windows to avoid missing MEMs at boundaries. This is MapReduce — divide by region, compute independently, merge results.

### Redundancy and Validation

```
Job redundancy: 2 (two workers must agree)

Worker A submits MEMs [m1, m2, m3]
Worker B submits MEMs [m1, m2, m3]
→ agree → validated, both credited

Worker A submits MEMs [m1, m2, m3]
Worker B submits MEMs [m1, m2]
→ partial disagreement → spot check m3
→ adjust reputation accordingly
```

Validation (verifying a proposed MEM) is much cheaper than finding it. Server spot-checks, doesn't re-run everything.

### Incentives

- **Priority access** — contributors get faster downloads and higher rate limits
- **Bandwidth credits** — serve segments, earn download quota
- **Attribution** — graph version metadata credits contributing nodes
- **Scientific output** — optimization results are publishable data

### Contributor CLI

```bash
genomehub contribute --max-cpu 50% --max-bandwidth 10MB/s
genomehub contribute --bandwidth-only
genomehub contribute --once
genomehub contribute --schedule "weekdays 22:00-06:00"
```

### Reputation

```
New nodes: low trust, results need corroboration
High trust nodes: spot-checking only
Consistently wrong nodes: quarantined automatically
```

---

## Genome Comparison Prioritization (Sketching)

### Why Not Random

Comparing unrelated genomes wastes compute. A cactus and rice share almost nothing — MEM-finding between them finds nothing and wastes contributor time.

### MinHash Sketching

Cheap genome fingerprint:
1. Compute all k-mers (e.g. 21-mers) of a genome
2. Hash each k-mer
3. Keep only the smallest 1000 hashes → sketch (~8KB regardless of genome size)
4. Compare sketches between genomes → similarity score

Tool: **Mash** (existing, proven)

### Priority Tiers

```
similarity > 0.6   → high priority MEM jobs, enqueue immediately
similarity 0.2-0.6 → medium priority, enqueue when compute available
similarity < 0.2   → skip, shared segments too small to matter
```

The sketch comparison also auto-selects the minimap2 preset:
```
similarity > 0.95  → asm5  (same species)
similarity 0.7-0.95 → asm20 (same genus)
similarity < 0.5   → skip
```

### Sketch Database

```
sketch store:     genome_id → sketch (~8KB)
similarity cache: (genome_a, genome_b) → score, computed_at
```

The similarity graph is scientifically valuable in its own right — essentially a computational phylogenetic tree. Queryable: "what are the 10 most similar genomes to my assembly?"

### Full Ingestion Pipeline

```
New genome arrives
  → gear hash chunking → segments stored immediately
  → sketch computed
  → sketch compared against all existing sketches
  → similarity scores cached
  → MEM jobs enqueued by priority (preset auto-selected from similarity)
  → manifest written, header updated (graph_version=1)
  → genome immediately available for download

Background (contributor nodes, continuously):
  → pick up MEM jobs in priority order
  → run minimap2 -c --eqx, extract exact match boundaries
  → rechunk both genomes at MEM boundaries
  → server validates, integrates
  → new graph version published (graph_version=2+)
  → subscribers notified via fanout
  → clients download only segment diffs
```

---

## Technology Stack

| Component | Choice | Reason |
|---|---|---|
| Core language | Go | Single binary distribution, great concurrency, you know it well |
| Segment store | BadgerDB → RocksDB | Pure Go to start, switch when limits hit |
| Transport | QUIC (quic-go) | Multiplexed, resumable, low overhead per request |
| Manifest API | HTTP/2 + JSON | Curl-friendly, scriptable |
| Wire format | MessagePack (planned, JSON now) | Compact binary for production; JSON during development |
| Hashing | BLAKE3 | Faster than SHA-256, same security |
| CLI | cobra | Standard, well understood |
| Observability | OpenTelemetry → Prometheus + Grafana | From day one, not afterthought |
| Sketching | Mash (existing tool) | Don't reinvent, wrap it |
| Graph input | GFA format + biogo/hts | Standard pangenome format |
| Alignment (production) | minimap2 subprocess | Battle-tested, SIMD-optimized C, ~10 years of refinement |
| Alignment (native) | internal/fmindex (Go) | No external dep, per-chromosome, up to ~50 MB |

### Explicitly Avoided

- Python — distribution nightmare, performance ceiling
- Rust — iteration speed cost not worth it early on
- PostgreSQL for segments — wrong tool for blob store
- gRPC — too heavy for file transfer
- Any desktop GUI — CLI first, always
- Building a custom aligner to replace minimap2 — the bottleneck is algorithms, not code; minimap2 is optimal for full-genome alignment

### On minimap2 vs Native FM-index

minimap2 is mature, SIMD-vectorized C doing approximate alignment followed by DP for CIGAR. The native FM-index in `internal/fmindex` is O(n log²n) SA construction and finds exact matches only. They solve different sub-problems:

- **minimap2**: finds approximate syntenic blocks between full genomes (fast, production use)
- **native FM-index**: finds exact match runs within known regions, or for per-chromosome analysis (no external dep, testable)

Long-term direction: SA-IS construction + bi-directional FM-index (BWA-MEM style) could replace both for this protocol's specific needs, since we only need exact matches, not approximate alignment.

---

## Things to Learn

### Done
- ✅ **FM-index + Burrows-Wheeler Transform** — implemented in `internal/fmindex`
- ✅ **Content-addressable storage** — implemented (git objects mental model confirmed correct)

### Still Essential

1. **GFA format + variation graphs** — the pangenome graph representation standard (~1-2 weeks)
2. **Read vg and minigraph papers** — Garrison et al. 2018 (vg), Li 2018 (minimap2) (~1 week)
3. **CRAM format spec + htslib source** — understand the delta encoding model (~1-2 weeks)
4. **SA-IS algorithm** — linear-time suffix array construction, needed when sequences exceed 50 MB (~1 week)

### Learnable As You Go

- Specific plant genomics biology (polyploidy, transposable elements)
- htslib internals (read source when needed)
- GFA tooling ecosystem (vg, minigraph, seqwish, odgi)

### Key Papers

| Paper | Why |
|---|---|
| Hsi-Yang Fritz et al. 2011 | Original CRAM — delta encoding model |
| Garrison et al. 2018 | vg toolkit — variation graph model |
| HPRC 2023 | Human pangenomics — where the field is heading |
| Li 2018 | Minimap2 — fast approximate alignment at scale |
| Ferragina & Manzini 2000 | FM-index — foundational, dense but necessary |

---

## Implementation Status

### ✅ Milestone 1 — Local store + CLI (complete)

- Content-addressable segment store (BadgerDB, BLAKE3)
- Gear hash content-defined chunking (configurable min/max)
- Manifest format (JSON, per-chromosome integrity hashes, segments_root)
- `import` → `reconstruct` → `verify` pipeline confirmed byte-identical on real TAIR10 genome (119 MB, 2104 segments)
- `viz` command showing shared/unique segments across multiple manifests

### ✅ Milestone 1b — Cross-genome dedup analysis (complete)

- `align` command wrapping minimap2 with PAF streaming and identity fix (dv tag)
- `reindex` command rechunking at MEM boundaries
- Native FM-index implementation with full test suite
- `aligner.Aligner` interface with two backends (minimap2, fmindex.NativeFinder)

### ✅ Milestone 1c — Delta encoding + similarity routing (complete)

Discovered empirically (see [ADR 0001](docs/adr/0001-delta-vs-segment-dedup-routing.md))
that segment dedup is the wrong tool for **near-identical, SNP-dense** genomes
(same-species accessions): the SNP-every-~150 bp shreds shared sequence into
millions of tiny segments, capping practical savings at ~27% and bloating
manifests. The fix is to route by similarity.

- **`reindex` cross-species bug fixed** — was a hung `propagateCuts` (O(P·C²)),
  now a worklist; alignment results cached. Cross-genome dedup now works (54/48/37%
  exact-match coverage on TAIR10/Ler0/Cvi0).
- **`delta` / `reconstruct-delta`** — encode a near-identical genome as a
  reference + copy/literal patch in a compact binary format (`GHD1`: interned
  ref-chrom table, varint-delta copies, 2-bit-packed literals). Result: Ler0 (130 MB)
  → **13.1 MB** against TAIR10 (asm5), byte-identical round-trip. Reference resolves
  from the segment store (`ref_manifest`), so deltas are self-contained.
- **`sketch` / `similarity` / `similar` / `route`** — native bottom-k MinHash (no
  Mash dependency). Sketch persisted on `import`; `similar` answers "most similar
  genomes" instantly from cache; `route` picks delta (>0.95) / reindex (0.5–0.95) /
  skip (<0.5), `--execute` runs it.

### ✅ Milestone 2a — Server/client + container sim (complete)

- **`serve`** (`internal/httpapi`) — read-only content-addressed HTTP: segments,
  manifests, deltas, catalog, healthz. HTTP/1.1 for now (transport-agnostic routes;
  h2c/QUIC is a later swap). Origin and peer expose the identical API.
- **`download`** — fetches a genome (delta-aware), pulling only segments not
  already local, re-hashing each on arrival (untrusted source).
- **Docker simulation** (`docker-compose.yml`, `scripts/seed.sh`, `docs/SIMULATION.md`)
  — origin + cold/warm clients. Demonstrated live: a warm node pays **15.9 MB**
  over the wire for a 130 MB genome.

### ✅ Milestone 2b — Peer swarm (complete)

Design locked in [ADR 0003](docs/adr/0003-node-session-tracker-daemon.md).

- **`tracker`** — stateless `hash → [nodes]` index + node liveness; GC reclaims
  nodes past heartbeat timeout. Knows nothing about genome structure.
- **`node`** — long-lived peer: serves blobs, announces held hashes, heartbeats,
  re-announces content each tick, deregisters cleanly on shutdown.
- **`download --tracker`** — peer-first segment fetch (origin fallback),
  **parallel** across peers (`--parallel`, bounded worker pool), every chunk
  re-hashed. Verified in a 3-node swarm: one download spread ~evenly (47/35/42 MB)
  across nodes; warm peer pays ~16 MB for a 130 MB genome.
- **delta-as-chunks** — `delta-publish` splits a delta blob into content-addressed
  chunks + a recipe; the client fetches them through the same `/segments` path, so
  deltas swarm and dedup like genome segments ([ADR 0002 §1](docs/adr/0002-content-addressed-blobs-and-trust.md)).
- **Multi-node `docker-compose.swarm.yml`** — tracker + origin + 2 peers, verified.

### ✅ Milestone 2c — Distributed MEM-finding (complete)

Design in [ADR 0002 §5](docs/adr/0002-content-addressed-blobs-and-trust.md).

- **`coordinator`** — job queue + **re-verifies every submitted MEM** by exact byte
  comparison against its own genomes (untrusted workers; a false MEM fails the
  re-check). Validated MEMs written to `--mem-dir`.
- **`work`** — claim → reconstruct both genomes from the swarm → minimap2 → extract
  MEMs → submit. minimap2 bound to a signal context (clean Ctrl-C, no orphans).
- **Tiling** — `job-enqueue --tile` splits a pair into one job per query
  chromosome; parallel and progress streams. Verified: TAIR10×Ler0 → 5 tiles,
  46,692 MEMs, **46,692 verified (100%)**.

### ✅ Observability (complete)

- **`GET /status`** per node; live TUIs: `top` (swarm), `status` (one node +
  swarm standing via `--tracker`), `jobs --watch` (MEM queue, tiles, MEMs
  verified climbing live), and `control` — the operator pane that folds origin +
  tracker + coordinator into one screen for the layer we run ourselves.

### 🔄 Known limits

- **`-` strand in FM-index `FindMEMs`** — `ExtractExactMatches` handles `-`,
  `FindMEMs` forward only (the production path uses minimap2, which handles both).
- **`reindex` is batch-only and seed-scale only** — all-pairs is O(N²)
  ([ADR 0002 §4](docs/adr/0002-content-addressed-blobs-and-trust.md)).

### ⬜ Next Milestones

**Milestone 4 — Incremental graph updates** *(load-bearing for scale, not polish)*
- Add one genome to the existing graph without rechunking existing manifests
- Requires the explicit node→placement table (= GFA graph), enabling node-split
  ([ADR 0002 §3](docs/adr/0002-content-addressed-blobs-and-trust.md))
- Graph-version diffs: what changed between v1 and v2

**Refinements** (none blocking)
- Sub-chromosome tiling with overlap (big chromosomes); target `.mmi` reuse
- Contribution + availability reputation scoring (signal exists: per-job valid/found)
- All-pairs similarity cache on ingest; MEM-job enqueue by priority
- QUIC transport (h2c → QUIC behind the same routes); bloom-filter content announce
- Smart peer selection (currently random shuffle); interactive (navigable) TUI

---

## Open Questions

- Optimal similarity threshold for MEM job prioritization (needs empirical data)
- Minimum redundancy for job validation (2? 3?)
- Segment store GC policy (how long to retain deprecated segments)
- Whether to support lossy quality score binning for FASTQ (not just FASTA)
- Exact node reputation scoring formula
- Best minimum exact match length for dedup vs segment count tradeoff (currently 4096 bp; may need tuning per species)
- SA-IS vs comparison-sort SA: at what genome size does the difference matter in practice?

---

## Potential Collaborators / Target Users

- **Embrapa** — Brazilian agricultural genomics, world-class soybean/sugarcane/eucalyptus work
- **UNIPAMPA biology/agronomy departments** — local academic collaboration, validation partner
- **PLAZA** — plant-specific public genomics database, natural distribution partner
- Any lab that works with non-model plant species where CRAM's single linear reference breaks down

---

*Last updated: 2026-06-11*

## Architecture Decision Records

See `docs/adr/`:

- [ADR 0001](docs/adr/0001-delta-vs-segment-dedup-routing.md) — route storage by similarity: delta for near-identical genomes, segment dedup for diverged ones
- [ADR 0002](docs/adr/0002-content-addressed-blobs-and-trust.md) — everything is a content-addressed blob; trust splits into correctness / availability / contribution; distributed MEM-finding; the O(N²) scaling boundary
- [ADR 0003](docs/adr/0003-node-session-tracker-daemon.md) — node session lifecycle, the tracker, and the contribute daemon

---

## License

[PolyForm Noncommercial License 1.0.0](LICENSE) — see [`LICENSE`](LICENSE).

GenomeHub is **source-available, not open source**. You may use, modify, fork,
and redistribute it freely **for any noncommercial purpose** — research,
education, personal study, and use by academic, government, public-research, and
other nonprofit organizations all qualify. Forks must keep this same license.

**Commercial / for-profit use is not granted** by this license. If you want to
use GenomeHub in a commercial context, contact the author for a separate
commercial license.

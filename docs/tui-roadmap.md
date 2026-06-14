# TUI Roadmap — from monitor to control center

The `dash` TUI today is **read-only**: it polls a node's `/status` (+ the tracker)
and renders. The goal is to make it the participant's full control center —
discover genomes, see what you partially hold, download the rest, update
manifests, delete from cache, stop seeding — all from the keyboard.

## The core constraint

badger locks a store to **one process**. The running node owns that lock, so a
separate TUI process **cannot** mutate the store (download/delete) while the node
runs. Every mutating action must therefore be performed **by the node itself**,
exposed via a control API. The node becomes an agent the TUI drives; download
moves *into* the node as a cancellable background task (which makes
pause/resume/progress fall out for free).

## Backlog (ordered)

### 0. Quick fix — interrupted downloads should show as partial
- `download` writes the manifest cache only *after* `fetchAll` completes, so an
  interrupted download leaves partial segments but **no manifest** → shows as
  nothing instead of partial.
- Move `saveManifestCache` to right after the manifest is fetched (before
  `fetchAll`). One-line move; makes the most common real partial render.

### 1. Discovery / Search tab (read-only) — DONE
- `GET /registry` (httpapi): cheap genome summaries (assembly, organism, version,
  segments, bases) — no hashes, lists even huge genomes.
- `GET /discover` (httpapi): node pulls its configured `--registry` (origin),
  computes coverage of each genome against its own store — including genomes not
  in its catalog (their manifests fetched once + cached). Returns have/total +
  `local`. This is what makes a node *learn* it half-holds ATHENA2 and that
  ECOLI/HSAP exist, none of which it imported.
- `--registry` flag on `node` and `serve`.
- dash **Discover** tab (3): searchable, coverage bar, version, **SRC** (sources),
  species, state. Actions wired (see #2/#3/#5/#6): T/U/D/c/p/x/S/X.
- **Aggregated across the swarm**: `/discover` unions the registry of *every*
  online node the tracker knows (plus origin), counting sources per genome
  (availability). Survives origin going down and surfaces peer-only genomes —
  not a single upstream. Coverage manifests fetched from any source, cached.
- Verified: peer1 discovers ATHENA(src=3)/ATHENA2(2)/ECOLI(2)/HSAP(1); after
  killing origin, discovery still returns ATHENA(2)/ATHENA2(1)/ECOLI(1) via peers.

### 2. Node control plane (the unlock) — DONE (segment download)
- `POST /actions/download {assembly}` runs an in-node, cancellable background
  task (`internal/httpapi/download.go`): tracks the manifest, then fetches every
  missing segment from the registry (verify → Put). Runs in the node — the only
  process that can write the store while it serves.
- `POST /actions/download/{pause,resume,cancel} {assembly}`. Pause is a flag the
  fetch loop honours between segments; cancel is a context. Safe to stop anytime
  because each segment is atomic (verified, then Put) and skip-local makes resume
  free — exactly the hash-by-hash model.
- Live progress in `/status.Downloads` (have/total/bytes/state) — no extra poll.
- TUI (Discover): `D` download the rest, `p` pause/resume, `x` cancel; in-row
  `↓ have/total` marker + a progress bar on the selected detail line.
- Verified: empty node downloads ATHENA2 5/5 (400 KB); partial node (3/5) fetches
  only the 2 missing (160 KB).
- Still TODO: fetch via tracker peers (currently registry/origin only); a small
  worker pool (currently sequential); `chroms` subset (see #4).

### 3. Manifest track / update (`T` / `U`) — DONE (lock-free)
- `POST /actions/manifest {assembly}` on the node: fetches the manifest from the
  upstream registry, persists it to manifestDir, splices it into a fresh catalog
  (copy-on-write via `atomic.Pointer[Catalog]`), and invalidates cached coverage.
  No store lock (a manifest is a file), so it works while the node is live.
- Catalog is now `atomic.Pointer[Catalog]` (`srv.cur()`), swapped on track/update.
- dash keys on Discover/Seeding: `T` track, `U` update — the selected genome
  moves into Seeding at its real coverage with no restart. Verified: empty-catalog
  node tracks ATHENA2 → Seeding shows 3/5 live.
- Still TODO: show a "newer version available" badge (compare local vs registry
  version) so `U` has a visible prompt. The mechanism (versions in registry +
  seeding) is in place.

### 4. Partial / per-chromosome download — DONE
- `GET /genomes/{assembly}/chromosomes` → per-chrom coverage (have/segments).
- `POST /actions/download {assembly, chroms[]}` fetches only the named
  chromosomes' segments (`chromHashes`).
- TUI: `c` opens a chromosome picker (space toggle, `a` all, enter download).
  Defaults to selecting chromosomes you don't fully hold. Verified: download
  chr4+chr5 of ATHENA2 → seeding 2/5.

### 5. Delete from cache (`X`) — DONE
- `store.Delete(hash)` added (badger txn.Delete).
- `POST /actions/delete {assembly}` reference-counts across held manifests: frees
  only segments no other held genome needs, keeps shared ones, then unseeds.
  Returns {deleted, kept}. Verified: delete ATHENA2 with ATHENA held → freed 2
  unique, kept 3 shared, ATHENA intact.
- TUI: `X` with a `[y/N]` confirm.

### 6. Stop seeding (`S`) — DONE
- `POST /actions/unseed {assembly}` removes the manifest from the catalog (copy-
  on-write) and deletes the tracked manifest file (only under manifestDir),
  keeping all segments. Verified: unseed ATHENA → held stays 5, seeding empties.
- TUI: `S` with a `[y/N]` confirm.

### 8. Delta genome download — DONE
- `runDownload` now probes `/deltas/{a}` first (mirrors CLI `fetchGenome`). If
  the registry returns a recipe, it routes to `runDeltaDownload`; if a raw GHD1
  blob, the file is already tracked (no chunks to fetch) and the reference
  download is kicked off immediately.
- `trackDelta`: fetches the delta/recipe from the registry, persists it to
  `manifestDir` as `{assembly}.deltarecipe.json` or `.delta.ghd`, splices it
  into the catalog copy-on-write. Lock-free (file op), same pattern as
  `trackManifest`.
- `runDeltaDownload`: fetches all recipe chunks through the same 8-worker peer-
  routed pool as manifest segments. On completion, automatically starts a
  separate download task for the reference genome — the node needs the reference
  to reconstruct / re-serve the delta. Progress lives in `/status.Downloads`
  as two entries (e.g. "Ler0" and "TAIR10").
- Coverage decisions:
  - Recipe-backed delta: `Have/Total = chunks held / total chunks`. Measures
    "can this node serve the delta artifact?" — honest and directly actionable.
  - Raw delta blob: `Have=1, Total=1` (file on disk, always fully "seeded").
  - Reference genome is tracked as a normal manifest entry — its coverage is
    shown separately, not blended into the delta's bar.
- `registry()` now lists delta/recipe assemblies (Kind="delta",
  Segments=chunk count) so they appear in `/discover` on peers.
- `discoverHashes` falls back to fetching `/deltas/{a}` (recipe JSON) when
  `/genomes/{a}/manifest` 404s — so Ler0 coverage shows in Discover without a
  manual track step.
- Seeding view: recipe-backed deltas render a real coverage bar (green/amber/red
  with pct and "delta full / delta partial / delta 0 chunks" label). Raw blobs
  still render as "delta (file-served)".
- Chromosome picker disabled for delta genomes (flashes a message); `c` on a
  delta is a no-op.
- Verified: `TestDeltaDownload` in `internal/httpapi/control_test.go` — in-
  process origin serves TAIR10 manifest + Ler0 recipe; peer downloads Ler0 via
  `/actions/download`; asserts chunk coverage + TAIR10 auto-download.
- Still TODO: delete/unseed for delta genomes (currently only works for
  manifests); per-chunk Bloom for lazy delta coverage at scale.

### 7. Peer-routed + parallel download — DONE
- `fetchSegment` tries tracker peers first (`/peers/{hash}`, shuffled to spread
  load), origin/registry fallback. `--tracker` flows into the node handler.
- 8-worker pool; pause/cancel honoured per worker. Verified in the swarm: peer2
  downloading ATHENA pulled segments from both peer1 and origin.

## Big-manifest coverage (discover at real scale)

A genome's manifest lists every segment hash (CVI0 ≈ 349 MB). Listing is cheap
(registry summary has counts, no hashes), but computing *coverage* needs the hash
set → fetching the full manifest. That stalls a browse on real data.

### A. Lazy + size guard — DONE
- `discover` computes exact coverage only for genomes held locally (free) or with
  `Segments <= maxEagerSegments` (20000). Bigger genomes are returned with
  `coverage_known=false` and no manifest fetch.
- `GET /coverage/{assembly}` computes exact coverage on demand.
- TUI: deferred rows show `····  ?` ("select to size"); selecting one fires
  `/coverage`, and the result is cached client-side so it survives the 2 s
  discover refresh.

### B. Per-genome Bloom filter — TODO (approximate coverage everywhere)
- Genome publishes a Bloom of its segment hashes in the registry (~3.6 MB for 3 M
  segments vs 349 MB manifest, ~100×). Node tests its held hashes against it →
  approximate coverage without the manifest. Exact stays available at download.
- Simpler than C: static artifact built at import, fetched once, no origin state.

### C. Per-store Bloom (push) — TODO (only if centralizing cost on origin)
- Node uploads one Bloom of its whole store; origin tests each genome's hashes
  against it and returns counts. Manifest never leaves origin. Costs: origin holds
  per-node blooms + must stream big manifests on demand (needs a streaming
  manifest hash reader to avoid loading 349 MB into RAM). Reach for this only if
  origin-side exactness/centralization becomes a requirement.

## Real scenarios that produce a partial seeder
1. **Overlap with something you hold** — related accession / same species /
   shared repeats / prior version. Automatic via content addressing.
2. **Interrupted / resumed download** — see item 0.
3. **Version drift (v1 → v2)** — you hold v1; v2 shares most segments.
4. **Disk-capped cache eviction (LRU)** — not built; natural for a bounded cache.
5. **Region/chromosome-subset fetch** — item 4.

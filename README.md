# GenomeHub

**Content-addressable, deduplicating distribution for genomic data.** Store shared
sequence once, reconstruct any genome from shared building blocks, and download only
the parts you don't already have.

Genomes from related organisms are massively redundant. Instead of every lab
re-downloading the same 30 GB, GenomeHub stores sequence as content-addressed
segments — identical sequence has an identical hash, so it's stored and transferred
exactly once. Think *BitTorrent meets git objects, built for biological sequence*.

A single static Go binary. The core researcher workflow has **no external dependencies**.

```text
A 32 GB genome, 90% similar to one you already have:
  naive download ........ 32 GB
  with GenomeHub ........ ~3.2 GB   (only the 10% that's new)
```

> [PolyForm Noncommercial 1.0.0](LICENSE) — free for research, education, and nonprofit
> use. Commercial use requires a separate license. Source-available, not open source.

---

## Contents

- [Install](#install)
- [Quickstart](#quickstart-5-minutes-fully-offline) — offline round-trip + dedup payoff
- [Just want to download a genome?](#just-want-to-download-a-genome)
- [Run a network](#run-a-network) — serve, peers, tracker, TLS, auth, signing
- [Command reference](#command-reference)
- [How it works](#how-it-works)
- [Documentation](#documentation)
- [Status](#status)
- [License](#license)

---

## Install

**One-liner** (auto-detects OS/arch, grabs the latest release):

```bash
# macOS / Linux
curl -fsSL https://raw.githubusercontent.com/luizeduardocarvalho/genomehub/main/install.sh | sh
```
```powershell
# Windows (PowerShell)
irm https://raw.githubusercontent.com/luizeduardocarvalho/genomehub/main/install.ps1 | iex
```

<details>
<summary>Other install methods (prebuilt archive, <code>go install</code>, from source)</summary>

**Prebuilt archive** — from the [releases page](https://github.com/luizeduardocarvalho/genomehub/releases):

```bash
tar xzf genomehub_*_$(uname -s)_$(uname -m).tar.gz
sudo mv genomehub /usr/local/bin/
```

**With the Go toolchain** (Go 1.24+):

```bash
go install github.com/luizeduardocarvalho/genomehub@latest
```

**From source:**

```bash
git clone https://github.com/luizeduardocarvalho/genomehub
cd genomehub
make install        # builds with version metadata → /usr/local/bin
# or: make build    # just produces ./genomehub
```
</details>

Verify: `genomehub version` prints a version, commit, and build date.

**Dependencies:** none for the researcher workflow (`import`, `reconstruct`, `verify`,
`download`, `viz`). `minimap2` is only needed for contributor/server commands
(`align`, `reindex`, `work`) — install it with `brew install minimap2` if you run those.

---

## Quickstart (5 minutes, fully offline)

No server, no network, no minimap2. This proves the core round-trip and the
deduplication payoff on genomes you already have. All state lives under `~/.genomehub`
(override with `--store`).

**1. Ingest a FASTA** — chunk it into content-addressed segments + write a manifest:

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
genomehub verify --original genome.fa --reconstructed rebuilt.fa   # exits non-zero on mismatch
```

**4. See the dedup** — import a *related* genome (same species, a prior version,
anything sharing sequence). Shared segments are stored once:

```bash
genomehub import --fasta other.fa --organism "Arabidopsis thaliana" \
  --assembly Ler0 --output Ler0.manifest.json
genomehub viz TAIR10.manifest.json Ler0.manifest.json
# bar chart of shared vs unique, bytes stored vs naive, savings %
```

> For **near-identical genomes** (same-species accessions), `delta` encoding beats
> segment dedup — Ler0 (130 MB) compresses to **13.1 MB** against TAIR10, byte-identical.
> See the delta / similarity-routing commands in the [command reference](#command-reference).

---

## Just want to download a genome?

```bash
genomehub download --server https://cdn.example.org --assembly TAIR10 --output TAIR10.fa
```

`download` fetches the manifest, checks which segments you already have, and pulls only
the missing ones — re-hashing each on arrival (the source is untrusted). With
`--tracker <url>`, it fetches from peers first and falls back to the origin.

See **[docs/users.md](docs/users.md)** for the full user guide (browse, download, seed).

---

## Run a network

GenomeHub scales from a single read-only server to a peer swarm. The read API is
immutable and content-addressed, so an "origin" can even be **static files on free
object storage** — no running server required.

> **Deploy guides:**
> - [docs/deploy-mirror.md](docs/deploy-mirror.md) — **recommended backbone:** static
>   mirror on Cloudflare R2 (free egress, no server to run, ~$0–5/mo for 100 genomes).
> - [docs/deploy-swarm.md](docs/deploy-swarm.md) — optional live P2P swarm (tracker +
>   origin node) for peer offload.

### Serve, peers, tracker

```bash
genomehub serve --catalog ./catalog --addr :8080            # read-only node
genomehub tracker --addr :9000                              # stateless hash→nodes index
genomehub node --tracker http://tracker:9000 --addr :8080 \ # long-lived peer
  --advertise http://me:8080 --catalog ./catalog
genomehub download --server http://origin:8080 --tracker http://tracker:9000 \
  --assembly Ler0 --output Ler0.fa --parallel 8             # peer-first, parallel
```

Want to help host with one command? `genomehub seed` auto-configures and runs a node
(add `--tunnel` for a public URL via Cloudflare).

### Static mirror (near-zero-cost hosting)

```bash
genomehub mirror --catalog ./catalog --store ./store --out ./static --sign-key origin.key
rclone copy ./static r2:my-bucket    # or: aws s3 sync / wrangler
```

Egress — normally ~90% of a cloud bill — is the part this design eliminates.

### Security

Set these on any network-reachable node. **Details and rationale in
[DESIGN.md](DESIGN.md#distribution--server-peers-tracker).**

| Concern | Flag | What it does |
|---|---|---|
| Transport | `--tls-cert` / `--tls-key` | HTTPS; authenticates the origin. `--insecure` on clients for self-signed dev certs only. |
| Control plane | `GENOMEHUB_TOKEN` env | Bearer token gates writes/deletes. **Set one, or anyone reachable can unseed your data.** |
| Manifest trust | `--sign-key` / `--verify-key` | Origin signs manifests (ed25519); downloads verify even when relayed by an untrusted peer. |
| Node identity | `--identity` (+ tracker `--require-identity`) | Signed announces — no one else can announce as your node. |
| Abuse | `--cache-max <size>`, `--rate <n>` | Bounded LRU cache (cache nodes only) + per-IP rate limit. |

```bash
export GENOMEHUB_TOKEN=$(openssl rand -hex 32)
genomehub keygen --out origin       # origin.key (secret) + origin.pub
genomehub serve --catalog ./catalog --addr :8443 \
  --tls-cert cert.pem --tls-key key.pem --sign-key origin.key
```

### Contribute / publish a genome

```bash
genomehub import --fasta my-soybean.fa --organism "Glycine max" \
  --assembly MySoy --output MySoy.manifest.json
genomehub publish --manifest MySoy.manifest.json --server https://origin:8443
```

Only segments the origin is **missing** are uploaded; it re-hashes every one (a
mislabeled segment is rejected), and accepts the manifest only once all segments are
present — so the catalog never gains an unreconstructable genome.

---

## Command reference

`--store <path>` is accepted everywhere (default `~/.genomehub/segments`).
Run `genomehub <command> --help` for full flags. Internals in [DESIGN.md](DESIGN.md#cli-reference).

**Researcher (no dependencies)**

| Command | Purpose |
|---|---|
| `import` | Chunk a FASTA into content-addressed segments + write a manifest |
| `reconstruct` | Rebuild a FASTA from a manifest + stored segments (verifies per-chromosome hash) |
| `verify` | Confirm a reconstructed FASTA matches the original (sequence-level) |
| `viz` | Visualize shared vs unique segments across manifests + savings % |
| `download` | Fetch a genome, pulling only missing segments (peer-aware with `--tracker`) |

**Storage strategy (delta + similarity routing)**

| Command | Purpose |
|---|---|
| `sketch` / `similarity` / `similar` | MinHash fingerprints; nearest-neighbour lookup from cache |
| `route` | Pick strategy by similarity: delta (>0.95) / reindex (0.5–0.95) / skip |
| `delta` / `reconstruct-delta` / `delta-publish` | Encode a near-identical genome as a patch; rebuild; chunk it so it swarms |

**Server / contributor (needs `minimap2`)**

| Command | Purpose |
|---|---|
| `serve` / `node` / `tracker` | Read-only server · long-lived peer · stateless hash→nodes index |
| `seed` | Become a seeder in one command (auto-setup + run a node; `--tunnel` for a public URL) |
| `mirror` | Export the catalog as a static tree for object-storage hosting |
| `publish` | Push a locally-imported genome to an origin (uploads only missing segments) |
| `align` / `reindex` | Find aligned blocks (minimap2) · rechunk N genomes at shared MEM boundaries |
| `coordinator` / `work` / `job-enqueue` / `jobs` | Distributed MEM-finding: queue · worker · enqueue · monitor |
| `keygen` / `config` | ed25519 keypairs · persist config (e.g. `config set auth-token`) |

**Monitoring (live TUIs)**

| Command | Purpose |
|---|---|
| `control` | Operator pane: origin + tracker + coordinator on one screen |
| `top` / `status` / `dash` | The swarm · one node + its swarm standing · control dashboard |

---

## How it works

- **Content-addressable segments** — every segment is identified by the BLAKE3 hash of
  its content. Identical sequence in two genomes → identical hash → stored once. No
  coordination needed.
- **Manifests** — a manifest is the recipe for a genome: an ordered list of segment
  hashes per chromosome. Reconstruction = fetch each hash in order, concatenate.
- **Content-defined chunking** — a rolling gear hash cuts boundaries by content, so an
  insertion shifts only the chunks around it. For diverged genomes, `minimap2` alignment
  finds the true shared boundaries (MEM-boundary reindexing).
- **Right tool per similarity** — segment dedup for diverged genomes; delta encoding for
  near-identical same-species accessions; the system routes automatically by sketch.

Full design — architecture, manifest format, chunking, the integrity/trust chain,
distributed MEM-finding, and the empirical findings behind these choices — is in
**[DESIGN.md](DESIGN.md)** and the [ADRs](docs/adr/).

---

## Documentation

| Doc | What's in it |
|---|---|
| [docs/users.md](docs/users.md) | User guide: install, browse, download, seed |
| [docs/deploy-mirror.md](docs/deploy-mirror.md) | Static mirror on R2 (recommended backbone) |
| [docs/deploy-swarm.md](docs/deploy-swarm.md) | Live P2P swarm deployment |
| [docs/SIMULATION.md](docs/SIMULATION.md) | Local docker simulation (origin + peers) |
| [DESIGN.md](DESIGN.md) | Full design document — concepts, architecture, internals |
| [docs/adr/](docs/adr/) | Architecture Decision Records (the *why* behind the design) |

---

## Status

Milestones 1–2c complete: local store + CLI, cross-genome dedup, delta + similarity
routing, server/client, peer swarm, and distributed MEM-finding (all verified on real
*Arabidopsis* genomes). Next: incremental graph updates. Full breakdown in
[DESIGN.md](DESIGN.md#implementation-status).

---

## License

[PolyForm Noncommercial License 1.0.0](LICENSE).

GenomeHub is **source-available, not open source**. Use, modify, fork, and redistribute
freely **for any noncommercial purpose** — research, education, personal study, and use
by academic, government, public-research, and nonprofit organizations all qualify. Forks
must keep this same license. **Commercial / for-profit use is not granted** — contact the
author for a separate commercial license.

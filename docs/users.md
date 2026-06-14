# Using GenomeHub (for researchers)

GenomeHub distributes genomes as content-addressed segments: you download only
what you don't already have, and every segment is re-hashed + the manifest
signature verified on arrival. No account, no token — reads are open.

This page assumes a live origin. The reference deployment:

- origin:  `https://genomehub.duckdns.org:8443`
- tracker: `https://genomehub.duckdns.org:9000`
- verify key (origin's signing public key):
  `f6208cf8aceecaab4bda26f254e714f646e22b5a3209070f08701f756df31d29`

> Replace these with your own origin's values if you run a different network.

---

## 1. Install

**One-liner (recommended):**

macOS / Linux:
```bash
curl -fsSL https://raw.githubusercontent.com/luizeduardocarvalho/genomehub/main/install.sh | sh
```
Windows (PowerShell):
```powershell
irm https://raw.githubusercontent.com/luizeduardocarvalho/genomehub/main/install.ps1 | iex
```
Each detects your OS/arch, downloads the latest release, and puts `genomehub` on
your PATH. Then `genomehub version` confirms it.

<details><summary>Manual install (if you prefer)</summary>

**macOS (Apple Silicon):**
```bash
curl -L https://github.com/luizeduardocarvalho/genomehub/releases/latest/download/genomehub_0.1.1_darwin_arm64.tar.gz | tar xz
sudo mv genomehub /usr/local/bin/
genomehub version
```
Intel Mac → `darwin_amd64`.

**Linux (x86-64):**
```bash
curl -L https://github.com/luizeduardocarvalho/genomehub/releases/latest/download/genomehub_0.1.1_linux_amd64.tar.gz | tar xz
sudo mv genomehub /usr/local/bin/
genomehub version
```
ARM → `linux_arm64`.

**Windows (PowerShell, x86-64):**
```powershell
Invoke-WebRequest -Uri https://github.com/luizeduardocarvalho/genomehub/releases/latest/download/genomehub_0.1.1_windows_amd64.zip -OutFile genomehub.zip
Expand-Archive genomehub.zip -DestinationPath $env:USERPROFILE\genomehub
cd $env:USERPROFILE\genomehub
.\genomehub.exe version
```
ARM → `windows_arm64`. Use **Windows Terminal / PowerShell 7** for the TUI.

**With the Go toolchain (any OS):**
```bash
go install github.com/luizeduardocarvalho/genomehub@latest
```
</details>

No external dependencies for downloading. (`minimap2` is only needed for
contributor commands like `reindex`/`align`, not for downloading.)

---

## 2. Browse what's available

```bash
# quick list
curl -s https://genomehub.duckdns.org:8443/registry        # JSON of genomes

# interactive TUI (browse, coverage, swarm view)
genomehub dash --server https://genomehub.duckdns.org:8443 \
  --tracker https://genomehub.duckdns.org:9000
```

The dash is for **browsing and monitoring**. To actually save a genome to your
disk, use `download` (next) — the dash does not write FASTA files locally.

---

## 3. Download a genome

```bash
genomehub download \
  --server https://genomehub.duckdns.org:8443 \
  --tracker https://genomehub.duckdns.org:9000 \
  --assembly Phypa_V3 --output ppatens.fa \
  --verify-key f6208cf8aceecaab4bda26f254e714f646e22b5a3209070f08701f756df31d29
```

What happens: peers are tried first (origin as fallback), only segments you
don't already hold are fetched, each is re-hashed, and the origin's signature on
the manifest is verified. `ppatens.fa` is a standard FASTA.

If a download is interrupted, just run it again — it resumes (already-fetched
segments are skipped).

Windows is identical with `.\genomehub.exe` and backtick line-continuations.

---

## 4. (Optional) Become a seeder

If your machine is **publicly reachable** (a lab server / VPS with an open
port), you can reshare what you hold and take load off the origin:

```bash
genomehub keygen --out mynode
genomehub node --tracker https://genomehub.duckdns.org:9000 \
  --advertise https://your-public-host:8443 \
  --store ./store --catalog ./catalog --identity mynode.key
```

**Behind home/office NAT?** A laptop can't accept inbound connections, so it
can't serve peers directly (GenomeHub fetches over HTTP — a seeder must be a
reachable server). To seed from a NAT'd machine, give it a public URL with a
tunnel:

```bash
cloudflared tunnel --url http://localhost:8080 &     # prints a public https URL
genomehub node --tracker https://genomehub.duckdns.org:9000 \
  --advertise https://<that-public-url> \
  --store ./store --catalog ./catalog --identity mynode.key
```

In practice the swarm is carried by a handful of **public institutional
seeders** (the origin + partner labs); laptops are consumers. Frictionless
"download = seed from anywhere" needs a true P2P transport (see
[ADR 0004](adr/0004-p2p-transport-libp2p.md)) and is future work.

---

## Troubleshooting

- **`tls: ... timeout` / connection hangs** — the origin may be a small VM under
  load; retry, or lower `--parallel` (e.g. `--parallel 2`).
- **`signature does not match`** — wrong `--verify-key`, or you're pointed at the
  wrong origin. Use the origin's published public key.
- **Windows TUI looks garbled** — use Windows Terminal / PowerShell 7, not legacy
  `cmd.exe`.

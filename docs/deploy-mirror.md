# Deploy the content backbone on Cloudflare R2 (~$0, no server)

The read API is immutable + content-addressed, so the origin doesn't need a
running server: `genomehub mirror` exports the whole catalog as a static object
tree, and any object store with free egress serves it. **Cloudflare R2** is the
recommended backbone — free egress, a global CDN, no compute to babysit.

Result: clients download from a rock-solid `https://…` endpoint that costs
effectively nothing, even at hundreds of users. The live P2P swarm (tracker +
nodes) becomes an optional bonus on top, not the critical path.

---

## What you need

- A **Cloudflare account** (free): <https://dash.cloudflare.com/sign-up>
- **rclone** to upload: <https://rclone.org/downloads/> (`brew install rclone`)
- The genome data to mirror: a **segment store** + **catalog** (manifests), and
  the **origin signing key** (`origin.key`) so manifests are signed in the tree.
  Run `mirror` wherever you have all three (your origin box or operator machine).

## 1. Create the R2 bucket + public URL

1. Cloudflare dashboard → **R2** → **Create bucket** (e.g. `genomehub`).
2. Bucket → **Settings → Public access**:
   - enable the **r2.dev** managed subdomain → you get `https://pub-XXXX.r2.dev`, **or**
   - **Connect a custom domain** (e.g. `cdn.yourlab.org`) for a clean URL + caching.
3. Note that public URL — it becomes the client `--server`.

## 2. R2 API token for uploads

R2 → **Manage R2 API Tokens** → **Create** (Object Read & Write). Save:
- Access Key ID, Secret Access Key
- Account ID → endpoint is `https://<ACCOUNT_ID>.r2.cloudflarestorage.com`

## 3. Configure rclone for R2

Add to `~/.config/rclone/rclone.conf`:

```ini
[r2]
type = s3
provider = Cloudflare
access_key_id = <ACCESS_KEY_ID>
secret_access_key = <SECRET_ACCESS_KEY>
endpoint = https://<ACCOUNT_ID>.r2.cloudflarestorage.com
region = auto
```

## 4. Mirror + upload

```bash
genomehub mirror --catalog ./catalog --store ./store --out ./static --sign-key origin.key
rclone copy ./static r2:genomehub --progress
```

`mirror` writes `segments/{hash}`, `genomes/{a}/manifest(.sig)`, `deltas/{a}`,
and `pubkey`, keyed exactly as clients request them, deduping segments across
genomes. `rclone copy` is incremental — re-running only uploads new objects.

## 5. Point clients at it

```bash
genomehub download --server https://pub-XXXX.r2.dev \
  --assembly Phypa_V3 --output ppatens.fa \
  --verify-key <origin.pub hex>
```

No token, no compute, free egress. The manifest signature is in the tree, so the
download is verified end-to-end even though the host is "dumb" static storage.

## 6. (Optional) Combine with the swarm

Use R2 as the always-available fallback and the swarm for peer offload:

```bash
genomehub download --server https://pub-XXXX.r2.dev \
  --tracker https://genomehub.duckdns.org:9000 \
  --assembly Phypa_V3 --output ppatens.fa --verify-key <origin.pub>
```

Peers are tried first; R2 backs everything when no peer holds a segment. Best of
both: P2P offload when available, a bulletproof backbone always.

## 7. Updating the catalog

Add genomes to your store/catalog (via `import`, or `publish` to your origin then
mirror that origin's store), then re-run:

```bash
genomehub mirror --catalog ./catalog --store ./store --out ./static --sign-key origin.key
rclone copy ./static r2:genomehub --progress
```

Only new content-addressed objects upload. Old ones are immutable, so nothing
re-transfers.

---

## Notes

- **R2 free tier**: 10 GB storage, 10M reads/month, **egress free**. Plenty for
  100 genomes / hundreds of users; storage is ~$0.015/GB beyond 10 GB.
- **Custom domain** (step 1) adds Cloudflare CDN caching in front — hot segments
  served from the edge, even faster.
- This makes the weak/free origin VM **non-critical**: it can be a swarm seeder or
  go away entirely; downloads never depend on it.
- Recommended default `--server` for your users once live: the R2 URL. Set it
  with `genomehub config` defaults (or document it on your web page).

# Deploy a GenomeHub swarm (one free VM)

A complete swarm needs two always-on processes — a **tracker** (hash → peers
index) and at least one **seeder node** holding the full catalog (nodes are
caches; something must always have the data). Both fit on a single free VM.

This runbook targets **Oracle Cloud Always-Free** (Ampere ARM, Ubuntu) with the
free **DuckDNS** hostname `genomehub.duckdns.org`. One hostname serves both
services on different ports:

- tracker → `https://genomehub.duckdns.org:9000`
- origin node → `https://genomehub.duckdns.org:8443`

TLS is terminated by **Caddy** (automatic Let's Encrypt); the GenomeHub
processes run plaintext on localhost behind it.

---

## 0. Accounts / DNS (done once)

- Oracle Cloud Always-Free VM (Ampere A1, Ubuntu 22.04): https://www.oracle.com/cloud/free/
- DuckDNS hostname `genomehub.duckdns.org`: https://www.duckdns.org/
  → set it to the VM's **public IP** (DuckDNS dashboard, or its update URL).

Open ingress for **80** (ACME/TLS issuance), **8443** (node), **9000**
(tracker) in **both** places on Oracle:

1. VCN → Security List → add ingress rules (0.0.0.0/0, TCP 80, 8443, 9000).
2. On the VM, Oracle's Ubuntu image ships restrictive iptables — open them too:

```bash
sudo iptables -I INPUT -p tcp --dport 80   -j ACCEPT
sudo iptables -I INPUT -p tcp --dport 8443 -j ACCEPT
sudo iptables -I INPUT -p tcp --dport 9000 -j ACCEPT
sudo netfilter-persistent save
```

---

## 1. Install the binary

```bash
# ARM64 release (Ampere). Swap arch if on x86.
curl -L -o genomehub.tar.gz \
  https://github.com/luizeduardocarvalho/genomehub/releases/latest/download/genomehub_*_linux_arm64.tar.gz
tar xzf genomehub.tar.gz
sudo mv genomehub /usr/local/bin/
genomehub version
```

## 2. Keys

```bash
sudo mkdir -p /etc/genomehub && cd /etc/genomehub
genomehub keygen --out origin     # origin.key = signs manifests (SECRET); origin.pub = share with users
genomehub keygen --out node       # node.key   = this node's network identity
openssl rand -hex 32 | sudo tee token.txt   # control-plane auth token (SECRET)
```

## 3. Ingest genomes

```bash
sudo mkdir -p /var/lib/genomehub/{store,catalog}
genomehub import --fasta soybean.fa --organism "Glycine max" --assembly MySoy \
  --store /var/lib/genomehub/store --output /var/lib/genomehub/catalog/MySoy.manifest.json
# repeat per genome; optionally `genomehub delta`/`reindex` first for dedup savings
```

## 4. Caddy (TLS front)

Install Caddy (https://caddyserver.com/docs/install), then `/etc/caddy/Caddyfile`:

```
genomehub.duckdns.org:8443 {
	reverse_proxy localhost:8080
}
genomehub.duckdns.org:9000 {
	reverse_proxy localhost:9001
}
```

```bash
sudo systemctl restart caddy   # obtains a Let's Encrypt cert via :80 automatically
```

## 5. systemd units

`/etc/systemd/system/genomehub-tracker.service`:

```ini
[Unit]
Description=GenomeHub tracker
After=network.target

[Service]
ExecStart=/usr/local/bin/genomehub tracker --addr 127.0.0.1:9001 --require-identity --verify-announce
Restart=always
User=genomehub

[Install]
WantedBy=multi-user.target
```

`/etc/systemd/system/genomehub-node.service`:

```ini
[Unit]
Description=GenomeHub origin seeder
After=network.target genomehub-tracker.service

[Service]
EnvironmentFile=/etc/genomehub/node.env
ExecStart=/usr/local/bin/genomehub node \
  --tracker https://genomehub.duckdns.org:9000 \
  --addr 127.0.0.1:8080 \
  --advertise https://genomehub.duckdns.org:8443 \
  --catalog /var/lib/genomehub/catalog \
  --store /var/lib/genomehub/store \
  --identity /etc/genomehub/node.key \
  --sign-key /etc/genomehub/origin.key
Restart=always
User=genomehub

[Install]
WantedBy=multi-user.target
```

`/etc/genomehub/node.env`:

```
GENOMEHUB_TOKEN=<contents of /etc/genomehub/token.txt>
```

```bash
sudo useradd -r -s /usr/sbin/nologin genomehub
sudo chown -R genomehub: /var/lib/genomehub /etc/genomehub
sudo systemctl daemon-reload
sudo systemctl enable --now genomehub-tracker genomehub-node
sudo systemctl status genomehub-node --no-pager
```

---

## 6. Verify from your laptop

```bash
# tracker reachable
curl https://genomehub.duckdns.org:9000/healthz          # -> ok
# node serving + signed
curl https://genomehub.duckdns.org:8443/genomes/MySoy/manifest.sig -o /dev/null -w '%{http_code}\n'  # 200

# full download, peer-first, signature-verified (origin.pub = contents of /etc/genomehub/origin.pub)
genomehub download \
  --tracker https://genomehub.duckdns.org:9000 \
  --server  https://genomehub.duckdns.org:8443 \
  --assembly MySoy --output MySoy.fa \
  --verify-key <paste origin.pub hex>
genomehub verify --original soybean.fa --reconstructed MySoy.fa
```

## 7. Share with users

Publish two things:

- tracker + origin URLs above
- the **origin public key** (`origin.pub`) for `--verify-key`

Users download exactly as in step 6.

## 8. Add more seeders (the actual swarm)

On any other public, reachable host (partner lab, second VM):

```bash
genomehub keygen --out thisnode
# pull the genomes once (caches manifests + segments locally)
genomehub download --tracker https://genomehub.duckdns.org:9000 \
  --server https://genomehub.duckdns.org:8443 --assembly MySoy --output /dev/null \
  --store /var/lib/genomehub/store --verify-key <origin.pub>
# then reseed
genomehub node --tracker https://genomehub.duckdns.org:9000 \
  --advertise https://that-host:8443 --catalog /var/lib/genomehub/catalog \
  --store /var/lib/genomehub/store --identity thisnode.key
```

---

## Notes & caveats

- **Reachability is required to seed.** A seeder must have a public IP + open
  port. Hosts behind NAT can download but won't serve outside peers — there is
  no hole-punching. Real swarms rely on a handful of public institutional
  seeders; everyone else benefits.
- **The origin must stay up.** Peers are caches; if all peers leave, only the
  origin still has the data. Keep at least one always-on seeder with the full
  catalog.
- **Updating genomes:** add to the catalog/store and the running node serves
  them on its next announce (or `POST /actions/manifest` via the control plane).
- **Cost:** Oracle Always-Free covers the VM + 10 TB/mo egress at $0; peer
  offload keeps origin egress low. Effectively free at this scale.
- **Alternative (no VM at all):** if you don't need live P2P, `genomehub mirror`
  + free-egress object storage (R2/B2) hosts the same content statically — see
  the Near-zero-cost hosting section in the README.

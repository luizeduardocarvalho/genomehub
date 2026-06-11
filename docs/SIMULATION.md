# Server/Client Simulation

Slice 1 of the distribution layer: separate the **origin/serving** role from the
**downloading client** role using containers, over a real network boundary. This
is not the P2P network — it is the foundation it grows from. A peer, later, is
just a client container that also runs `serve`.

## Roles

Same binary, role = subcommand:

- `genomehub serve` — a node. Read-only HTTP over its local segment store plus a
  catalog of manifests/deltas. Origin is "the node that has everything".
- `genomehub download` — a client. Fetches a genome, pulling only segments it does
  not already have, re-hashing each on arrival (so the source is untrusted).

### Endpoints (`internal/httpapi`)

```
GET /segments/{hash}              raw content-addressed segment bytes
GET /genomes/{assembly}/manifest  manifest JSON
GET /deltas/{assembly}            delta blob
GET /catalog                      what this node can serve
GET /healthz
```

Transport is plain HTTP/1.1 for now (curl-friendly, zero-dependency). The routes
are transport-agnostic, so HTTP/2 (h2c) or QUIC is a later swap behind the same
paths.

## Run it

```bash
scripts/seed.sh            # host-side: import TAIR10, encode Ler0 as a delta → ./sim
docker compose up --build  # origin + client-cold + client-warm
```

`scripts/seed.sh` runs on the host (it needs `minimap2` to build the delta); the
containers only serve/download and need no aligner. It populates:

- `sim/origin-store/` — TAIR10 reference segments (badger)
- `sim/catalog/` — `TAIR10.manifest.json`, `Ler0.delta.ghd`

## What it demonstrates

`Ler0` is stored as a **delta against TAIR10**, not as its own segments.

- **client-cold** (empty store) downloads `Ler0`: must first pull the TAIR10
  reference segments, then the delta → **~130 MB** over the wire.
- **client-warm** downloads `TAIR10` (~114 MB) and then `Ler0`: the reference is
  already local, so `Ler0` costs only the **~16 MB delta** — 128 segments skipped.

A warm peer pays ~16 MB for a 130 MB genome it has a relative of. That is the
bandwidth thesis, measured across the container network rather than asserted.

## Multi-node swarm (built)

`docker-compose.swarm.yml` runs a real swarm: a stateless tracker, an origin
node, and two peer nodes (each filled by a one-shot init container, then serving).
See the header of that file for usage — including the `--no-deps` requirement when
running the client against an already-up swarm.

Watch it from the host:

```bash
genomehub top    --tracker http://localhost:9000      # the swarm
genomehub status --server  http://localhost:8080      # one node
genomehub jobs   --coordinator http://localhost:9100 --watch   # MEM-finding queue
```

`scripts/swarm-up.sh` / `swarm-down.sh` bring an equivalent swarm up/down locally
(no Docker) for the same TUIs.

# ADR 0003 — Node session lifecycle, the tracker, and the contribute daemon

- Status: Accepted
- Date: 2026-06-11
- Related: [ADR 0002](0002-content-addressed-blobs-and-trust.md)

## Context

The distribution layer needs participants that are *up and reachable* to seed
blobs and run MEM-finding work. So far GenomeHub has a one-shot `serve` (a
long-lived HTTP server) and a one-shot `download` (fetch and exit). What is
missing is the **participation model**: how a peer joins, advertises what it has
and can do, stays known to the network while running, picks up work, and leaves
cleanly.

The mental model is a torrent client: you launch it, it stays up, it seeds and
contributes until you close it. That "session" is the unit of participation.

## Decision

### 1. A node is one long-lived process: `genomehub node`

```
genomehub node --tracker http://tracker:9000 --serve :8080 \
  --max-cpu 50% --max-bandwidth 10MB/s --jobs on
```

On launch it:

1. Opens its store and starts the `serve` HTTP surface (ADR 0002 §1) — it can now
   seed any blob it holds.
2. **Announces to the tracker**: node id, address, capabilities (cpu budget,
   has-minimap2, bandwidth cap).
3. **Announces content**: the set of hashes it holds (see §3 for how this scales).
4. Enters a loop until interrupted:
   - **Heartbeat** to the tracker every ~30s.
   - **Poll the job queue**: claim a MEM tile (ADR 0002 §5), fetch the sequence
     blobs, run minimap2, submit MEMs, repeat — subject to `--max-cpu`.
   - **Serve** incoming segment requests concurrently.

On shutdown (SIGTERM / Ctrl-C) it deregisters, **releases any claimed jobs back to
pending**, and releases the store lock. Graceful exit is required so the tracker
and queue notice promptly and so badger does not leave a stale directory lock
(the same stale-lock failure mode seen when a `reindex` is hard-killed).

`serve` remains as the minimal always-on subset (no tracker, no jobs); `node` is
`serve` plus participation.

### 2. Two deployment modes, one binary

- **Interactive session** — a researcher opts in ("open the app to help"), runs
  `genomehub node` in a terminal, seeds + computes until they close it. Donation is
  explicit; resource caps (`--max-cpu`, `--schedule "weekdays 22:00-06:00"`) let
  them bound the cost. This is the incentive entry point (earns contribution
  reputation, ADR 0002 §2).
- **Daemon / service** — an institutional always-on seeder (a lab server, an
  Embrapa node) runs the same command under systemd or a container. The Docker
  `serve` container today is this mode minus tracker registration and the job loop.

### 3. The tracker is a stateless `hash -> [nodes]` + liveness map

The tracker knows *nothing* about genome structure. It maintains:

- **Liveness**: node id -> {address, capabilities, last_heartbeat}. A node is
  *online* while heartbeats arrive; after a timeout (~2 min) it is dropped from
  peer lists and its in-flight jobs are released to pending. Because no node is
  ever the sole holder of anything (origin is the archive), a peer vanishing costs
  only speed, never data.
- **Content index**: hash -> [nodes that hold it], used to answer "who has
  `blake3:...`". A client asks the tracker, fetches from the fastest peer, and
  falls back to origin. The `GET /segments/{hash}` endpoint is identical whether
  the responder is origin or peer — content addressing is what makes them
  interchangeable.

**Scale note:** announcing every held hash is heavy once a node holds millions.
v1 may be naive (explicit lists); the intended path is a compact summary (a bloom
filter of held hashes) announced periodically, or reactive per-hash lookups. The
tracker is centralized in v1 (it is lightweight and stateless); DHT
decentralization is explicitly deferred.

**Reachability note:** peers behind NAT cannot accept inbound connections (the
classic torrent problem). Target users (labs, institutions) mostly have public IPs
or can port-forward, so v1 assumes reachable nodes with origin as fallback;
hole-punching / relay is deferred.

### 4. Result propagation rides existing mechanisms

Keeping the network current — distributing newly discovered MEMs and new graph
versions — is not new machinery:

- **Graph-version fanout (SSE, already in the README):** the server publishes
  version N+1; subscribed nodes are notified and fetch the diff (added/removed
  segment hashes). One diff, broadcast to all.
- **Swarm growth:** every node that downloads a blob then *also seeds* it, so
  popular genomes gain sources over time — the network gets faster as it is used,
  realising the project's "improves over time" goal literally.

### 5. Observability is a status feed with two front-ends

The node exposes `GET /status` (assemblies served, segments held, bytes served,
request count, uptime, claimed jobs, resource usage). Two views consume it, both
optional, both shipping as part of the single binary:

- **Local operator view** — a k9s-style TUI bound to the node's own session:
  what *this* node is doing (seeding, claimed MEM tiles, peers, transfer rates,
  store, caps).
- **Network view** — a TUI (`genomehub top`) or web page bound to the **tracker**:
  every node's liveness, role, transfers, jobs, reputation, and system-wide saved
  %. Like k9s pointing at a cluster.

The dashboards can only show what nodes report, so observability completeness
tracks the system's: build `GET /status` first, grow the views as the tracker and
job queue land.

## Consequences

- New command `node` (daemon) layered on the existing `serve`; new lightweight
  `tracker` service; new `top`/TUI status front-ends. `download` gains a tracker
  lookup step (peer first, origin fallback).
- The job queue (claim / heartbeat / reassign) from ADR 0002 §5 is owned by the
  tracker or a sibling service; node liveness and job liveness share the heartbeat.
- Resource caps (`--max-cpu`, `--max-bandwidth`) are real controls, not cosmetic:
  a serving/computing node must be able to bound its footprint, and a node exposing
  a port must resist DoS (content addressing prevents bad-data injection but not
  resource exhaustion).
- Graceful shutdown is a correctness requirement (job release + store-lock
  release), not just politeness.

## Alternatives considered

- **Keep `serve`/`download` as the only roles (no daemon).** Rejected: one-shot
  clients cannot participate (seed, compute, be discovered) or be observed live;
  the network needs long-lived, announced participants.
- **Decentralized discovery (DHT) from the start.** Rejected as premature: the
  tracker is stateless and trivial; a DHT adds large complexity before there is a
  network big enough to need it.
- **Solve NAT traversal now.** Rejected: institutional users mostly do not need it;
  origin fallback covers v1. Revisit when consumer-grade peers matter.
- **Web-only dashboard.** Rejected as the *only* option: a node is a terminal
  process, so a TUI is the natural local view; the network view can be a TUI too
  (`genomehub top`). Web remains optional, not required.

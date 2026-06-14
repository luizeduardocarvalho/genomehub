# ADR 0004 — P2P transport: HTTP-pull now, libp2p when scale demands it

Status: Accepted (deferred build) — 2026-06-14

## Context

GenomeHub's distribution layer is HTTP request/response: a client fetches a
segment with `GET /segments/{hash}` from a node's advertised URL, and the
tracker is a central `hash → [reachable nodes]` index. This works and is live.

It has one structural limit: **a seeder must be a reachable HTTP server.** A peer
behind NAT (a home/office laptop) can download but cannot be fetched *from* —
there is no reverse channel over an HTTP pull. So in the current design, only
publicly reachable hosts can seed; most end-user machines are consumers only.

The desired end state ("works like BitTorrent — everyone who downloads also
seeds, from anywhere") is not achievable on HTTP pull. BitTorrent achieves it
because peers hold **bidirectional connections that the NAT'd peer initiates**,
plus NAT traversal (hole-punching) and relay fallback for un-punchable peers.

## Decision

**Keep HTTP-pull for now. Defer a true P2P transport (libp2p) until real usage
demonstrates the need.** Record the migration plan here so the decision is made,
not the code.

### Why defer

- **No users yet.** Frictionless NAT-seeding is a scale property (many peers
  straining one origin's bandwidth). At launch — a few labs — the origin plus a
  couple of public institutional seeders carry the load trivially, with code that
  already exists and works.
- **The cost goal is already met without seeding.** Serving the static,
  content-addressed read API from free-egress object storage (Cloudflare R2 /
  Backblaze B2) via `genomehub mirror` costs ~$0 in egress. The swarm's main
  payoff — offloading origin egress — is moot when egress is already free. So
  "every user must seed" is not load-bearing for the mission today.
- **libp2p is a heavy, permanent maintenance burden** (large dependency, new
  failure modes, NAT/relay debugging) — not worth taking on speculatively for a
  small, non-profit, lightly-staffed project.

### What the migration looks like (when triggered)

The **content model is unchanged** — content-addressed BLAKE3 segments,
manifests, delta encoding, signing, verification all stay. Only the
exchange/transport changes:

| Today (HTTP) | Target (libp2p) |
|---|---|
| `GET /segments/{hash}` (needs inbound) | bidirectional streams over a dialed conn |
| central tracker (`hash → nodes`) | Kademlia **DHT** for content routing |
| reachable seeders only | **AutoNAT + DCUtR hole-punching + circuit-relay v2** → NAT'd peers seed |
| URL node id | **peer ID = ed25519 public key** (already implemented as node identity) |
| manual peer fetch | **bitswap-style** want-have/want-block exchange |

libp2p is the natural fit: peer IDs are public keys (maps directly onto our
existing `--identity` ed25519 keys), it is content-addressing-native, and it
ships hole-punching + relays. WebRTC data channels are the alternative, relevant
mainly if browser-resident peers become a goal.

### Phasing (when we build it)

1. libp2p host + identity = existing ed25519 key; dial/stream plumbing.
2. Segment exchange protocol (bitswap-lite) over streams; reuse store + verify.
3. DHT content routing; keep the tracker as a bootstrap/rendezvous fallback.
4. AutoNAT + relay + hole-punching so NAT'd peers seed.
5. Deprecate HTTP-pull for peer↔peer (optionally keep it for the static
   mirror / curl-friendly reads).

### Trigger to revisit

Build libp2p when **either**:

- origin + institutional seeders can no longer absorb demand (real bandwidth
  pressure, measured), **or**
- users explicitly need to seed from non-reachable machines and the institutional
  -seeder model is insufficient.

## Consequences

- Until then: **consumers download from anywhere** (works on all platforms);
  **seeders are reachable hosts** (origin, partner labs, or tunneled machines).
  This matches scientific reality (institutions have public servers; students
  download).
- The cheapest, most robust deployment is the **R2 static mirror** as the
  always-available backbone, with the live HTTP swarm as an optional bonus —
  neither requires the libp2p work.
- A smaller related UX gap (noted, not yet addressed): the `dash` TUI browses and
  drives nodes but cannot save a FASTA to the local machine; getting a file
  locally is the `download` command. A "download-to-local-file" dash action would
  close it independently of transport.

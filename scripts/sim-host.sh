#!/usr/bin/env bash
# Host-only multi-genome swarm for exploring the TUI — no Docker, no minimap2.
# Generates synthetic genomes with engineered overlap, imports them into an
# origin, and launches a tracker + origin + two peers as background host
# processes, each with its own store/catalog so coverage differs per node:
#
#   origin  — holds everything (full seed: ATHENA, ATHENA2, ECOLI, HSAP)
#   peer1   — downloaded ATHENA; carries ATHENA2's manifest → ATHENA 100%,
#             ATHENA2 ~60% PARTIAL (shares chr1..3 via content addressing)
#   peer2   — downloaded ECOLI (unrelated); carries ATHENA's manifest →
#             ECOLI 100%, ATHENA 0% (knows the genome, holds none of it)
#
# Usage:
#   scripts/sim-host.sh up       # build, seed, launch; prints dash commands
#   scripts/sim-host.sh down     # stop everything
#   scripts/sim-host.sh fetch    # client downloads ATHENA2 from the swarm
set -euo pipefail
cd "$(dirname "$0")/.."

ROOT=sim/host
TRACKER=http://localhost:19000
GEN="ATHENA ATHENA2 ECOLI HSAP"

pidfile=$ROOT/pids.txt

down() {
  if [ -f "$pidfile" ]; then
    while read -r pid; do kill "$pid" 2>/dev/null || true; done < "$pidfile"
    rm -f "$pidfile"
  fi
  pkill -f 'genomehub (node|tracker).*sim/host' 2>/dev/null || true
  echo "swarm stopped."
}

# node STORE CATALOG ADDR ADVERTISE ID [REGISTRY]
start_node() {
  local store=$1 cat=$2 addr=$3 adv=$4 id=$5 reg=${6:-}
  ./genomehub --store "$store" node --tracker "$TRACKER" \
    --addr "$addr" --advertise "$adv" --id "$id" \
    --catalog "$cat" --heartbeat 2s ${reg:+--registry "$reg"} >"$store/../node.log" 2>&1 &
  echo $! >> "$pidfile"
}

up() {
  echo "building binary..."
  go build -o genomehub .

  echo "generating synthetic genomes..."
  python3 scripts/gen-genomes.py "$ROOT/fasta"

  rm -rf "$ROOT/origin" "$ROOT/peer1" "$ROOT/peer2"
  mkdir -p "$ROOT/origin/store" "$ROOT/origin/catalog" \
           "$ROOT/peer1/store" "$ROOT/peer1/catalog" \
           "$ROOT/peer2/store" "$ROOT/peer2/catalog"
  : > "$pidfile"

  echo "importing all genomes into the origin..."
  for g in $GEN; do
    org="$g"
    case $g in
      ATHENA|ATHENA2) org="Athena ficticia";;
      ECOLI) org="Escherichia coli";;
      HSAP) org="Homo sapiens";;
    esac
    ./genomehub --store "$ROOT/origin/store" import \
      --fasta "$ROOT/fasta/$g.fa" --organism "$org" --assembly "$g" \
      --output "$ROOT/origin/catalog/$g.manifest.json" >/dev/null
  done

  echo "starting tracker + origin..."
  ./genomehub tracker --addr :19000 >"$ROOT/tracker.log" 2>&1 &
  echo $! >> "$pidfile"
  sleep 1
  start_node "$ROOT/origin/store" "$ROOT/origin/catalog" :18080 http://localhost:18080 origin
  sleep 1

  echo "peer1: download ATHENA, then carry ATHENA2's manifest (→ partial)..."
  ./genomehub --store "$ROOT/peer1/store" download --server http://localhost:18080 \
    --assembly ATHENA --output /dev/null >/dev/null 2>&1 || true
  cp "$ROOT/origin/catalog/ATHENA2.manifest.json" "$ROOT/peer1/catalog/"
  start_node "$ROOT/peer1/store" "$ROOT/peer1/catalog" :18081 http://localhost:18081 peer1 http://localhost:18080

  echo "peer2: download ECOLI (unrelated), then carry ATHENA's manifest (→ 0%)..."
  ./genomehub --store "$ROOT/peer2/store" download --server http://localhost:18080 \
    --assembly ECOLI --output /dev/null >/dev/null 2>&1 || true
  cp "$ROOT/origin/catalog/ATHENA.manifest.json" "$ROOT/peer2/catalog/"
  start_node "$ROOT/peer2/store" "$ROOT/peer2/catalog" :18082 http://localhost:18082 peer2 http://localhost:18080

  sleep 1
  cat <<EOF

swarm up. open a dash per node (each in its own terminal):

  ./genomehub dash --server http://localhost:18080 --tracker $TRACKER --id origin
  ./genomehub dash --server http://localhost:18081 --tracker $TRACKER --id peer1
  ./genomehub dash --server http://localhost:18082 --tracker $TRACKER --id peer2

what you should see:
  Seeding tab (2):
    origin  ATHENA/ATHENA2/ECOLI/HSAP all 100% (full seed)
    peer1   ATHENA 100% (full), ATHENA2 ~60% (partial cache, amber)
    peer2   ECOLI 100% (full), ATHENA 0% (empty, red)
  Discover tab (3) — every genome in the network + how much YOU hold:
    peer1   ATHENA 100%, ATHENA2 60%, ECOLI 0%, HSAP 0%  (learns it half-holds
            ATHENA2 and that ECOLI/HSAP exist — none of which it imported)

drive an interaction (peer1 serves the shared chunks):
  scripts/sim-host.sh fetch     # a client pulls ATHENA2 from the swarm

stop:
  scripts/sim-host.sh down
EOF
}

fetch() {
  echo "client downloading ATHENA2 from the swarm (shared chunks come from peer1)..."
  rm -rf "$ROOT/client-store"
  ./genomehub --store "$ROOT/client-store" download --tracker "$TRACKER" \
    --server http://localhost:18080 --assembly ATHENA2 --output /dev/null --parallel 8
}

case "${1:-up}" in
  up) up;;
  down) down;;
  fetch) fetch;;
  *) echo "usage: $0 {up|down|fetch}"; exit 1;;
esac

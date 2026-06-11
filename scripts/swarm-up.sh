#!/usr/bin/env bash
# Brings up a local swarm for watching with `genomehub top` / `genomehub status`:
#   - tracker on :9000
#   - origin node on :8101 (holds TAIR10 + the Ler0 delta, from scripts/seed.sh)
#   - two peer nodes on :8102 / :8103, each pre-filled with TAIR10 by downloading
#     from the swarm, so all three are TAIR10 seeders.
# Each store is owned by exactly one process (badger locks), so peers are filled
# while down, then brought online. Logs in sim/logs/. Stop with scripts/swarm-down.sh.
set -e
cd "$(dirname "$0")/.."

[ -d sim/origin-store ] && [ -d sim/catalog ] || { echo "run scripts/seed.sh first"; exit 1; }

go build -o genomehub .
mkdir -p sim/logs sim/empty-catalog
rm -f sim/swarm.pids
rm -rf sim/peer1-store sim/peer2-store
TR=http://localhost:9000

echo "starting tracker + origin..."
./genomehub tracker --addr :9000 >sim/logs/tracker.log 2>&1 & echo $! >>sim/swarm.pids
./genomehub --store sim/origin-store node --tracker $TR --addr :8101 \
  --advertise http://localhost:8101 --catalog sim/catalog --heartbeat 2s >sim/logs/origin.log 2>&1 & echo $! >>sim/swarm.pids
sleep 2

echo "pre-filling peer1 and peer2 with TAIR10 (downloaded from the swarm)..."
./genomehub --store sim/peer1-store download --tracker $TR --server http://localhost:8101 \
  --assembly TAIR10 --output sim/_p1.fa >sim/logs/fill1.log 2>&1
./genomehub --store sim/peer2-store download --tracker $TR --server http://localhost:8101 \
  --assembly TAIR10 --output sim/_p2.fa >sim/logs/fill2.log 2>&1
rm -f sim/_p1.fa sim/_p2.fa

echo "bringing peer1 + peer2 online..."
./genomehub --store sim/peer1-store node --tracker $TR --addr :8102 \
  --advertise http://localhost:8102 --catalog sim/empty-catalog --heartbeat 2s >sim/logs/peer1.log 2>&1 & echo $! >>sim/swarm.pids
./genomehub --store sim/peer2-store node --tracker $TR --addr :8103 \
  --advertise http://localhost:8103 --catalog sim/empty-catalog --heartbeat 2s >sim/logs/peer2.log 2>&1 & echo $! >>sim/swarm.pids
sleep 2

cat <<'MSG'

swarm up:
  tracker   http://localhost:9000
  origin    http://localhost:8101   (TAIR10 manifest + Ler0 delta)
  peer1     http://localhost:8102
  peer2     http://localhost:8103

watch (each in its own terminal):
  ./genomehub top    --tracker http://localhost:9000 --interval 1s
  ./genomehub status --server  http://localhost:8101 --interval 1s
  ./genomehub status --server  http://localhost:8102 --interval 1s
  ./genomehub status --server  http://localhost:8103 --interval 1s

try a download from the swarm (new client, peer-served):
  ./genomehub --store sim/client-store download \
    --tracker http://localhost:9000 --server http://localhost:8101 \
    --assembly Ler0 --output sim/Ler0.fa

stop: scripts/swarm-down.sh
MSG

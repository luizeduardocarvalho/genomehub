#!/usr/bin/env bash
# Stops the swarm started by scripts/swarm-up.sh. Sends SIGINT first so nodes
# deregister from the tracker cleanly (graceful leave), then reaps anything left.
cd "$(dirname "$0")/.."

if [ -f sim/swarm.pids ]; then
  while read -r p; do kill -INT "$p" 2>/dev/null; done <sim/swarm.pids
  sleep 2
  while read -r p; do kill "$p" 2>/dev/null; done <sim/swarm.pids
  rm -f sim/swarm.pids
fi
# belt and suspenders
pkill -f "genomehub tracker" 2>/dev/null || true
pkill -f "genomehub.*node" 2>/dev/null || true
echo "swarm down"

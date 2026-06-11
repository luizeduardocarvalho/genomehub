#!/usr/bin/env bash
# Host-side data prep for the docker-compose simulation. Imports a reference
# genome into an origin store and encodes a second genome as a delta against it,
# laying out ./sim/origin-store and ./sim/catalog for the `origin` container to
# serve. Runs on the host (needs minimap2); the containers themselves only
# serve/download and need no aligner.
set -euo pipefail
cd "$(dirname "$0")/.."

TAIR=${TAIR:-tmp/Arabidopsis_thaliana.TAIR10.dna.toplevel.fa}
LER=${LER:-tmp/ler0_hifi.fna}
PRESET=${PRESET:-asm5}

for f in "$TAIR" "$LER"; do
  [ -f "$f" ] || { echo "missing FASTA: $f (set TAIR/LER env vars)"; exit 1; }
done

echo "building binary..."
go build -o genomehub .

rm -rf sim/origin-store sim/catalog
mkdir -p sim/origin-store sim/catalog

echo "importing TAIR10 (reference) into origin store..."
./genomehub --store sim/origin-store import \
  --fasta "$TAIR" --organism "Arabidopsis thaliana" --assembly TAIR10 \
  --output sim/catalog/TAIR10.manifest.json

echo "encoding Ler0 as a delta against TAIR10 (preset $PRESET)..."
./genomehub --store sim/origin-store delta \
  --reference TAIR10:"$TAIR" --query Ler0:"$LER" --preset "$PRESET" \
  --reference-manifest TAIR10.manifest.json \
  --output sim/catalog/Ler0.delta.ghd

echo "publishing the Ler0 delta as content-addressed chunks (so it swarms)..."
./genomehub --store sim/origin-store delta-publish \
  --delta sim/catalog/Ler0.delta.ghd \
  --output sim/catalog/Ler0.deltarecipe.json

echo
echo "seeded:"
echo "  sim/origin-store  (TAIR10 segments)"
echo "  sim/catalog       ($(ls sim/catalog | tr '\n' ' '))"
echo
echo "next: docker compose up --build"

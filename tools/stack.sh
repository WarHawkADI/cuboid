#!/usr/bin/env bash
# Development path without Docker: run every service from bin/, against
# tools/devdb.sh. Usage: tools/stack.sh up|down
cd "$(dirname "$0")/.." || exit 1
export CUBOID_DEMO_MODE=true
case "${1:-up}" in
up)
  mkdir -p data
  (nohup ./bin/supervisor -- ./bin/cuboid > data/cuboid.log 2>&1 &)
  (nohup ./bin/storesim > data/storesim.log 2>&1 &)
  (nohup ./bin/naive > data/naive.log 2>&1 &)
  (nohup ./bin/loadgen > data/loadgen.log 2>&1 &)
  ;;
down)
  for p in supervisor cuboid storesim naive loadgen; do
    taskkill //F //IM "$p.exe" >/dev/null 2>&1 || pkill -x "$p" 2>/dev/null
  done
  ;;
up-cuboid)
  (nohup ./bin/supervisor -- ./bin/cuboid >> data/cuboid.log 2>&1 &)
  ;;
down-cuboid)
  for p in supervisor cuboid; do
    taskkill //F //IM "$p.exe" >/dev/null 2>&1 || pkill -x "$p" 2>/dev/null
  done
  ;;
*) echo "usage: $0 up|down|up-cuboid|down-cuboid"; exit 2 ;;
esac

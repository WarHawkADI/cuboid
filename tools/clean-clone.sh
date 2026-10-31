#!/usr/bin/env bash
# Proves the repository works from a fresh clone, with nothing from this working
# copy. Usage: tools/clean-clone.sh native|docker
#   native  builds and tests with Go and Node
#   docker  runs the README's quick start. Stop any stack on ports 8080-8083 and
#           5433-5434 first. Its containers and volumes are removed afterwards.
set -eu
mode="${1:-native}"
root="$(cd "$(dirname "$0")/.." && pwd)"
dir="$(mktemp -d)"
trap 'rm -rf "$dir"' EXIT
git clone --quiet "$root" "$dir/cuboid"
cd "$dir/cuboid"

case "$mode" in
native)
  go build ./...
  go vet ./...
  go test ./...
  npm --prefix web ci --no-audit --no-fund
  npm --prefix web run build
  ;;
docker)
  export COMPOSE_PROJECT_NAME=cuboidcheck
  cleanup() { docker compose --profile seed down -v --remove-orphans; rm -rf "$dir"; }
  trap cleanup EXIT
  docker compose up -d --build
  SEED_TIER=dev docker compose --profile seed run --rm seed
  for _ in $(seq 1 180); do
    if curl -fsS http://localhost:8080/healthz | grep -q '"streaming"'; then break; fi
    sleep 2
  done
  curl -fsS http://localhost:8080/healthz | grep -q '"streaming"'
  curl -fsS -o /dev/null http://localhost:8080/monitor
  docker compose exec -T cuboid cuboid keys | head -5
  go run ./cmd/bench demo-check
  ;;
*)
  echo "usage: $0 native|docker" >&2
  exit 2
  ;;
esac
echo "clean clone check ($mode): passed"

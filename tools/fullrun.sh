#!/usr/bin/env bash
# The whole scorecard from one build, on the native stack: speed on the small
# tier, then a fresh main tier and all five targets. Output goes to data/run.log.
cd "$(dirname "$0")/.." || exit 1
export CUBOID_DEMO_MODE=true
export BENCH_CUBOID_STOP='taskkill /F /IM supervisor.exe & taskkill /F /IM cuboid.exe'
export BENCH_CUBOID_START='start /b bin\supervisor.exe -- bin\cuboid.exe >> data\cuboid.log 2>&1'

ready() {
  sleep 15
  until curl -s -m 3 "localhost:8080/api/monitor/state?since=9999999999999" | python -c "import sys,json;d=json.load(sys.stdin);sys.exit(0 if d['state']=='streaming' and not d['rebuild']['running'] and 0<=d['freshness']['lag_ms']<3000 else 1)" 2>/dev/null; do
    sleep 5
  done
  sleep 30
}

{
  echo "== $(date +%H:%M) seed small"
  ./bin/seed --tier small | tail -2
  ready
  echo "== $(date +%H:%M) speed on the small tier"
  ./bin/bench speed
  echo "== $(date +%H:%M) seed main"
  ./bin/seed --tier main | tail -2
  ready
  echo "== $(date +%H:%M) all five targets on the main tier"
  ./bin/bench targets -phase 120s
  code=$?
  echo "== $(date +%H:%M) finished"
  echo "exit $code"
} > data/run.log 2>&1

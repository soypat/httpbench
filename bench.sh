#!/usr/bin/env bash
# Measures every implementation and draws every figure.
#
#   ./bench.sh                      # full matrix, then the figures
#   ./bench.sh -requests=2000       # anything here is passed to `httpbench run`
#   REQUESTS=2000 ./bench.sh        # same, for the one flag worth an env var
#   PLOT_DIR=plots/v2 ./bench.sh    # keep a run's figures next to the last one's
#
# The two steps are one script because they are one result: figures drawn from a
# results.json that a later run has already overwritten are figures of nothing.
set -euo pipefail

cd "$(dirname "$0")"

JSON=${JSON:-results.json}
REPORT=${REPORT:-REPORT.md}
PLOT_DIR=${PLOT_DIR:-plots}
BIN_DIR=${BIN_DIR:-bin}

# Passed through as -requests=N when set, so the common case needs no flag
# syntax. Anything else goes in as an argument.
run_args=(-json="$JSON" -o="$REPORT" -bin="$BIN_DIR")
if [[ -n ${REQUESTS:-} ]]; then
	run_args+=(-requests="$REQUESTS")
fi
run_args+=("$@")

# A red gate is not a benchmark. Everything below measures servers built from
# this tree, so the tree has to compile and pass vet first.
echo "== vet"
go vet ./...

echo "== measure"
# Each implementation is rebuilt, started, sampled and killed by the
# orchestrator; nothing else should be competing for the machine while it runs.
go run ./cmd/httpbench run "${run_args[@]}"

echo "== plot"
go run ./cmd/httpbenchplot -json="$JSON" -dir="$PLOT_DIR"

echo
echo "measurements: $JSON"
echo "report:       $REPORT"
echo "figures:      $PLOT_DIR/"

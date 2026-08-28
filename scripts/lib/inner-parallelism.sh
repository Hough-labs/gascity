#!/usr/bin/env bash
# inner-parallelism.sh — computes GOFLAGS=-p=<n> for the go-test binaries
# launched by each outer test-local-parallel job (ga-04m84s).
#
# The outer job count (test-local-job-count) sizes concurrent shard
# processes; each shard's `go test` binary defaults its internal -p to
# GOMAXPROCS, so when multiple shards run concurrently they each
# independently try to claim the whole machine, oversubscribing it.
# gc_inner_parallelism divides the outer budget across however many
# shards are actually running concurrently so each one's -p is capped to
# its fair share instead.
#
# Scope: -p only bounds cross-package build/test-binary concurrency, not
# within-package t.Parallel() fan-out (that's the separate -parallel flag,
# also defaulting to GOMAXPROCS). Shards that invoke go test against a single
# package -- most of cmd/gc's job list -- get -p bounded only for their
# dependency-build phase, not their t.Parallel() run phase; the multi-package
# jobs get the full benefit.
#
# gc_gate_parallelism below bounds that second dimension for the `make test`
# and `make test-mac` gate lanes (gascity-ngab). test-local-parallel still
# leaves -parallel at GOMAXPROCS.
#
# Source this file in other scripts:
#   source "$repo_root/scripts/lib/inner-parallelism.sh"

# gc_inner_parallelism LOCAL_JOBS JOB_COUNT prints the -p value each
# concurrent job should pass to `go test`. GC_TEST_INNER_P overrides the
# computation outright (must be a positive integer) for deterministic tests.
gc_inner_parallelism() {
  local local_jobs="$1" job_count="$2"

  if [[ -n "${GC_TEST_INNER_P:-}" ]]; then
    [[ "$GC_TEST_INNER_P" =~ ^[0-9]+$ && "$GC_TEST_INNER_P" -gt 0 ]] ||
      { echo "GC_TEST_INNER_P must be a positive integer" >&2; return 1; }
    printf '%s\n' "$GC_TEST_INNER_P"
    return
  fi

  local effective_outer="$job_count"
  if (( local_jobs < effective_outer )); then
    effective_outer="$local_jobs"
  fi
  local inner_p=$(( local_jobs / effective_outer ))
  if (( inner_p < 1 )); then
    inner_p=1
  fi
  printf '%s\n' "$inner_p"
}

# gc_gate_parallelism CPU_BUDGET OUTER_P prints the -parallel value each gate
# test binary should carry. A `go test` binary defaults -parallel to
# GOMAXPROCS, so the OUTER_P binaries -p allows to run at once each fan their
# t.Parallel() tests out across every core independently: peak concurrency is
# OUTER_P x GOMAXPROCS, four times the core count for the gate's -p=4 on a
# 16-core host. Dividing the budget makes the two dimensions multiply out to
# roughly the core count instead.
#
# The result is floored at 4 so a small CI runner -- where OUTER_P already
# meets or exceeds the core count -- keeps the behaviour it has today rather
# than being serialized to one test at a time. GC_TEST_GATE_PARALLEL
# overrides the computation outright (must be a positive integer).
#
# This deliberately does not touch GOMAXPROCS. Lowering that would also thin
# the runtime's scheduler, which changes goroutine interleaving and weakens
# the race detector; -parallel bounds only how many top-level tests run at
# once and leaves execution semantics alone.
gc_gate_parallelism() {
  local cpu_budget="$1" outer_p="$2" floor=4

  if [[ -n "${GC_TEST_GATE_PARALLEL:-}" ]]; then
    [[ "$GC_TEST_GATE_PARALLEL" =~ ^[0-9]+$ && "$GC_TEST_GATE_PARALLEL" -gt 0 ]] ||
      { echo "GC_TEST_GATE_PARALLEL must be a positive integer" >&2; return 1; }
    printf '%s\n' "$GC_TEST_GATE_PARALLEL"
    return
  fi

  [[ "$cpu_budget" =~ ^[0-9]+$ && "$cpu_budget" -gt 0 ]] ||
    { echo "gc_gate_parallelism: cpu budget must be a positive integer" >&2; return 1; }
  [[ "$outer_p" =~ ^[0-9]+$ && "$outer_p" -gt 0 ]] ||
    { echo "gc_gate_parallelism: outer -p must be a positive integer" >&2; return 1; }

  local parallel=$(( cpu_budget / outer_p ))
  if (( parallel < floor )); then
    parallel="$floor"
  fi
  printf '%s\n' "$parallel"
}

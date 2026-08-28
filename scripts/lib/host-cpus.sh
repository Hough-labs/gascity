#!/usr/bin/env bash
# host-cpus.sh — how many CPUs this host offers a test run.
#
# Extracted from scripts/test-local-job-count so the gate lanes
# (scripts/test-gate-parallelism) size themselves against the same number,
# and so GC_TEST_LOCAL_CPUS overrides both for deterministic tests.
#
# Source this file in other scripts:
#   source "$repo_root/scripts/lib/host-cpus.sh"

# gc_host_cpus prints the CPU count. GC_TEST_LOCAL_CPUS overrides the
# detection outright (must be a positive integer). Detection is best-effort
# and falls back to 8 rather than failing, because every caller is sizing a
# budget rather than making a correctness decision.
gc_host_cpus() {
  if [[ -n "${GC_TEST_LOCAL_CPUS:-}" ]]; then
    [[ "$GC_TEST_LOCAL_CPUS" =~ ^[0-9]+$ && "$GC_TEST_LOCAL_CPUS" -gt 0 ]] ||
      { echo "GC_TEST_LOCAL_CPUS must be a positive integer" >&2; return 1; }
    printf '%s\n' "$GC_TEST_LOCAL_CPUS"
    return
  fi

  nproc 2>/dev/null ||
    getconf _NPROCESSORS_ONLN 2>/dev/null ||
    sysctl -n hw.ncpu 2>/dev/null ||
    printf '8\n'
}

---
title: Darwin /proc fallback audit
description: Per-call-site verdicts on whether Gas City's procfs fallbacks are actually exercised and correct on macOS.
---

# Darwin `/proc` fallback audit (gascity-t8r1, gc-zxxy facet 2)

**Status:** complete. One defect found and fixed; three residual gaps filed, of
which gascity-232 has since closed one (the `managedDoltDoctorProcessOwnsRuntime`
cwd arm).
**Scope:** the six fallback-bearing `/proc` call sites named in gascity-t8r1,
audited per call site rather than per file.
**Host:** `darwin/arm64`, Darwin 25.6.0 (`hammer`). `/proc` does not exist.

The premise this audit had to test is narrow and worth restating, because the
bead it came from restated it too: **"a fallback exists in the file" is not
"the fallback is exercised and correct on Darwin."** Reading a file and finding
an `lsof` branch proves neither that the branch is reached nor that its parser
matches what macOS actually prints. So every verdict below is backed by one of
two things, and the table says which:

- **live** — the real function was called on this host against real processes,
  and its return value is quoted. Where a live probe needed a subject, it used
  the two `dolt sql-server` processes running at the time (pid 2867 on port
  3307, pid 19479 on port 51160, the latter being the managed city server
  recorded in `dolt-state.json`).
- **read** — the Darwin path is a guard or an error return with no observable
  output; the verdict comes from tracing the call path to its consumer.

A methodological note, because it cost a wrong intermediate answer here:
`discoverActiveTestRoots` first appeared to find nothing on Darwin. The fault
was the probe, not the code — the synthetic subject process was
`sleep 20 <path>`, and BSD `sleep` rejects the second operand and exits
immediately, so there was never a process to find. Re-run with `tail -f
<path>` it returns the correct root. A Darwin audit that spawns subjects has to
check that its own subjects survived.

## Correction: the file count in the bead was low

gascity-t8r1 records that `git grep '"/proc'` finds 7 non-test files. On the
same tree (`origin/edge-integration`) it finds **fifteen**. The audited six
plus `doctor_fork_rate.go` account for seven of them; the remainder are:

| File | Why it was missed | Darwin status |
|---|---|---|
| `cmd/gc/bead_worktree_liveness_linux.go` | build-tagged `linux` | ported (facet 1, `df0f1ad31`) |
| `cmd/gc/fs_pressure_linux.go` | build-tagged `linux` | Linux-only by construction |
| `internal/runtime/proctable/{guard,scan_linux,descendants_linux}.go` | build-tagged, plus a comment match | **has real darwin peers** — `descendants_darwin.go`, `scan_darwin.go` |
| `internal/pidutil/pidutil.go` | not in the bead's list | `Alive` has a `ps` fallback; `StartTime`/`Cmdline` return errors that callers degrade on, documented in their doc comments |
| `internal/workspacesvc/orphan_reap.go` | not in the bead's list | **silent no-op on Darwin** — filed as gascity-si96 |
| `test/dolttest/dolttest.go` | test helper | out of scope |

`internal/runtime/proctable` is the model the rest of the tree should be read
against: a tagged `_linux.go` / `_darwin.go` / `_stub.go` split where the
Darwin file is a real implementation, not a stub returning nil.

The one that matters is `internal/workspacesvc/orphan_reap.go`, and it matters
because of what it pairs with. It is the second of two sweeps for leftover
workspace-service processes; the first
(`cmd/gc/cmd_supervisor_lifecycle.go:215`) is explicitly `GOOS`-guarded and
warns the operator on macOS. This one returns `nil` and says nothing. So on
Darwin both halves of that cleanup are off, one loudly and one silently.

## The defect: `pidGone` reported every live PID as gone

`cmd/gc/cmd_start_drift.go`, fixed in this change.

```go
// before
func pidGone(pid int) bool {
	if err := syscall.Kill(pid, syscall.Signal(0)); err == syscall.ESRCH {
		return true
	}
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "status"))
	if err != nil {
		return os.IsNotExist(err)   // <-- unconditionally true with no procfs
	}
	...
}
```

The procfs read exists to catch the zombie case that signal-zero reports as
alive. On Linux, returning `os.IsNotExist(err)` is right: a status file that
vanished between the two syscalls means the entry was reaped. On a host with
no procfs at all the read fails with `ENOENT` for **every** PID, so the
function answered "gone" for every process that was not already ESRCH.

This is the only site in the audited set that fails in the dangerous
direction. It is not a skipped check — it is a confident wrong answer feeding
a destructive decision. `waitForPIDExit` is the drift path's gate for "the old
supervisor has really exited": it returned `nil` on its first poll while the
`SIGTERM`'d supervisor was still running and still holding its control socket,
and the caller then spawned a replacement on top of it. Neither the SIGTERM
grace wait nor the SIGKILL escalation could ever run on macOS.

Reproduced on this host before the fix:

```
--- FAIL: TestPidGoneReportsLiveProcessAsAlive
    pidGone(98548) = true for the running test process; want false
--- FAIL: TestWaitForPIDExitActuallyKillsSurvivingProcess
    waitForPIDExit reported pid 99477 exited, but the process was still running
```

`pidGone` now delegates to the package-local `pidAlive`, i.e.
`pidutil.Alive`, which already encodes this probe portably — signal-zero,
then `/proc/<pid>/stat` for the zombie state, then `ps -o stat=` where there
is no procfs. Reusing it keeps one liveness definition in the tree rather than
a second procfs-only copy that drifts from it. Regression coverage is in
`cmd/gc/cmd_start_drift_pid_liveness_test.go`; the consequence test judges
liveness with `wait(2)` rather than with `pidGone`, so it does not assert the
function against itself.

The swap does move Linux behavior in one narrow window, and it moves it in the
safe direction. Both implementations probe signal-zero first and then read
procfs, so both have a race between the two syscalls; they disagree only about
what a failed read means there. The old code answered `os.IsNotExist(err)` —
"reaped, therefore gone." `pidutil.Alive` answers `!psReportsZombie(pid)`, and
`ps -p <reaped-pid>` exits non-zero, so the probe reports the PID *alive* for
that one call. The next poll 50ms later gets `ESRCH` from signal-zero and
reports it gone, so `waitForPIDExit` loses at most one poll interval. Erring
toward "still running" is the direction that costs a delay rather than a
second supervisor on a live control socket, which is the whole failure this
change exists to prevent.

## Per-call-site verdicts

### `cmd/gc/dolt_cleanup_discovery.go` — the reaper's discovery walk

| Line | Function | Darwin behavior | Evidence | Verdict |
|---|---|---|---|---|
| 59 | `discoverDoltProcesses` | `ReadDir("/proc")` errors → `discoverDoltProcessesFromPS` | live: `err=<nil> n=2`, both live dolt servers found with correct argv | ✅ |
| 616, 628, 704 | `portsByPID` | `/proc/net/tcp{,6}` unreadable → `checked=false` → `portsByPIDFromLsof` | live: full host port map returned; `2867:[3307]`, `19479:[51160]` | ✅ |
| 230, 244 | `discoverActiveTestRoots` | falls back to `discoverActiveTestRootsFromPS` | live: synthetic `tail -f <tmp>/TestZq7Probe/001/dolt-config.yaml` → 1 root, correct | ✅ |
| 193 | `doltProcCWDState` | readlink always fails → `procPathStateUnknown` | read: `procPathStateUnknown == ""`; `classifyDoltProcess` authorizes a reap only on `procPathStateDeleted`, and adjusts its reason string so an unknown cwd is never described as confirmed live (`dolt_cleanup_reaper.go:274`, `:322`) | ✅ by design |
| 334 | `readProcStartTimeTicks` | Linux arm only; PS arm sets `StartIdentity` from `lstart` instead | live: `ticks=0 ident="Wed Aug 26 14:55:23 2026"` | ✅ |
| 376 | `readProcRSSBytes` | Linux arm only; PS arm takes `rss` from `ps` | live: `rss=908656640` for pid 19479 | ✅ |
| 395 | `readDoltSQLServerArgv` | Linux arm only | read | ✅ |

The PS arm leaves `CWDState` empty and `StartTimeTicks` zero, and both are
load-bearing rather than sloppy: `procPathStateUnknown` is the empty string, so
an unset `CWDState` is already the protect-leaning value, and
`sameReapProcessIdentity` prefers ticks when non-zero and falls back to
`StartIdentity` when it is zero. The two arms are exact complements.

One thing checked and cleared, because it looked like a bug: `parseDoltPSLine`
normalizes the five `lstart` fields with `strings.Join(..., " ")`, while
`readProcStartIdentity` returns raw `ps -p <pid> -o lstart=` output. If BSD
pads the day of month with `strftime %e` — the documented `lstart` format —
those two forms differ on days 1–9. **This one is reasoned, not observed:**
every process on the host at audit time had started on the 26th, so the
single-digit form never appeared and no probe could force it. It is not a
defect either way, because the two producers never meet: every `StartIdentity`
comparison in the tree (`cmd/gc/cmd_dolt_cleanup.go:534`,
`dolt_start_managed.go:892`) compares two values from the *same* producer.
Worth knowing before anyone introduces a third.

No fixture covered the column-padded shapes the fallback actually parses —
every existing one used a two-digit day and single-space separators — so
`TestParseDoltPSLine_ColumnPaddedLstart` and
`TestArgvFromPSLine_ColumnPaddedNonDolt` were added, the first case captured
verbatim from this host's `ps`.

### `cmd/gc/dolt_process_inspection.go` — managed-server inspection

The best-prepared file in the set. Its `probeResult` type is a three-state
answer (`probeYes` / `probeNo` / `probeUnknown`) whose doc comment names Darwin
explicitly, and the contract — `probeUnknown` may never stand in for `probeNo`
in a decision that mutates state — is what keeps a timed-out `lsof` from being
read as "nothing holds this port."

| Line | Function | Darwin behavior | Evidence | Verdict |
|---|---|---|---|---|
| 519, 546, 558 | `findPortHolderPIDFromProc` | not checked → `findPortHolderPIDFromLsof` | live: `findPortHolderPID("51160") = 19479, checked=true`, matching `dolt-state.json` | ✅ |
| 356, 359 | `processHasDeletedDataInodes` | fd walk fails → `deletedDataInodeTargetsFromLsof` | live: `probeNo` for pid 2867 against gc's data dir — an answer, not `probeUnknown` | ✅ |
| 701 | `processArgsFromProc` | → `processArgsFromPS` | live: full argv returned for pid 2867 | ✅ |
| 783 | `processCWDMatches` | readlink fails → `processCWDFromLsof` | live: `processCWDFromLsof(2867) = "/Users/hunter/.local/share/bastion/dolt", probeYes`; `processCWDMatches(2867, gcDataDir) = probeNo` | ✅ |

### `internal/doctor/checks.go` — the managed-dolt doctor checks

| Line | Function | Darwin behavior | Evidence | Verdict |
|---|---|---|---|---|
| 1797, 1825, 1837 | `managedDoltDoctorPortHolderFromProc` | not checked → `...FromLsof` | live: `managedDoltDoctorPortHolderPID(51160) = 19479, checked=true`; `(1) = 0, checked=true` — unheld and unknown stay distinct | ✅ |
| 1767 | `managedDoltDoctorProcCmdline` | → `ps -p <pid> -o args=` | live: full argv for pid 19479 | ✅ |
| 1759 | `managedDoltDoctorProcessOwnsRuntime` cwd arm | → `lsof -a -p <pid> -d cwd -Fn` | live (pre-fix): verdict correct via the cmdline arm for the real config, `false` for a config the argv does not name | ✅ fixed in gascity-232 |

The cwd arm is the second of two independent ownership signals. Before
gascity-232 only the first was portable: the production shape — a server
spawned as `dolt sql-server --config <configFile>` — is answered by the cmdline
arm, so the verdict was right where it counts, but a server whose argv did not
name the configured path got a false negative on Darwin where Linux would still
prove ownership. A diagnostic false negative, not a destructive one, which is
why it was filed rather than fixed during the audit.

gascity-232 closed it by porting a doctor-local `lsof -d cwd` probe into
`internal/doctor` (`dolt_cwd_probe.go`) rather than hoisting `cmd/gc`'s
`processCWDFromLsof` into a shared package. `internal/` cannot import `cmd/gc`,
and the file already contains the precedent: `managedDoltDoctorPortHolderFromLsof`
is itself a doctor-local reimplementation of `cmd/gc`'s
`findPortHolderPIDFromLsof`. The probe returns `(cwd, probed)` per the
gascity-4k6 contract, so a timed-out or absent `lsof` stays distinguishable from
a genuinely unreadable cwd and never establishes ownership.

### `cmd/gc/cmd_supervisor_lifecycle.go` — workspace-service cleanup

| Line | Function | Darwin behavior | Evidence | Verdict |
|---|---|---|---|---|
| 295, 309, 328 | `findSupervisorWorkspaceServiceProcesses` | unreachable — caller guards on `supervisorRuntimeGOOS != "linux"` and warns, naming the state roots to clean by hand | read: `cleanupSupervisorWorkspaceServicesForSupervisorStart`, lifecycle.go:215 | ✅ honest, documented |
| 1896 | `runningSupervisorPreserveSignalReady` | Linux-only path — reached from the systemd unit install only | read: caller at lifecycle.go:1979 is inside the systemd install | ✅ n/a on Darwin |

This is the shape the rest of the tree should copy when a port is genuinely
infeasible: refuse on the unsupported platform, say which platform, and tell
the operator what to do instead. Its complement in
`internal/workspacesvc/orphan_reap.go` does not, which is gascity-si96.

### `cmd/gc/dolt_preflight_cleanup.go` — stale unix-socket removal

| Line | Function | Darwin behavior | Evidence | Verdict |
|---|---|---|---|---|
| 26 | `unixSocketInodesForPath` | `/proc/net/unix` unreadable → `checked=false` | live (via caller) | ✅ |
| 25 | `fileOpenedByAnyProcessFromProc` | `ReadDir("/proc")` errors → `checked=false` → `lsof <path>` | live: `fileOpenedByAnyProcess(open socket) = true`; after `Close()`, `= false` — both with `err=<nil>` | ✅ |

Probed against a real `net.Listen("unix", ...)` socket, open and then closed,
so both branches of the answer are covered on this host. The
`errManagedDoltOpenStateUnknown` third state exists for the case where `lsof`
is absent or times out, and callers skip the removal rather than deleting a
socket they could not prove was dead.

### `cmd/gc/cmd_start_drift.go` — supervisor drift restart

| Line | Function | Darwin behavior | Evidence | Verdict |
|---|---|---|---|---|
| 479 | `readSupervisorExePath` | readlink always fails; a launchd-managed supervisor restarts via `launchctl` without needing the path, otherwise gc refuses to auto-restart and prints a Darwin-specific remedy | read: `printUnreadableSupervisorRestartError` (drift.go:460), branch at drift.go:301 | ✅ honest, fail-closed |
| 577 | `pidGone` | **was** inverted on Darwin; now `!pidAlive(pid)` | live: red→green cycle above | ✅ fixed here |

One cosmetic residue at drift.go:392: `newExe, _ := readSupervisorExePathHook(newPID)`
drops the error deliberately, so the post-restart identity line prints an empty
executable path on macOS. Display only — not filed.

## Residual gaps

| Bead | Site | Why not fixed here |
|---|---|---|
| gascity-si96 (P2) | `internal/workspacesvc/orphan_reap.go` | outside the audited six; the honest-signal fix and the full `ps -Eww` port are different sizes and want a decision |
| ~~gascity-38vz (P2)~~ | `cmd/gc/doctor_fork_rate.go` | **Resolved** by gascity-5830 — see "D4 resolved" below |

## D4 resolved: the Darwin fork-rate instrument

The feasibility question this table left open — does a faithful proxy for the
cumulative fork counter exist on macOS at all, given that a wrong proxy is worse
than the honest skip — was answered yes, and the instrument was built.

**The proxy.** macOS allocates PIDs sequentially, so a freshly allocated PID is
a monotone counter of process creations and the delta between two of them counts
the creations in the window. Measured on `hammer` (Darwin 25.6.0 arm64) over 1s
windows through the same `exec.Command` probe the check uses: 129-333
creations/s over eight consecutive windows at load ~119, comfortably above the
check's existing 100/s warn threshold. The figure tracks load rather than
sitting flat, which is the whole point — it is a *rate*, the thing the
originating bead said was unobtainable on macOS.

**Why it must spend a fork.** Two cheaper routes were tested and both fail:

- `kern.lastpid`, the BSD sysctl that reports the last allocated PID directly,
  does not exist on Darwin (`sysctl kern.lastpid` -> `unknown oid`).
- The highest *live* PID — the obvious fork-free substitute, readable from the
  process table — is dead on a host whose PID space has already wrapped. Measured
  here it sat pinned at a constant 99440 (a long-lived process from before the
  wrap) while the allocator itself was down at ~44000 and moving, so its delta
  read a constant zero. It would have reported "no fork activity" on a host doing
  ~130 creations/s.

**What honesty required.** The proxy is a LOWER BOUND, not the kernel's count, so
the implementation reports it as one: `forkCounterKind`/`forkCounterTraits` in
`doctor_fork_rate.go` carry per-counter reporting rules, so the Darwin arm prints
"at least N forks/s ... via PID-delta proxy" and the Linux arm keeps the exact,
unhedged `/proc/stat` figure. The probe's own process creation is discounted
before reporting, and PID wraparound reuses the existing backwards-delta branch
and skips rather than reporting a negative rate. `/proc/stat` stays preferred
wherever it is readable, so Linux is unchanged and no fork is spent where a plain
read suffices.

## What this audit does not cover

- **Linux behavior.** Every verdict above is about the Darwin arm. The
  `pidGone` change is the only one that alters a Linux path — its one
  behavioral delta there is the failed-read race described above — and it is
  covered by the reaped-PID and zombie tests plus `pidutil`'s own suite.
- **The `lsof`-absent host.** Each fallback has a third state for it and the
  states were read, but no probe ran with `lsof` removed from `PATH`.
- **Windows or any non-unix host.** Out of scope for both this bead and the
  tree.

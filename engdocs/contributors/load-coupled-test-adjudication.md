# Load-coupled test cluster: adjudication against the post-cap baseline

Seven beads were filed against a Darwin test lane that no longer exists in the shape
they describe. This records what the lanes are *now*, what was measured against them,
and the per-bead verdict, so the next person does not re-derive it.

Measured 2026-08-26 on `origin/edge-integration` @ 2103ea04f (clean worktree).

## What actually gates a push here

| Lane | What it runs | Who runs it |
|---|---|---|
| `make test-mac` | `-p=4 -count=1 -timeout 15m $(UNIT_PKGS_NONCMDGC)` — **excludes cmd/gc** | the Darwin pre-push hook; the rig's configured `test_command` |
| `make test` | the same sweep, **plus** cmd/gc as 6 *sequential* shards | AGENTS.md fallback |
| `make test-fast-parallel` | shards cmd/gc locally at `LOCAL_TEST_JOBS` | the non-Darwin pre-push branch |

Two facts do most of the adjudication work:

1. **The active pre-push hook is not the in-tree one.** `core.hooksPath` points at
   `.beads/hooks/pre-push`, which shadows `.githooks/pre-push`. It dispatches on
   `uname -s`: Darwin runs `make test-mac`, everything else `make test-fast-parallel`.
   The rig additionally pins `test_command = "make test-mac"` in city.toml
   `[rigs.formula_vars]`. So on this host, **cmd/gc is not in the gate at all.**

2. **`make test`'s sweep phase became byte-identical to `make test-mac`.** Before
   gascity-cgh (961d87935) `make test` was `-p=4 ./...` *including* cmd/gc. After it,
   both targets run the same `UNIT_PKGS_NONCMDGC` sweep and cmd/gc moved to sequential
   shards. cgh changed **nothing** about the `test-mac` package set — it renamed
   `MAC_UNIT_PKGS` to `UNIT_PKGS_NONCMDGC`. For the two test-mac-lane beads the only
   relieving change is gascity-6tr's slot cap.

## The commit the cmd/gc beads never saw

`gascity-4k6` (3d5721df0, "make a failed lsof probe distinguishable from a genuine
negative") is the fix for *an lsof timeout being indistinguishable from "no port
holder"* — which is precisely the `port_holder_owned = "false", want true` and
`deleted_inodes = "false", want true` symptom that dominates gascity-4h5 / -4nv / -iql.

Checked by **ancestry, not author date** (it landed on edge-integration out of date
order):

| Tree | Bead(s) measured on it | 4k6 |
|---|---|---|
| cd954f8c6 | 4h5, 4nv, tqi | **absent** |
| 7f47d2db1 | iql | **absent** |
| b10d3510a | lm5 | **absent** |
| 027500873 | 4h5's own "correction" note | **absent** |
| 2103ea04f | this adjudication | **present** |

Every cmd/gc observation in this cluster predates the fix most likely to explain it.
That is why those beads were re-measured rather than reasoned about.

## The cap is working (and its stale-looking slot files are correct)

Both slot files held diagnostic lines naming **dead** pids — `slot-0` a prior gate of
this very session, `slot-1` a `gastown.refinery` gate ~17h old. `flock -n` showed both
slots genuinely **free** and `lsof` showed no leaked descriptor.

Per `scripts/push-gate-lock-lib.sh` the slot **content is diagnostic only**; flock(1) is
the whole mechanism and the kernel releases it when the last descriptor closes. A stale
line next to a free lock is expected. Do not "clean up" slot files: a slot that is
genuinely still locked with its gate gone means a descendant inherited the descriptor,
and the fix is `lsof <slot-file>` and killing that descendant.

## Baseline measurement

Three `make test-mac` runs on the identical tree, each taking a gate slot normally.

| Run | Wall | Verdict | Failing tests | `scripts` pkg | Load before → after | Other slot |
|---|---|---|---|---|---|---|
| 1 | 484s | PASS | 0 | 166.9s | 12.4 → 5.9 | free |
| 2 | 458s | PASS | 0 | 146.9s | 5.8 → 4.8 | free |
| 3 | 488s | PASS | 0 | 156.9s | 4.9 → 20.4 | free |

181/181 packages started every run. Live seats throughout: control-dispatcher, refinery,
witness, `crew.gasman`, mayor, deacon — normal operating conditions per the mayor's
amendment.

**Important qualifier on all three runs: the other gate slot was free the entire time.**
So this is a measurement at *1 of 2 slots*, not at cap-2 contention. It bounds
"does this reproduce when nothing else is gating", not "does the cap hold under load".

## Mechanisms read from the code (independent of any run)

- **gascity-pin** — `internal/runtime/tmux/startup_test.go:2747`. Real clock, not injected:
  a shell emitting `progress N` every ~100ms is asserted not to trip a **300ms** idle
  watchdog. A 3x margin against the scheduler. Nothing in cgh or 6tr touches it.
- **gascity-lm5** — `internal/productmetrics/spool_unix_test.go`. The clock IS injected
  (`deps.now`, `beforeRecordOperation`), and the failing assertion is a **write count**
  plus `Lstat` checks. A 0.11s failure therefore cannot be "ran out of time"; it points at
  a real filesystem-error path aborting before `generationOpen`. Mechanism still unknown.
- **gascity-tqi** — `cmd/gc/scoped_store_test.go:212`. The stub `bd` must fork `sleep 30 &`
  **and** write `bd-child.pid` inside a **200ms** context deadline before the process-group
  kill lands; the test then waits 5s for that file. On Darwin each unsigned-binary exec
  carries the ~250ms Gatekeeper tax measured in **gascity-wz1** — the same order as the
  entire budget. This predicts failure on a quiet box, which is exactly what the bead
  reports, and makes it the strongest STILL REAL candidate in the cluster.

## cmd/gc, 6-shard sweep — sweep 1

`make test-cmd-gc-process-parallel`, 651s, **rc=2**, 3 of 6 shards red. Three failures:

| Test | Message | In any bead's list? |
|---|---|---|
| `TestLoadStatusSessionSnapshotKillsBdChildOnTimeout` | `city_status_snapshot_test.go:228: timed out waiting for .../bd-child.pid to be written` | **no** |
| `TestGcBeadsBdStartConcurrentWaitPassesRemainingExistingManagedBudget` | `beads_provider_lifecycle_test.go:9333: unexpected gc args: dolt-config write-managed ...` | **no** |
| `TestBdRuntimeEnvUsesValidProviderStateWhenPublishedStateIsMissing` | `bd_env_test.go:847: listener process on 54111 ... did not become ready` | **no** |

**cmd/gc is still red under this sweep, but not one of the ~35 tests the beads name failed.**
The set rotated again — which is the beads' own thesis (an unstable set drawn from a shared
pool by load), now reproduced on the post-4k6 baseline.

### The one that is not noise: a shared bd-child-pid race

`TestLoadStatusSessionSnapshotKillsBdChildOnTimeout` fails with **exactly** the message
gascity-tqi is filed for. The reason is concrete:

```
$ grep -rn 'waitForNonEmptyFileContent' cmd/gc/*_test.go
cmd/gc/city_status_snapshot_test.go:228:  childPid := waitForNonEmptyFileContent(t, pidFile, 5*time.Second)
cmd/gc/scoped_store_test.go:245:          childPid := waitForNonEmptyFileContent(t, pidFile, 5*time.Second)
cmd/gc/scoped_store_test.go:256:     func waitForNonEmptyFileContent(...)
```

The helper has **two** call sites and both are the same defect: a stub `bd` must fork a
child *and* write its pid file inside a short cancellation window (200ms in tqi's case),
then the test waits 5s for that file. On Darwin the ~250ms per-exec Gatekeeper tax
(gascity-wz1) is the same order as the whole budget.

gascity-tqi names `scoped_store_test.go`'s call site only. The `city_status_snapshot_test.go`
one is **unfiled** — and in this sweep it is the one that failed while tqi's own test passed.
Whoever fixes tqi must fix the helper's contract, not one caller, or the gate stays red.

## cmd/gc, 6-shard sweep — sweep 2, and the disjointness result

`make test-cmd-gc-process-parallel` again, same tree, 615s, **rc=2**, 1 of 6 shards red:

| Test | Message |
|---|---|
| `TestScopedBdStoreForCityKillsChildOnCtxCancel` (**gascity-tqi**) | `scoped_store_test.go:245: timed out waiting for .../bd-child.pid to be written` |
| `TestProbeDetachedWork_TmuxExitStatus` | `detached_probe_test.go:98: Status = "timeout", want "alive" (err=context deadline exceeded)` |

```
sweep 1: TestBdRuntimeEnvUsesValidProviderStateWhenPublishedStateIsMissing
         TestGcBeadsBdStartConcurrentWaitPassesRemainingExistingManagedBudget
         TestLoadStatusSessionSnapshotKillsBdChildOnTimeout
sweep 2: TestProbeDetachedWork_TmuxExitStatus
         TestScopedBdStoreForCityKillsChildOnCtxCancel
in both: (none)
```

**Zero overlap between two runs of the same sweep on the same commit.** That is the
cluster's own thesis reproduced on the post-4k6 baseline: the failing set is drawn from a
pool by load, so a red sweep here carries no information about the tree under test. The
practical consequence stands unchanged — `make test-cmd-gc-process-parallel` and
`make test-fast-parallel` cannot discriminate a branch regression in cmd/gc from host
noise on this host, and must not be used as a merge gate.

Note also that **both** `waitForNonEmptyFileContent` call sites failed, one per sweep —
`city_status_snapshot_test.go:228` in sweep 1, `scoped_store_test.go:245` (tqi) in sweep 2.
Two independent confirmations of one shared defect, and the clearest signal in the cluster.

Every one of these five failures is a **deadline/readiness** assertion — `bd-child.pid`
not written in time, a listener not ready, a probe returning `timeout` instead of `alive`.
None is a logic assertion. That is the signature of tests measuring the host, and it is the
same family gascity-l5w was closed for.

## Verdicts

### gascity-irt — STALE PREMISE (close)

The bead is filed against `scripts` exceeding its 15m deadline "under full-suite
contention" in `make test`. That sweep contained cmd/gc, the single largest package
(~14.4 min of fast-unit alone). gascity-cgh removed it. The lane the bead measured
does not exist.

Measured in the surviving sweep, three runs: **166.9s / 146.9s / 156.9s — 16-19% of the
900s budget**, against the 900.498s deadline the bead recorded. Headroom went from ~1.0x
to ~5.4x. The refinery's own scope-correction note already bounded the bead to the
`make test` lane; that lane's sweep phase is now byte-identical to `test-mac`.

The underlying cost driver is real and belongs elsewhere: `scripts` is exec-heavy and
pays the ~250ms-per-exec Darwin Gatekeeper tax tracked in **gascity-wz1**. It is still
the third-slowest package in the sweep. Close irt referencing this bead; leave the cost
question to wz1.

### gascity-pin — STILL REAL (do not close)

`TestRunSetupCommandActivityStreamingSurvivesIdleWindow` passed **3/3** in the capped
lane (1.62s / 1.36s / 1.86s). That is *not* a clearance, for two reasons:

1. **The mechanism is untouched.** The test drives a real shell emitting `progress N`
   every ~100ms and asserts a **300ms** idle watchdog does not fire. Real clock, no
   injection, 3x margin. Neither cgh (which changed nothing about the test-mac package
   set) nor 6tr (a concurrency cap) alters it. The bead says this itself.
2. **All three runs held 1 of 2 slots** — the other slot was free throughout. So I never
   observed it at cap-2 contention, which is the condition the bead is about.

Verdict: the defect is intact and latent. What would settle whether it still gates
pushes is a run with a second gate deliberately holding the other slot — which the brief
correctly forbids manufacturing on a shared box. Keep open; the bead's own suggested
direction (drive the watchdog from an injected clock, or assert ordering rather than
wall-clock spacing) remains the right fix.

### gascity-lm5 — INCONCLUSIVE (do not close)

The named test and its sibling both passed 3/3 (`0.09/0.16/0.18s` and `0.08/0.06/0.02s`).
But this bead must not be closed on that, because **its mechanism was never identified**:

- the clock IS injected (`deps.now`, `beforeRecordOperation`), so the failure is not timing;
- the failing assertion is a **write count** plus `Lstat` checks, and the recorded failure
  took 0.11s — too fast to be "ran out of time";
- nothing in cgh, 6tr, or 4k6 touches a filesystem-error path in `internal/productmetrics`.

Three green runs at 1-of-2 slots therefore show only that it did not reproduce, not that
anything fixed it. Recording a verdict of RESOLVED here would send the next person to
debug against a premise we invented. Keep open; the bead's existing instruction — instrument
which `recordOperation`/`storageStep` sequence actually runs on a failing run before
touching any constant — is still the correct first move.

## cmd/gc isolated runs

35 named tests (the union of iql + 4h5 + 4nv + tqi), `-count=3 -p 1`, serial, isolated
env. Guard: the `-run` regex was verified with `go test -list` to match exactly 35 tests,
so a silent zero-match is ruled out.

**Result: 104 PASS, 1 FAIL of 105 executions.** The single failure:

```
=== RUN   TestScopedBdStoreForCityKillsChildOnCtxCancel
    scoped_store_test.go:245: timed out waiting for .../bd-child.pid to be written
--- FAIL: TestScopedBdStoreForCityKillsChildOnCtxCancel (5.37s)
--- PASS: TestScopedBdStoreForCityKillsChildOnCtxCancel (0.36s)
--- PASS: TestScopedBdStoreForCityKillsChildOnCtxCancel (0.36s)
```

Two details matter. A passing iteration takes **0.36s**; the failing one burns the full
**5.37s** wait. And the one that failed was the **first** iteration — the cold exec.
That is precisely what the gascity-wz1 Gatekeeper-tax model predicts: the first exec of a
freshly written stub pays the ~250ms tax against a 200ms cancellation budget, and
iterations 2-3 run warm.

A separate `-count=5` run of that test alone: **5/5 pass** (4.0s total).

So the other 34 named tests — every test named by gascity-iql, -4h5 and -4nv — are
**green in isolation, 102/102**, on this baseline.

### gascity-tqi — STILL REAL (confirmed, and wider than filed)

Full tally today:

| Shape | Result |
|---|---|
| 6-shard sweep 1 | pass |
| 6-shard sweep 2 | **FAIL** — `scoped_store_test.go:245` |
| isolated union, `-count=3 -p 1` | **1 FAIL** / 2 pass |
| isolated alone, `-count=5` | 5 pass |

**2 failures in 10 executions, one of them in a serial isolated run.** The prior carried
in the handoff — "believed to fail in isolation on a clean baseline" — is confirmed. This
is the one bead in the cluster that is unambiguously a live defect rather than lane noise.

It is also **wider than filed**. `waitForNonEmptyFileContent` has exactly two callers and
both exhibit the identical failure; `city_status_snapshot_test.go:228`
(`TestLoadStatusSessionSnapshotKillsBdChildOnTimeout`) failed in sweep 1 and is **unfiled**
— `gc bd search` on both the test name and `bd-child.pid` returns only tqi. Fixing one
caller leaves the gate red. Widen tqi to the helper's contract rather than filing a
duplicate.

### gascity-4h5 and gascity-4nv — STILL REAL as a family, STALE as filed (retitle + merge)

Both beads enumerate specific cmd/gc test names. **Not one of those names failed in either
sweep**, and all of them passed 3/3 in the isolated union. Meanwhile the sweeps were red
both times, with five distinct failures and zero overlap between runs.

So the two halves of these beads split cleanly:

- **The family claim is STILL REAL.** cmd/gc under the sharded local sweep produces
  nondeterministic failures on a tree with no branch content. Reproduced twice today.
- **The test enumerations are STALE.** They have now been superseded three times (this is
  the fourth distinct failing set recorded against this cluster). A fixer handed these
  lists would chase five tests that are currently green.

4h5's "fail only under the concurrent 6-shard local run" framing is already contradicted by
its own correction note, and the data agrees: the shared trait of every failure observed
today is a **deadline/readiness assertion against real host state** — a pid file not
written in time, a listener not ready, a probe returning `timeout` instead of `alive` —
not cross-shard interference.

Recommended disposition, which is what 4nv's own bead-hygiene note already asks for:
merge 4h5 and 4nv into a single bead titled for the *family and its mechanism* (e.g.
"cmd/gc tests assert host-state readiness on real deadlines and fail nondeterministically
under the local sharded sweep"), drop the enumerations into a "historical sightings"
section, and keep it searchable on `dolt-state` / `port` / `drift-check` / `readiness`
rather than on test names that rotate every run. Do not close either: the family
reproduces.

### gascity-iql — STALE PREMISE (close)

One `make test-fast-parallel` run on the clean baseline, the exact lane and width this
bead names:

```
make test-fast-parallel     1354s   rc=0   "All fast jobs passed"
Running 10 fast job(s) with LOCAL_TEST_JOBS=11 inner_p=1
  [unit-cmd-gc-1-of-6] ok   [unit-cmd-gc-4-of-6] ok
  [unit-cmd-gc-2-of-6] ok   [unit-cmd-gc-5-of-6] ok
  [unit-cmd-gc-3-of-6] ok   [unit-cmd-gc-6-of-6] ok
```

Zero `--- FAIL:` lines. All six cmd/gc shards were confirmed to have *run* (each logged
`start` then `ok`) rather than being skipped — a skipped shard is indistinguishable from a
pass by exit code alone. Against this bead's record of **5 of 6 shards red, ~20 tests,
MAKE_EXIT=2**.

All three of its claims are falsified on the current baseline: the lane is not red, there
are not ~20 failures, and "corrupts the refinery merge gate" is false by configuration —
the rig pins `test_command = "make test-mac"`, which excludes cmd/gc, and the Darwin hook
execs the same.

The residual is real but is not this bead: two 6-shard sweeps on this same commit were
both red with disjoint sets. That is 4h5/4nv's scope, and keeping iql open as a third
tracker of one family is what generates the duplicate lists this cluster already suffers
from. Closed with the residual folded into 4h5 + 4nv.


## Summary

| Bead | Verdict | Action |
|---|---|---|
| gascity-irt | **STALE PREMISE** | closed — the `-p=4 ./...` lane it measured no longer exists |
| gascity-pin | **STILL REAL** (latent) | open — 3/3 green, but at 1-of-2 slots and the mechanism is untouched |
| gascity-lm5 | **INCONCLUSIVE** | open — did not reproduce; mechanism still unidentified |
| gascity-tqi | **STILL REAL** (confirmed) | open, retitled — reproduces serially in isolation; two callers, one unfiled |
| gascity-4h5 | family real, enumeration stale | open, retitled off test names |
| gascity-4nv | family real, enumeration stale | open, retitled off test names |
| gascity-iql | **STALE PREMISE** | closed — lane green at the width it names; residual folded into 4h5/4nv |

## What this cluster actually is

Strip the lane confusion away and there are **two** defects, not seven:

1. **`waitForNonEmptyFileContent`'s two call sites** (gascity-tqi) — a real, reproducible
   race between a stub child's pid-file write and a 200ms cancellation budget, on a
   platform whose per-exec cost is of the same order. This one is worth fixing and is the
   only member that reproduces on a quiet box.
2. **A population of cmd/gc tests that assert host-state readiness on real deadlines**
   (gascity-4h5 / -4nv, and the rotating names behind gascity-iql). Individually they look
   like flakes; collectively they are one design problem — tests reading live host state
   instead of isolating it. The failing subset is drawn by load, which is why enumerating
   test names has now produced four superseded lists.

gascity-pin and gascity-lm5 sit outside both: same *symptom* class (load-coupled), but in
the test-mac lane and with unrelated mechanisms.

## Two process notes for whoever runs this next

**Enumerating test names in bead titles does not work for this family.** Four lists, all
superseded, and a genuine duplicate (`TestLoadStatusSessionSnapshotKillsBdChildOnTimeout`)
that title-search cannot find. Title beads by mechanism.

**A green run at 1-of-2 slots is not evidence about behaviour at cap-2.** Every `test-mac`
run in this adjudication had the other slot free, which is why gascity-pin is not closed
despite 3/3. Record the slot state alongside the result — `flock -n` on each slot file,
since the file *contents* are diagnostic only and routinely name dead pids.

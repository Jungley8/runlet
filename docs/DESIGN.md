# runlet design

## Problem

One self-hosted runner serves eleven repositories. A runner process takes one
job at a time, so independent jobs serialize. Measured on 2026-11-14:

- average CI run: 12.9 job-minutes
- one PR: 61.5 minutes queued, 15.1 minutes of work
- a 19-job workflow: 90.9 minutes wall clock

The host is a 2-core/4-thread mini PC with 7.6 GiB RAM and 5.7 GiB of swap
already in use. It also runs immich, multica, and clickhouse.

## What a scheduler can control

GitHub self-hosted runners poll for work. There is no queue API. Therefore:

1. **Capacity**: the number of runner processes, each with a cgroup budget.
2. **Routing**: the labels a runner registers with.
3. **Cancellation**: which runs still in `queued` are allowed to start.

`runlet` does not promise ordering, priority, or preemption. It cannot deliver
them.

## Layers

```
runlet (systemd --user service)
├── config        YAML, unknown keys rejected, fail-closed Validate()
├── ghapi         REST: list runs, cancel, registration token, runners
├── policy        Decide(snapshot, now, params) → []Decision   (pure)
├── supervisor    lane lifecycle, systemd-run scopes            (P2)
└── server        /metrics, /status, /healthz
```

The policy layer is pure. It takes a snapshot, a clock, and a grace period. It
returns decisions. The caller re-reads each run and confirms `queued` before it
POSTs a cancel. That re-read is the safety boundary, not the grace period.

## Decision rule

Group runs by `(workflow, head repository, branch, event)`. The survivor is the
run with the maximum `(created_at, run_id)`. Every older run that is still
`queued`, and older than the grace period, becomes a decision.

Never a target:

- `in_progress` runs
- `workflow_dispatch`, `schedule`, `release`, `delete` events
- tag pushes, which have no branch, so a group key would collapse every release
  into one
- manual re-runs (`run_attempt > 1`), whose `created_at` still points at the
  original commit and therefore always looks superseded
- a group whose two newest runs share a timestamp

The head repository is part of the key because two forks can both have a branch
named `main`.

Before each cancel the caller re-reads **both** runs. The target must still be
`queued`, and the survivor must still be `queued` or `in_progress`. Checking
only the target would cancel the last queued run on a branch when the survivor
finished a second earlier.

## Lane lifecycle

```
IDLE → TOKEN → CONFIGURING → LISTENING → RUNNING → DEREGISTERING → CLEANING → IDLE
                        ↘ error → BACKOFF (cap 5m) → IDLE
```

- Registration tokens are minted per attempt and used immediately. A token that
  expires within a minute is discarded rather than used.
- Runner names are attempt-unique (`runlet-<host>-lane-<n>-<8hex>`). Reusing a
  stable name with `--replace` lets two attempts de-register each other, so the
  flag is never passed.
- Each lane has its own directory. The runner install is hardlinked in, not
  copied: `.runner`, `.credentials`, and `_work` are not concurrency-safe, but
  the binaries are read-only and can share inodes. A version change in the
  source wipes and relinks the lane copy.
- Shared caches are injected as `GOCACHE`, `GOMODCACHE`, and `PNPM_STORE_DIR`.
- The lane runs under `systemd-run --user --scope` with `MemoryMax`,
  `MemoryHigh`, `CPUQuota`, `KillMode=control-group`, and `TimeoutStopSec=20`.
- Cancelling the supervisor's context kills `systemd-run`, not the scope, so
  stop always runs on a fresh context.
- Cleanup runs on every exit path: stop the scope, delete `_work` except
  `_actions`, rotate `_diag` older than seven days, delete the registration,
  drop the state file.
- A drain flag (`<state_dir>/DRAIN`) stops new attempts without a restart. An
  ephemeral lane finishes its current job by itself, so drain never kills work.
- On startup the supervisor adopts live `runlet-lane-*` scopes and waits for
  them; it never starts a second runner for an occupied slot. A lock file in
  `state_dir` stops a second supervisor.

GitHub has no REST endpoint that cancels a single job. When a lane dies
mid-job, runlet records the runner name and leaves the job to GitHub's own
runner-loss detection. Cancelling the whole run to free one job would destroy
the sibling jobs that are still healthy.

## Resource limits

`user@1000.service` has `Delegate=yes`, linger is on, and the `cpu memory pids`
controllers are delegated. A user service can set `MemoryMax`, `MemoryHigh`, and
`CPUQuota` on transient scopes without root. Verified on the host; do not assume
it elsewhere.

The aggregate lane budget must fit under `pool.memory_budget`, and `doctor`
refuses a lane count when `MemAvailable` is below it.

## Phases

| Phase | Scope |
|---|---|
| P1 | Config, GitHub client, policy engine, dry-run, metrics |
| P1.5 | `apply` with an hourly cap and an audit log |
| P2 | Lane supervisor with cgroup caps and ephemeral registration |
| P3 | Status, doctor, drain |
| P4 | Packaging and release automation |

## Failure modes

| Failure | Guard |
|---|---|
| Cancellation storm | dry-run default, event deny list, hourly cap, kill switch, audit log |
| Wrong cancellation | re-read target and survivor before POST; never target `in_progress` |
| Token file readable by others | mode 0600 enforced on load and at startup, even under `EnvironmentFile=` |
| Endpoints exposed on the LAN | `server.listen` must be loopback or startup fails |
| Orphaned registrations | attempt-unique names, startup adoption, deletion by name after every exit |
| Host OOM | per-lane `MemoryMax`, aggregate budget, `MemoryHigh`, doctor refuses without headroom |
| Token expiry | fetch per cycle, discard partial credentials, retry once |
| API rate limit | ETags, 30 s poll, backoff; cancellation pauses |

# runlet

A single-host scheduler for self-hosted GitHub Actions runners.

`runlet` does three things:

- **Cancellation:** cancel queued runs that a newer run on the same branch has
  superseded.
- **Lanes:** run N resource-capped runner processes from one binary.
- **Reporting:** queue depth, oldest queued run, and lane state on `/metrics`
  and `/status`.

`runlet` does not reorder GitHub's queue. Self-hosted runners pull jobs, and no
API lets an outside process see or reorder that queue. `runlet` controls three
things only:

1. How many lanes exist.
2. Which labels each lane carries.
3. Which already-created runs may continue.

## Why

A single runner process takes one job at a time. When several repositories share
that runner, a burst of pushes leaves a long queue:

- 200 workflow runs in 7 days on one repository; the average CI run is
  12.9 job-minutes.
- One pull request waited 61.5 minutes for a 15.1-minute run.
- A 19-job workflow took 90.9 minutes of wall clock. The jobs are independent;
  serialization produced the duration.

Superseded runs are the cheapest waste to remove. If a branch moved twice while
a run waited, only the newest run can produce a useful result.

## Status

P1 and P2. The policy controller works and defaults to `dry-run`. The lane
supervisor runs when `pool.lanes` is configured.

## Build

```sh
make build          # bin/runlet
make test           # go test ./... -race
```

Go 1.27 or newer.

## Install as a user service

```sh
make build
sh scripts/install.sh
```

The script installs the binary to `~/.local/bin`, the unit to
`~/.config/systemd/user`, and enables `runlet.service`. It does not need root.

## Configure

Copy `runlet.example.yaml` to `~/.config/runlet/config.yaml`.

The token is separate from the config. Put it in `~/.config/runlet/env`:

```sh
install -m 600 /dev/null ~/.config/runlet/env
printf 'GITHUB_TOKEN=%s\n' "$TOKEN" >> ~/.config/runlet/env
```

The file must be mode 0600. `runlet` refuses to read it otherwise, and it
checks the mode even when systemd has already exported the value through
`EnvironmentFile=`.

`server.listen` must be a loopback address. `/metrics` and `/status` have no
authentication, so binding to a LAN address is rejected at startup.

A dry-run token needs read access to Actions. `apply` needs write access to
cancel runs.

Lanes need a second token that can mint registration tokens: `RUNLET_ADMIN_TOKEN`
in the same env file. Keep the admin token out of every other use.

## Commands

```sh
runlet doctor                       # token, API reachability, delegation, memory
runlet status                       # queue depth, oldest queued run, lane state
runlet serve                        # poll and act; dry-run unless mode: apply
runlet drain                        # let running jobs finish, start no new lane
runlet drain -off                   # resume starting lanes
runlet policy check SNAPSHOT        # replay a recorded snapshot, print decisions
```

`policy check` reads a YAML or JSON file:

```yaml
now: 2026-11-14T12:00:00Z
repos:
  - slug: octarq-org/octarq-pro
    runs:
      - {id: 1, workflow_id: 9, head_branch: feat, event: pull_request,
         status: queued, created_at: 2026-11-14T11:40:00Z}
```

## Lanes

Set `pool.lanes` to run runner processes. Each lane is its own runner
registration, its own directory, and its own systemd scope:

- The runner install in `pool.runner_dir` is hardlinked into each lane. One
  inode per file, so a second lane costs almost no disk.
- `pool.runner_dir` must be a complete extracted runner release, or a live
  install of one. `run.sh` sources `run-helper.sh.template` and `safe_sleep.sh`
  from its own directory; a copy that lacks them starts and exits before it
  listens. Point it at the install directory, not at a hand-picked subset.
- Each lane gets its own `HOME`, `.runner`, `.credentials`, and `_work`. Those
  are not safe to share between runner processes.
- `GOCACHE`, `GOMODCACHE`, and `PNPM_STORE_DIR` point at one shared cache, so
  two lanes do not re-download the same modules.
- `systemd-run --user --scope` sets `MemoryMax`, `MemoryHigh`, and `CPUQuota`.
  `KillMode=control-group` stops a job's child processes with the lane.
- Runner names are unique per attempt, so two overlapping attempts cannot
  de-register each other.
- Registrations are ephemeral: each lane takes one job, exits, and is cleaned
  up. `_work/_actions` survives, because that is the action cache.

Start lanes only when the host has memory to spare:

```sh
runlet doctor    # fails when MemAvailable is below pool.memory_budget
```

Two lanes on a host that is already swapping trades a queue for an OOM kill of
something else on the machine. `runlet doctor` checks this; do not raise
`pool.lanes` to meet a deadline.

On restart, the supervisor adopts live `runlet-lane-*` scopes instead of starting
duplicates, and a lock file stops a second supervisor.

## Safety

- Dry-run is the default. `apply` is an explicit choice.
- Only `queued` runs are cancelled. An `in_progress` run is never touched.
- `workflow_dispatch`, `schedule`, `release`, and `delete` runs are never
  cancelled.
- Tag pushes have no branch and are never cancelled. A manual re-run is an
  explicit human instruction and is never cancelled.
- Pull requests from different forks with the same branch name are different
  groups.
- If two runs claim the newest timestamp, nothing in that group is cancelled.
- Both the target and the newest run are re-read before a cancel. If the newest
  run already finished, nothing is cancelled: the older run is then the only
  one left.
- Each repository has an hourly cancellation cap.
- Create `KILL` in `~/.config/runlet/` (or in `pool.state_dir` when set) to
  pause cancellation without a restart.
- Every cancellation writes an audit line with the run URL and the survivor.

## Metrics

`serve` exposes `GET /metrics`, `GET /status`, and `GET /healthz` on
`server.listen` (loopback by default).

## Limitations

- `runlet` cannot reorder GitHub's queue.
- `runlet` cannot preempt a job that GitHub already assigned.
- Identical-label lanes add concurrency, not priority. Priority requires
  partitioned labels and a `runs-on` change in each workflow.
- GitHub has no REST endpoint that cancels a single job. When a lane dies
  mid-job, runlet logs the runner name and relies on GitHub's own runner-loss
  detection.

## License

MIT. See `LICENSE`.

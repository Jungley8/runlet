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

- Size a lane from what the job needs, not from what the build alone needs.
  The runner process itself takes about 0.5 GiB of the lane, so a 2.5 GiB lane
  leaves roughly 2 GiB for the job.
- Keep `memory_high` equal to `memory`. A lower `memory_high` makes the lane
  reclaim memory early, and the reclaimed pages are the lane's own page cache.
  A compiler then re-reads the same sources from disk: measured on one host at
  `memory_high: 1.6GiB` with a 2 GiB cap, two compile processes read 264 GB
  each from disk at 470 MB/s and the disk, not the CPU, set the pace. When the
  cap is the only threshold, the cache stays and the reads stop. The process
  logs a warning at startup for a `memory_high` below `memory`.
- The `memory.pressure` file in the lane's cgroup shows whether a lane is
  waiting on memory. `systemctl --user show <unit>.scope -p MemoryCurrent`
  shows what it holds.
- Each lane sets `TMPDIR` to its own directory inside the lane, not the host's
  `/tmp`. The directory is emptied at the start of every attempt and after
  every job. When `/tmp` is a tmpfs, a leftover build directory costs memory,
  and a full one fails every lane at once: the error arrives as `no space left
  on device` in whichever step writes a large file, not as a build error.
- `pool.scratch` watches the host temp filesystem anyway, because tools outside
  the lane write there too. `runlet_scratch_used_ratio` reports the fill level,
  a warning is logged once per crossing of `warn_percent` (85 by default), and
  a sweep runs every `every` (24h by default). The sweep removes directories
  that match `patterns` (`go-build*` by default), are older than `stale` (12h
  by default), and belong to the service user. It does not follow symlinks and
  does not touch plain files. `runlet status` and `runlet doctor` print the
  current usage.
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
- Set `pool.env` to inject environment variables into each lane; keys reserved by runlet are rejected.
- `systemd-run --user --scope` sets `MemoryMax`, `MemoryHigh`, and `CPUQuota`.
  `KillMode=control-group` stops a job's child processes with the lane.
- Runner names are unique per attempt, so two overlapping attempts cannot
  de-register each other.
- Registrations are ephemeral: each lane takes one job, exits, and is cleaned
  up. `_work/_actions` survives, because that is the action cache.
- A cancelled job can leave a detached process tree behind. The runner then
  waits for that tree forever and the lane never takes new work. When GitHub
  reports the runner busy, or drops its registration, while no in-progress job
  names it for `pool.stale_lane_minutes` (15 by default, `0` turns it off),
  runlet stops the scope. The stop is what kills the tree: `KillMode=control-group`
  reaches every process left in the cgroup. `runlet_lane_stale_stops_total`
  counts these. An incomplete API answer stops nothing.

Start lanes only when the host has memory to spare:

```sh
runlet doctor    # fails when MemAvailable is below pool.memory_budget
```

Two lanes on a host that is already swapping trades a queue for an OOM kill of
something else on the machine. `runlet doctor` checks this; do not raise
`pool.lanes` to meet a deadline.

On restart, the supervisor adopts live `runlet-lane-*` scopes instead of starting
duplicates, and a lock file stops a second supervisor. A stop does not kill a
running lane: the scope and its job continue, and the next process adopts them.

## Safety

- Dry-run is the default. `apply` is an explicit choice.
- Only `queued` runs are cancelled. An `in_progress` run is never touched.
- `workflow_dispatch`, `schedule`, `release`, and `delete` runs are never
  cancelled.
- Tag pushes have no branch and are never cancelled. A manual re-run is an
  explicit human instruction and is never cancelled.
- Pull requests from different forks with the same branch name are different
  groups, and so are pull requests from one head branch into different base
  branches.
- If two runs claim the newest timestamp, nothing in that group is cancelled.
- Both the target and the newest run are re-read before a cancel. If the newest
  run already finished, nothing is cancelled: the older run is then the only
  one left.
- Each repository has an hourly cancellation cap.
- Create `KILL` in `~/.config/runlet/` (or in `pool.state_dir` when set) to
  pause cancellation without a restart.
- Every cancellation writes an audit line with the run URL and the survivor.

## Security

`runlet` runs as a user service and starts runners in scopes of the same user.
That has consequences you must decide on before you deploy it.

What the code does:

- The config file and the token file must be regular files with mode `0600`.
  Anything else stops the process. Unknown keys and explicit zeros are errors.
- The HTTP endpoint binds to loopback and rejects any `Host` header that is not
  a loopback name, so a web page cannot reach it through DNS rebinding.
- Tokens are never logged. The registration token is redacted from every
  command and error message. The child environment carries the proxy and the
  user bus only.
- Lane directories are `0700`. Cleanup and log rotation refuse to follow a
  symlink that a job planted in `_work` or `_diag`.
- Runner names carry a per-instance prefix. Deregistration and the stale sweep
  only touch names with that prefix, and an adopted scope is deregistered only
  when the on-disk state file names that very unit.
- Each scope is capped: `MemoryMax`, `MemoryHigh`, `MemorySwapMax=0`,
  `TasksMax`, and `CPUQuota`.

What the code cannot do for you:

- **A job runs as the same user as `runlet`.** It can read
  `~/.config/runlet/env` and `/proc/<runlet>/environ`, and the state directory.
  Run this only for workflows you would let log in to the host. Never point it
  at a repository that runs untrusted pull requests.
- **An org-scope registration mints tokens for the whole org.** Every
  repository in the organization can match the lane labels, including a fork
  pull request someone approved. Add a label of your own (for example
  `macmini-ci`) to every lane and require it in `runs-on`, and put the runner
  in a runner group that only lists the repositories you mean.
- **A job can start its own scope.** `systemd-run --user` inside a job creates
  a sibling unit without the lane caps. The limits protect the host from
  runaway builds, not from hostile ones. To bound that, cap the whole user
  slice from the system manager
  (`sudo systemctl set-property user-1000.slice MemoryMax=4G`) and keep the cap
  above the sum of the lane limits. Or give the runners a dedicated user and
  cap that user's slice.
- **`bin` and `externals` are hardlinks.** A job that writes to its lane's
  `bin/Runner.Listener` writes the shared inode, and every later lane runs the
  modified binary. Re-extract the runner from a known tarball after a host has
  run untrusted code. Pointing `pool.runner_dir` at an install you do not own
  makes `runlet` copy instead of link, at the cost of disk.

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

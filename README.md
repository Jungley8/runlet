# runlet

A single-host scheduler for self-hosted GitHub Actions runners.

`runlet` does two things:

- **P1 (this release):** cancel queued runs that a newer run on the same branch
  has superseded.
- **P2 (next):** run N resource-capped runner lanes from one binary.

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

P1. The policy controller works and defaults to `dry-run`. Lane management is
not in this build.

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

## Commands

```sh
runlet doctor                       # token, API reachability, delegation, memory
runlet status                       # queue depth and oldest queued run per repo
runlet serve                        # poll and act; dry-run unless mode: apply
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
- P1 does not manage runners.

## License

MIT. See `LICENSE`.

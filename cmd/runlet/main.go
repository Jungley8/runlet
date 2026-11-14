// Command runlet is a single-host GitHub Actions runner scheduler.
//
// P1 (this binary) implements the policy controller: it finds queued runs that
// a newer run on the same branch has superseded and, in apply mode, cancels
// them. Lane management is P2 and is not in this build.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Jungley8/runlet/internal/config"
	"github.com/Jungley8/runlet/internal/ghapi"
	"github.com/Jungley8/runlet/internal/metrics"
	"github.com/Jungley8/runlet/internal/policy"
	"github.com/Jungley8/runlet/internal/serve"
	"gopkg.in/yaml.v3"
)

// version is overridden by goreleaser through -X main.version.
var version = "0.1.0-dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "serve":
		err = cmdServe(os.Args[2:])
	case "status":
		err = cmdStatus(os.Args[2:])
	case "doctor":
		err = cmdDoctor(os.Args[2:])
	case "policy":
		err = cmdPolicy(os.Args[2:])
	case "version", "-version", "--version":
		fmt.Println("runlet " + version)
		return
	case "help", "-h", "--help":
		usage()
		return
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "runlet:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `runlet — single-host GitHub Actions runner scheduler

Usage:
  runlet serve   [-config PATH]        run the policy controller and metrics endpoint
  runlet status  [-config PATH]        print queue depth for the configured repos
  runlet doctor  [-config PATH]        check token, API, cgroup delegation, memory
  runlet policy check SNAPSHOT         replay a recorded snapshot through the policy
  runlet version

The controller defaults to dry-run. Set policy.mode: apply to cancel runs.
`)
}

func defaultDir() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "."
	}
	return filepath.Join(dir, "runlet")
}

func defaultConfigPath() string { return filepath.Join(defaultDir(), "config.yaml") }
func defaultEnvPath() string    { return filepath.Join(defaultDir(), "env") }

func newLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	cfgPath := fs.String("config", defaultConfigPath(), "config file")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	token, err := config.ResolveToken(defaultEnvPath())
	if err != nil {
		return err
	}

	log := newLogger()
	log.Info("starting", "mode", cfg.Policy.ModeValue(), "repos", len(cfg.Repos), "poll", cfg.Policy.PollEvery().String())
	if cfg.Policy.ModeValue() == "dry-run" {
		log.Info("dry-run: decisions are logged, no run is cancelled")
	}

	killSwitch := filepath.Join(defaultDir(), "KILL")
	if cfg.Pool.StateDir != "" {
		killSwitch = filepath.Join(cfg.Pool.StateDir, "KILL")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := serve.New(serve.Options{
		Config:     cfg,
		Client:     ghapi.New(token),
		Metrics:    metrics.New(),
		Log:        log,
		KillSwitch: killSwitch,
	})
	return srv.Serve(ctx)
}

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	cfgPath := fs.String("config", defaultConfigPath(), "config file")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	token, err := config.ResolveToken(defaultEnvPath())
	if err != nil {
		return err
	}
	client := ghapi.New(token)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	fmt.Printf("%-40s %8s %8s %10s\n", "REPO", "QUEUED", "RUNNING", "OLDEST")
	var firstErr error
	for _, repo := range cfg.Repos {
		queued, err := client.ListRuns(ctx, repo.Slug, policy.StatusQueued)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			fmt.Printf("%-40s %8s %8s %10s  %v\n", repo.Slug, "?", "?", "?", err)
			continue
		}
		running, err := client.ListRuns(ctx, repo.Slug, policy.StatusInProgress)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		var oldest int64
		for _, r := range queued {
			if age := int64(time.Since(r.CreatedAt).Seconds()); age > oldest {
				oldest = age
			}
		}
		fmt.Printf("%-40s %8d %8d %9ds\n", repo.Slug, len(queued), len(running), oldest)
	}

	if len(cfg.Pool.Lanes) > 0 {
		fmt.Println()
		fmt.Printf("%-20s %-28s %10s %8s\n", "LANE", "LABELS", "MEMORY", "CPU")
		for _, l := range cfg.Pool.Lanes {
			fmt.Printf("%-20s %-28s %10s %8s\n", l.Name, strings.Join(l.Labels, ","), l.Memory, l.CPUQuota)
		}
	}
	return firstErr
}

func cmdDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	cfgPath := fs.String("config", defaultConfigPath(), "config file")
	if err := fs.Parse(args); err != nil {
		return err
	}

	var failures []error
	check := func(name string, err error) {
		if err != nil {
			failures = append(failures, err)
			fmt.Printf("FAIL  %s: %v\n", name, err)
			return
		}
		fmt.Printf("ok    %s\n", name)
	}

	cfg, err := config.Load(*cfgPath)
	check("config "+*cfgPath, err)
	if err != nil {
		return errors.Join(failures...)
	}

	token, err := config.ResolveToken(defaultEnvPath())
	check("token", err)
	if err == nil {
		client := ghapi.New(token)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		rl, rerr := client.RateLimit(ctx)
		cancel()
		check("github api", rerr)
		if rerr == nil {
			fmt.Printf("      rate limit: %d/%d remaining, resets %s\n",
				rl.Remaining, rl.Limit, rl.Reset.Format(time.RFC3339))
		}
	}

	if runtime.GOOS == "linux" {
		check("cgroup memory+cpu delegation", checkDelegation())
		if cfg.Pool.MemoryBudget > 0 {
			avail, merr := memAvailable()
			check("memavailable", merr)
			if merr == nil {
				fmt.Printf("      available: %s, lane budget: %s\n", config.Bytes(avail), cfg.Pool.MemoryBudget)
				if avail < cfg.Pool.MemoryBudget.Int64() {
					check("memory headroom", fmt.Errorf("available %s is below pool.memory_budget %s",
						config.Bytes(avail), cfg.Pool.MemoryBudget))
				}
			}
		}
	} else {
		fmt.Printf("skip  cgroup delegation (not Linux: %s)\n", runtime.GOOS)
	}

	return errors.Join(failures...)
}

func cmdPolicy(args []string) error {
	if len(args) == 0 || args[0] != "check" {
		return errors.New("usage: runlet policy check SNAPSHOT [-config PATH]")
	}
	fs := flag.NewFlagSet("policy check", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "optional config, used for grace_seconds")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: runlet policy check SNAPSHOT [-config PATH]")
	}

	grace := 45 * time.Second
	if *cfgPath != "" {
		cfg, err := config.Load(*cfgPath)
		if err != nil {
			return err
		}
		grace = cfg.Policy.Grace()
	}

	raw, err := os.ReadFile(fs.Arg(0))
	if err != nil {
		return err
	}
	var snap policy.Snapshot
	if err := yaml.Unmarshal(raw, &snap); err != nil {
		return fmt.Errorf("parse snapshot: %w", err)
	}
	now := snap.Now
	if now.IsZero() {
		now = time.Now()
	}

	fmt.Printf("%-34s %8s %8s %-24s %-20s %s\n", "REPO", "RUN", "SURVIVOR", "BRANCH", "WORKFLOW", "REASON")
	total := 0
	for _, repo := range snap.Repos {
		for _, d := range policy.Decide(repo.Slug, repo.Runs, now, policy.Params{Grace: grace}) {
			total++
			fmt.Printf("%-34s %8d %8d %-24s %-20s %s\n",
				d.Repo, d.RunID, d.SurvivorID, d.Branch, d.Workflow, d.Reason)
		}
	}
	if total == 0 {
		fmt.Println("\nno cancellations")
	}
	return nil
}

func checkDelegation() error {
	uid := os.Getuid()
	path := fmt.Sprintf("/sys/fs/cgroup/user.slice/user-%d.slice/user@%d.service/cgroup.controllers", uid, uid)
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	controllers := string(raw)
	for _, want := range []string{"memory", "cpu"} {
		if !strings.Contains(controllers, want) {
			return fmt.Errorf("%s lacks %q (have %q)", path, want, strings.TrimSpace(controllers))
		}
	}
	return nil
}

func memAvailable() (int64, error) {
	raw, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "MemAvailable:" {
			kb, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil {
				return 0, err
			}
			return kb * 1024, nil
		}
	}
	return 0, errors.New("MemAvailable not found in /proc/meminfo")
}

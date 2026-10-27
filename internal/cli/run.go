package cli

import (
	"fmt"
	"os"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/joejenniges/fleet/internal/dur"
	"github.com/joejenniges/fleet/internal/runner"
	"github.com/joejenniges/fleet/internal/store"
)

func runCmd() *cobra.Command {
	var o struct {
		critical bool
		reason   string
		purpose  string
		noRecord bool
	}
	c := &cobra.Command{
		Use:   "run [flags] -- <command> [args...]",
		Short: "Run a command transparently and record it (exit code, duration, output tail)",
		Long: "Runs the command with the same stdin/stdout/stderr and exit code. The run is recorded so agents can\n" +
			"build verified knowledge from it. --critical protects the process from being killed for its lifetime;\n" +
			"commands the policy marks `never` (migrations, DB clients) are critical automatically.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := openApp()
			if err != nil {
				// Fail open: run the command anyway.
				fmt.Fprintf(os.Stderr, "fleet: %v (running without recording)\n", err)
				return exitCode(runner.Run(runner.Options{Args: args}).ExitCode)
			}
			defer a.Close()
			code := a.RunCommand(appRunOpts(args, o.critical, o.reason, o.purpose, o.noRecord))
			if code != 0 {
				return exitCode(code)
			}
			return nil
		},
	}
	c.Flags().SetInterspersed(false) // everything after the command belongs to it
	c.Flags().BoolVar(&o.critical, "critical", false, "never kill this process while it runs")
	c.Flags().StringVar(&o.reason, "reason", "", "why it is critical (shown to other agents)")
	c.Flags().StringVar(&o.purpose, "purpose", "", "why this process is running (shown in fleet status)")
	c.Flags().BoolVar(&o.noRecord, "no-record", false, "do not store the command or its output")
	return c
}

func runsCmd() *cobra.Command {
	c := &cobra.Command{Use: "runs", Short: "Browse recorded command history"}
	var filt store.RunFilter
	var here, mine bool
	list := &cobra.Command{Use: "list", Short: "List recent runs"}
	lf := addFormat(list)
	list.Flags().BoolVar(&here, "here", true, "only runs in this repository")
	list.Flags().BoolVar(&mine, "mine", false, "only runs by the calling agent")
	list.Flags().BoolVar(&filt.Failed, "failed", false, "only failed runs")
	list.Flags().StringVar(&filt.Contains, "grep", "", "command contains")
	list.Flags().IntVar(&filt.Limit, "limit", 30, "max runs")
	list.RunE = func(cmd *cobra.Command, args []string) error {
		a, err := openApp()
		if err != nil {
			return err
		}
		defer a.Close()
		if here {
			filt.RepoRoot = a.Repo
		}
		if mine {
			filt.AgentID = a.AgentID()
		}
		runs, err := a.St.ListRuns(filt)
		if err != nil {
			return err
		}
		if lf.json() {
			return printJSON(os.Stdout, runs)
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		now := time.Now()
		for _, r := range runs {
			fmt.Fprintf(tw, "#%d\t%s\t%s\t%s\t%s\n", r.ID, exitText(r.ExitCode), dur.Ago(r.StartedAt, now), r.Source, trunc(r.Command, 80))
		}
		return tw.Flush()
	}
	show := &cobra.Command{Use: "show <id>", Short: "Show one run with its output", Args: cobra.ExactArgs(1)}
	sf := addFormat(show)
	show.RunE = func(cmd *cobra.Command, args []string) error {
		a, err := openApp()
		if err != nil {
			return err
		}
		defer a.Close()
		id, err := strconv.ParseInt(args[0], 10, 64)
		if err != nil {
			return err
		}
		r, err := a.St.GetRun(id)
		if err != nil {
			return err
		}
		if r == nil {
			return fmt.Errorf("no run #%d", id)
		}
		if sf.json() {
			return printJSON(os.Stdout, r)
		}
		fmt.Printf("run #%d  exit %s  %dms  %s  (%s)\n  %s\n  cwd %s\n", r.ID, exitText(r.ExitCode), r.DurationMS,
			r.StartedAt.Format(time.RFC3339), r.Source, r.Command, r.Cwd)
		if r.ToolVersion != "" {
			fmt.Printf("  tool %s\n", r.ToolVersion)
		}
		if r.Output != "" {
			fmt.Printf("--- output (%d lines%s) ---\n%s\n", r.OutputLines, map[bool]string{true: ", compacted"}[r.OutputCompacted], r.Output)
		}
		return nil
	}
	c.AddCommand(list, show)
	return c
}

func exitText(p *int) string {
	if p == nil {
		return "?"
	}
	if *p == 0 {
		return "ok"
	}
	return "exit " + strconv.Itoa(*p)
}

func pruneCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "prune",
		Short: "Apply run-history retention now (max_age, compact_after/compact_keep_lines in config.toml)",
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := openApp()
			if err != nil {
				return err
			}
			defer a.Close()
			d, c, err := a.Prune()
			if err != nil {
				return err
			}
			fmt.Printf("deleted %d run(s), compacted output of %d run(s)\n", d, c)
			return nil
		},
	}
}

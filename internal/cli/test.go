package cli

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/joejenniges/fleet/internal/dur"
	"github.com/joejenniges/fleet/internal/testkit"
)

func testCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "test",
		Short: "Post-install tests: isolated test home, deterministic selfcheck, live Claude scenarios",
		Long: `Typical use:

  fleet test selfcheck            # deterministic, no agent needed (also good for CI)
  fleet test standup              # isolated test home (own DB, config, policy)
  fleet test claude               # live headless Claude Code sessions against it
  fleet test teardown             # kill fixtures, delete the test home

Isolation is by FLEET_HOME, set only on processes the test launches, so agents you
are running for real keep using the real database. To poke at the test home by hand:
  source "$(fleet test env --path)"   (or: . <dir>\env.ps1 in PowerShell)`,
	}
	c.AddCommand(testStandupCmd(), testTeardownCmd(), testEnvCmd(), testSelfcheckCmd(), testClaudeCmd(),
		testServePortCmd(), testExitCmd(), testSleepCmd())
	return c
}

func testStandupCmd() *cobra.Command {
	var dir string
	c := &cobra.Command{
		Use:   "standup [--dir PATH]",
		Short: "Create an isolated test home (database, config, policy, Claude settings)",
		RunE: func(cmd *cobra.Command, args []string) error {
			h, err := testkit.Standup(dir)
			if err != nil {
				return err
			}
			fmt.Printf("test home: %s\n  FLEET_HOME=%s\n  work dirs: %s\n  logs:      %s\n", h.Dir, h.FleetHome, h.Work, h.Logs)
			if testkit.UserHasFleetHooks() {
				fmt.Println("  note: your ~/.claude/settings.json already runs fleet hooks; test sessions will use those (they inherit FLEET_HOME).")
			}
			fmt.Printf("Run `fleet test claude` next, or `source %s/env.sh` to use it by hand.\n", h.Dir)
			return nil
		},
	}
	c.Flags().StringVar(&dir, "dir", "", "directory to use (default: a new temp dir)")
	return c
}

func testTeardownCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "teardown",
		Short: "Kill test fixture processes and delete the test home",
		RunE: func(cmd *cobra.Command, args []string) error {
			h, killed, err := testkit.Teardown()
			if killed > 0 {
				fmt.Printf("killed %d fixture process(es)\n", killed)
			}
			if h == nil {
				fmt.Println("no test home was stood up")
				return err
			}
			if err != nil {
				return fmt.Errorf("removing %s: %w", h.Dir, err)
			}
			fmt.Printf("removed %s\n", h.Dir)
			return nil
		},
	}
}

func testEnvCmd() *cobra.Command {
	var path, ps bool
	c := &cobra.Command{
		Use:   "env [--path] [--powershell]",
		Short: "Print the environment that points fleet at the test home",
		RunE: func(cmd *cobra.Command, args []string) error {
			h, err := testkit.Current()
			if err != nil {
				return err
			}
			if h == nil {
				return fmt.Errorf("no test home; run `fleet test standup`")
			}
			switch {
			case path && ps:
				fmt.Println(h.Dir + string(os.PathSeparator) + "env.ps1")
			case path:
				fmt.Println(h.Dir + string(os.PathSeparator) + "env.sh")
			case ps:
				fmt.Printf("$env:FLEET_HOME = %q\n", h.FleetHome)
			default:
				fmt.Printf("export FLEET_HOME=%q\n", h.FleetHome)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&path, "path", false, "print the env file path instead")
	c.Flags().BoolVar(&ps, "powershell", false, "PowerShell syntax")
	return c
}

func testSelfcheckCmd() *cobra.Command {
	var keep bool
	c := &cobra.Command{
		Use:   "selfcheck",
		Short: "Deterministic end-to-end check of hooks, policy, run recording, cleanup and MCP (no agent needed)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if n := testkit.Selfcheck(os.Stdout, keep); n > 0 {
				return exitCode(1)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&keep, "keep", false, "keep the temporary fleet home for inspection")
	return c
}

func testClaudeCmd() *cobra.Command {
	var o testkit.HarnessOptions
	var only []string
	var repeat int
	var list, quiet bool
	var timeout string
	c := &cobra.Command{
		Use:   "claude [--only a,b] [--model M] [--repeat N]",
		Short: "Run live headless Claude Code scenarios against the test home (costs tokens)",
		Long: "Each scenario launches one or more `claude -p` sessions in fresh work directories under the test home.\n" +
			"HARD checks are guaranteed by fleet's hooks and fail the run; soft checks depend on the agent following\n" +
			"instructions and are reported as misses. If no test home is stood up, a temporary one is created and\n" +
			"removed afterwards.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if list {
				for _, s := range testkit.Scenarios() {
					fmt.Printf("%-16s %s\n", s.Name, s.Desc)
				}
				return nil
			}
			if err := testkit.Preflight(); err != nil {
				return err
			}
			h, err := testkit.Current()
			if err != nil {
				return err
			}
			temp := false
			if h == nil {
				if h, err = testkit.Standup(""); err != nil {
					return err
				}
				temp = true
				fmt.Printf("stood up temporary test home %s\n", h.Dir)
			}
			if temp {
				defer func() {
					if _, _, err := testkit.Teardown(); err == nil {
						fmt.Println("temporary test home removed (logs were in it; use `fleet test standup` first to keep them)")
					}
				}()
			}
			prev, had := os.LookupEnv("FLEET_HOME")
			os.Setenv("FLEET_HOME", h.FleetHome)
			defer func() {
				if had {
					os.Setenv("FLEET_HOME", prev)
				} else {
					os.Unsetenv("FLEET_HOME")
				}
			}()
			if o.Timeout, err = dur.Parse(timeout); err != nil {
				return err
			}
			if !quiet {
				o.Verbose = os.Stdout
			}
			checks := testkit.RunScenarios(&testkit.Harness{Home: h, Opt: o}, only, repeat)
			fmt.Println()
			if testkit.Report(os.Stdout, checks) > 0 {
				fmt.Printf("session transcripts: %s\n", h.Logs)
				return exitCode(1)
			}
			return nil
		},
	}
	c.Flags().StringSliceVar(&only, "only", nil, "scenarios to run (default: all; see --list)")
	c.Flags().BoolVar(&list, "list", false, "list scenarios")
	c.Flags().StringVar(&o.Model, "model", "", "claude --model (e.g. a small fast model to keep cost down)")
	c.Flags().IntVar(&o.MaxTurns, "max-turns", 12, "claude --max-turns per session")
	c.Flags().StringVar(&timeout, "timeout", "4m", "per-session timeout")
	c.Flags().IntVar(&repeat, "repeat", 1, "run each scenario N times (soft checks then show a rate)")
	c.Flags().BoolVar(&quiet, "quiet", false, "only print the report")
	return c
}

// ---- hidden fixture helpers used by the tests ----

func testServePortCmd() *cobra.Command {
	var port int
	var label string
	var detach bool
	c := &cobra.Command{
		Use:    "serve-port",
		Short:  "Test fixture: listen on a port until terminated",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if label == "" {
				return fmt.Errorf("--fleet-test-fixture=<label> is required")
			}
			return testkit.ServePort(port, label, detach)
		},
	}
	c.Flags().IntVar(&port, "port", 0, "port (0 = any free port)")
	c.Flags().StringVar(&label, strings.TrimSuffix(strings.TrimPrefix(testkit.FixtureFlag, "--"), "="), "", "fixture label")
	c.Flags().BoolVar(&detach, "detach", false, "start in the background and return")
	return c
}

func testExitCmd() *cobra.Command {
	return &cobra.Command{
		Use:                "exit <code> [--say text] [anything…]",
		Short:              "Test fixture: print --say text and exit with code (other args are ignored)",
		Hidden:             true,
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return fmt.Errorf("exit code required")
			}
			code, err := strconv.Atoi(args[0])
			if err != nil {
				return err
			}
			for i := 1; i < len(args)-1; i++ {
				if args[i] == "--say" {
					if code == 0 {
						fmt.Println(args[i+1])
					} else {
						fmt.Fprintln(os.Stderr, args[i+1])
					}
				}
			}
			if code != 0 {
				return exitCode(code)
			}
			return nil
		},
	}
}

func testSleepCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "sleep <duration>",
		Short:  "Test fixture: sleep",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			d, err := dur.Parse(args[0])
			if err != nil {
				return err
			}
			time.Sleep(d)
			fmt.Println("slept", d)
			return nil
		},
	}
}

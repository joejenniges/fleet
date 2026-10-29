package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"slices"

	"github.com/spf13/cobra"

	"github.com/joejenniges/fleet/internal/app"
	"github.com/joejenniges/fleet/internal/hooks"
	"github.com/joejenniges/fleet/internal/install"
	"github.com/joejenniges/fleet/internal/mcpserver"
	"github.com/joejenniges/fleet/internal/paths"
	"github.com/joejenniges/fleet/internal/store"
)

func appRunOpts(args []string, critical bool, reason, purpose string, noRecord bool) app.RunOpts {
	return app.RunOpts{Args: args, Critical: critical, Reason: reason, Purpose: purpose, NoRecord: noRecord}
}

func hookCmd() *cobra.Command {
	var kind, wake string
	c := &cobra.Command{
		Use:   "hook <" + joinEvents() + ">",
		Short: "Agent hook entry point (reads the hook JSON payload on stdin)",
		Long: "Fails open: exits 0 with no output on any error. The one deliberate non-zero exit is `stop` exiting 2\n" +
			"with signal data on stderr, which wakes (--wake async) or continues (--wake block) a waiting agent.",
		Args:      cobra.ExactArgs(1),
		ValidArgs: hooks.Events,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !slices.Contains(hooks.Events, args[0]) {
				return nil // unknown event: fail open
			}
			if code := hooks.Main(args[0], kind, wake, os.Stdin, os.Stdout, os.Stderr); code != 0 {
				return exitCode(code)
			}
			return nil
		},
	}
	c.Flags().StringVar(&kind, "agent", "claude", "agent kind: claude | codex | gemini | cursor | other")
	c.Flags().StringVar(&wake, "wake", hooks.WakeAsync, "stop hook mode: async (installed with asyncRewake) | block")
	return c
}

func joinEvents() string {
	s := ""
	for i, e := range hooks.Events {
		if i > 0 {
			s += "|"
		}
		s += e
	}
	return s
}

func mcpCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "mcp",
		Short: "Run the MCP server on stdio (configure your agent to launch `fleet mcp`)",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
			defer stop()
			return mcpserver.Serve(ctx, Version)
		},
	}
}

func installCmd() *cobra.Command {
	var scope, wake string
	var dry bool
	c := &cobra.Command{
		Use:   "install claude [--scope user|project] [--wake async|block] [--dry-run]",
		Short: "Wire fleet into an agent: MCP server, hooks, skill",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if args[0] != "claude" {
				return fmt.Errorf("only `claude` is supported so far (codex/gemini/cursor adapters are phase 4)")
			}
			cwd, _ := os.Getwd()
			p, err := install.PlanClaude(scope, paths.RepoRoot(cwd), wake)
			if err != nil {
				return err
			}
			if dry {
				fmt.Printf("would write %s:\n%s\n\nwould write skill to %s\nwould run: %v\n", p.SettingsPath, p.Settings, p.SkillPath, p.MCPCommand)
				if p.ProjectMD != "" {
					fmt.Printf("would add a coordination section to %s\n", p.ProjectMD)
				}
				return nil
			}
			if err := p.Apply(os.Stdout); err != nil {
				return err
			}
			fmt.Println("\nDone. Start a new Claude Code session to pick up the hooks. Check with `fleet status`.")
			return nil
		},
	}
	c.Flags().StringVar(&scope, "scope", "user", "user (all projects) | project (this repo)")
	c.Flags().BoolVar(&dry, "dry-run", false, "show what would change")
	c.Flags().StringVar(&wake, "wake", install.WakeAsync, "how waiting agents are woken: async (asyncRewake; agent goes idle) | "+
		"block (Stop hook holds the turn, for Claude Code versions without asyncRewake)")
	return c
}

func uninstallCmd() *cobra.Command {
	var scope string
	c := &cobra.Command{
		Use:   "uninstall claude [--scope user|project]",
		Short: "Remove fleet's hooks, skill and MCP registration (the database is kept)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cwd, _ := os.Getwd()
			return install.Uninstall(scope, paths.RepoRoot(cwd), os.Stdout)
		},
	}
	c.Flags().StringVar(&scope, "scope", "user", "user | project")
	return c
}

func msgCmd() *cobra.Command {
	c := &cobra.Command{Use: "msg", Short: "Send a message to an agent (delivered once, at its next hook)"}
	var typ string
	send := &cobra.Command{
		Use:   "send <agent|*> \"text\" [--type fyi|release_request]",
		Short: "Queue a message",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if typ != store.MsgFYI && typ != store.MsgReleaseRequest {
				return fmt.Errorf("--type must be fyi or release_request")
			}
			a, err := openApp()
			if err != nil {
				return err
			}
			defer a.Close()
			body := map[string]string{"text": args[1], "from": a.Who()}
			if args[0] == "*" {
				return a.St.Broadcast(a.AgentID(), a.AgentID(), typ, body)
			}
			ag, err := a.St.GetAgent(args[0])
			if err != nil || ag == nil {
				return fmt.Errorf("no agent %q", args[0])
			}
			return a.St.SendMessage(a.AgentID(), ag.ID, typ, body)
		},
	}
	send.Flags().StringVar(&typ, "type", store.MsgFYI, "fyi | release_request")
	inbox := &cobra.Command{
		Use:   "inbox",
		Short: "Read (and consume) the calling agent's undelivered messages",
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := openApp()
			if err != nil {
				return err
			}
			defer a.Close()
			id := a.AgentID()
			if id == "" {
				return fmt.Errorf("not inside an agent session")
			}
			msgs, err := a.St.DrainInbox(id)
			if err != nil {
				return err
			}
			return json.NewEncoder(os.Stdout).Encode(msgs)
		},
	}
	c.AddCommand(send, inbox)
	return c
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version and data locations",
		RunE: func(cmd *cobra.Command, args []string) error {
			db, _ := paths.DBPath()
			ucd, _ := paths.UserConfigDir()
			fmt.Printf("fleet %s\n  database:      %s\n  user config:   %s\n  global policy: %s\n", Version, db, ucd, paths.GlobalConfigDir())
			if h := paths.Home(); h != "" {
				fmt.Printf("  FLEET_HOME:    %s (isolated)\n", h)
			}
			return nil
		},
	}
}

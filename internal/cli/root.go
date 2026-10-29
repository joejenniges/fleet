// Package cli is the cobra command tree. Commands are thin: logic lives in app.
package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/joejenniges/fleet/internal/app"
)

// Version is set at build time (-ldflags "-X github.com/joejenniges/fleet/internal/cli.Version=…").
var Version = "dev"

func Execute() int {
	root := &cobra.Command{
		Use:           "fleet",
		Short:         "Coordinate coding agents on one machine: presence, kill policy, run history, shared command knowledge",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(
		statusCmd(), whoamiCmd(), runCmd(), runsCmd(), policyCmd(), docsCmd(),
		hookCmd(), mcpCmd(), installCmd(), uninstallCmd(), pruneCmd(), msgCmd(), priorityCmd(),
		eventsCmd(), versionCmd(), testCmd(), signalCmd(), waitCmd(), waitsCmd(), signalsCmd(),
		planned("preempt", "Ask a lower-priority agent to shelve and release resources (phase 2, DESIGN.md §7)"),
		planned("lease", "Acquire/release leases on ports, browser profiles, DBs, paths (phase 2, DESIGN.md §10)"),
		planned("port", "Allocate a free port leased to the caller (phase 2)"),
		planned("browser-mcp", "Wrap chrome-devtools-mcp with a leased profile slot (phase 2)"),
		planned("board", "Shared key/value scratch space (phase 2)"),
		planned("ui", "Terminal dashboard (phase 4)"),
	)
	if err := root.Execute(); err != nil {
		if ec, ok := err.(exitCode); ok {
			return int(ec)
		}
		fmt.Fprintln(os.Stderr, "fleet:", err)
		return 1
	}
	return 0
}

// exitCode lets a command return a specific process exit status.
type exitCode int

func (e exitCode) Error() string { return fmt.Sprintf("exit %d", int(e)) }

func planned(name, short string) *cobra.Command {
	return &cobra.Command{
		Use:                name,
		Short:              short + " [not yet implemented]",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return fmt.Errorf("`fleet %s` is planned but not implemented yet — see DESIGN.md", name)
		},
	}
}

func openApp() (*app.App, error) {
	a, err := app.Open()
	if err != nil {
		return nil, fmt.Errorf("open fleet database: %w", err)
	}
	return a, nil
}

// format flag

type formatFlag struct{ v string }

func addFormat(c *cobra.Command) *formatFlag {
	f := &formatFlag{v: "pretty"}
	c.Flags().StringVar(&f.v, "format", "pretty", "output format: pretty | json")
	return f
}

func (f *formatFlag) json() bool { return f.v == "json" }

func printJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// isHumanTerminal: interactive stdin and no agent ancestor. Used to gate
// human-only actions (approve/reject) and direct policy edits.
func isHumanTerminal(a *app.App) bool {
	return term.IsTerminal(int(os.Stdin.Fd())) && a.Agent() == nil
}

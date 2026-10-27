package cli

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/joejenniges/fleet/internal/app"
	"github.com/joejenniges/fleet/internal/dur"
	"github.com/joejenniges/fleet/internal/store"
)

func docsCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "docs",
		Short: "Shared knowledge: commands that work, known-bad commands, behaviors, notes",
	}
	c.AddCommand(docsAddCmd(), docsListCmd(false), docsListCmd(true), docsShowCmd(), docsTopCmd(), docsKnownCmd(), docsRetireCmd())
	return c
}

func docsAddCmd() *cobra.Command {
	var in app.AddDocInput
	c := &cobra.Command{
		Use:   "add <kind> \"<command or title>\" [--note …] [--from-run ID] [--fixes ID] [--scope repo|machine]",
		Short: "Record knowledge (kinds: " + strings.Join(store.DocKinds, ", ") + ")",
		Example: `  fleet docs add known-bad "dotnet ef upgrade" --from-run 812 --note "not an ef verb"
  fleet docs add valid-command "dotnet ef database update" --from-run 815 --fixes 41
  fleet docs add behavior "integration tests need docker compose up -d db first"`,
		Args: cobra.ExactArgs(2),
	}
	f := addFormat(c)
	c.Flags().StringVar(&in.Note, "note", "", "explanation")
	c.Flags().Int64Var(&in.FromRun, "from-run", 0, "run id that proves it (verifies the entry)")
	c.Flags().Int64Var(&in.Fixes, "fixes", 0, "known-bad entry this command replaces")
	c.Flags().StringVar(&in.Scope, "scope", "", "repo | machine (default: repo inside a git repo)")
	c.RunE = func(cmd *cobra.Command, args []string) error {
		in.Kind, in.Text = args[0], args[1]
		a, err := openApp()
		if err != nil {
			return err
		}
		defer a.Close()
		res, err := a.AddDoc(in)
		if err != nil {
			return err
		}
		if f.json() {
			return printJSON(os.Stdout, res)
		}
		fmt.Printf("#%d %s: %s — %s\n", res.Doc.ID, res.Doc.Kind, res.Doc.Label(), res.Message)
		return nil
	}
	return c
}

func docsListCmd(search bool) *cobra.Command {
	var kinds []string
	var all bool
	use, short := "list", "List knowledge entries visible here"
	if search {
		use, short = "search <words…>", "Search knowledge entries"
	}
	c := &cobra.Command{Use: use, Short: short}
	f := addFormat(c)
	c.Flags().StringSliceVar(&kinds, "kind", nil, "filter by kind")
	c.Flags().BoolVar(&all, "retired", false, "include retired entries")
	c.RunE = func(cmd *cobra.Command, args []string) error {
		a, err := openApp()
		if err != nil {
			return err
		}
		defer a.Close()
		filt := store.DocFilter{RepoRoot: a.Repo, Kinds: kinds, IncludeRetired: all}
		if search {
			if len(args) == 0 {
				return fmt.Errorf("give search words")
			}
			filt.Query = strings.Join(args, " ")
		}
		docs, err := a.St.ListDocs(filt)
		if err != nil {
			return err
		}
		for _, d := range docs {
			a.WithTally(d)
		}
		if f.json() {
			return printJSON(os.Stdout, docs)
		}
		if len(docs) == 0 {
			fmt.Println("No entries.")
		}
		for _, d := range docs {
			printDocLine(d)
		}
		return nil
	}
	return c
}

func printDocLine(d *store.Doc) {
	flags := []string{d.Scope}
	if d.Verified {
		flags = append(flags, "verified")
	} else if d.Kind == store.KindValid || d.Kind == store.KindBad {
		flags = append(flags, "unverified")
	}
	if t := d.Tally; t != nil && t.Successes+t.Failures > 0 {
		flags = append(flags, fmt.Sprintf("✓%d ✗%d", t.Successes, t.Failures))
		if t.Stale {
			flags = append(flags, "STALE?")
		}
	}
	if d.Execs > 0 {
		flags = append(flags, fmt.Sprintf("ran %d×", d.Execs))
	}
	if d.FixDocID != 0 {
		flags = append(flags, fmt.Sprintf("fix: #%d", d.FixDocID))
	}
	if !d.RetiredAt.IsZero() {
		flags = append(flags, "RETIRED")
	}
	fmt.Printf("#%-4d %-13s %s  [%s]\n", d.ID, d.Kind, d.Label(), strings.Join(flags, ", "))
	if d.Body != "" && d.Body != d.Label() {
		fmt.Printf("       %s\n", trunc(d.Body, 110))
	}
}

func docsShowCmd() *cobra.Command {
	c := &cobra.Command{Use: "show <id>", Short: "Show one entry with its run history", Args: cobra.ExactArgs(1)}
	f := addFormat(c)
	c.RunE = func(cmd *cobra.Command, args []string) error {
		a, err := openApp()
		if err != nil {
			return err
		}
		defer a.Close()
		id, err := strconv.ParseInt(args[0], 10, 64)
		if err != nil {
			return err
		}
		d, err := a.St.GetDoc(id)
		if err != nil {
			return err
		}
		if d == nil {
			return fmt.Errorf("no doc #%d", id)
		}
		a.WithTally(d)
		var runs []*store.Run
		if d.NormCmd != "" {
			runs, _ = a.St.ListRuns(store.RunFilter{NormCmd: d.NormCmd, Limit: 10})
		}
		if f.json() {
			return printJSON(os.Stdout, map[string]any{"doc": d, "recent_runs": runs})
		}
		printDocLine(d)
		fmt.Printf("       created by %s %s\n", orDash(d.CreatedBy), d.CreatedAt.Format("2006-01-02"))
		if d.EvidenceRunID != 0 {
			fmt.Printf("       evidence: run #%d\n", d.EvidenceRunID)
		}
		if t := d.Tally; t != nil && len(t.ToolVersions) > 0 {
			fmt.Printf("       tool versions seen: %s\n", strings.Join(t.ToolVersions, "; "))
		}
		now := time.Now()
		for _, r := range runs {
			fmt.Printf("       run #%d %s %s\n", r.ID, exitText(r.ExitCode), dur.Ago(r.StartedAt, now))
		}
		return nil
	}
	return c
}

func docsTopCmd() *cobra.Command {
	var kind string
	var n int
	c := &cobra.Command{Use: "top", Short: "Most-executed documented commands here (what agents see at session start)"}
	f := addFormat(c)
	c.Flags().StringVar(&kind, "agent-kind", "", "claude | codex | gemini | cursor (default: calling agent's kind)")
	c.Flags().IntVar(&n, "limit", 0, "entries (default from config)")
	c.RunE = func(cmd *cobra.Command, args []string) error {
		a, err := openApp()
		if err != nil {
			return err
		}
		defer a.Close()
		if kind == "" {
			kind = "claude"
			if ag := a.Agent(); ag != nil {
				kind = ag.Kind
			}
		}
		res, err := a.TopDocs(kind, n)
		if err != nil {
			return err
		}
		if f.json() {
			return printJSON(os.Stdout, res)
		}
		for _, d := range res.Top {
			printDocLine(d)
		}
		for _, h := range res.Hidden {
			fmt.Printf("  hidden #%d %s — %s\n", h.Doc.ID, h.Doc.Label(), h.Reason)
		}
		return nil
	}
	return c
}

func docsKnownCmd() *cobra.Command {
	var file string
	c := &cobra.Command{
		Use:   "known <id> [--file CLAUDE.md]",
		Short: "Mark an entry as already documented in your instructions file (hidden from top list until it expires)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := openApp()
			if err != nil {
				return err
			}
			defer a.Close()
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return err
			}
			until, err := a.MarkKnown(id, file)
			if err != nil {
				return err
			}
			fmt.Printf("#%d hidden from the top list until %s\n", id, until.Format("2006-01-02"))
			return nil
		},
	}
	c.Flags().StringVar(&file, "file", "", "instructions file (default: your agent's, e.g. CLAUDE.md)")
	return c
}

func docsRetireCmd() *cobra.Command {
	var reason string
	c := &cobra.Command{
		Use:   "retire <id> --reason …",
		Short: "Retire an entry that is no longer true (kept for history)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if reason == "" {
				return fmt.Errorf("--reason is required")
			}
			a, err := openApp()
			if err != nil {
				return err
			}
			defer a.Close()
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return err
			}
			if err := a.St.RetireDoc(id, reason); err != nil {
				return err
			}
			a.St.LogEvent(a.AgentID(), "docs.retired", map[string]any{"id": id, "reason": reason})
			fmt.Printf("#%d retired\n", id)
			return nil
		},
	}
	c.Flags().StringVar(&reason, "reason", "", "why it is no longer true")
	return c
}

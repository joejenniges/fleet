package cli

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/joejenniges/fleet/internal/app"
	"github.com/joejenniges/fleet/internal/cmdtext"
	"github.com/joejenniges/fleet/internal/policy"
	"github.com/joejenniges/fleet/internal/runner"
	"github.com/joejenniges/fleet/internal/store"
)

func policyCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "policy",
		Short: "Kill policy: test, propose changes, review and approve them",
		Long: "Policy decides what fleet may do to a process: never touch it, ask its agent, or kill it.\n" +
			"Rules: the most restrictive match across all levels wins. Defaults: the most specific level wins.\n" +
			"Levels: builtin < global < user < repo, plus runtime critical marks.",
	}
	c.AddCommand(policyTestCmd(), policyAddCmd(), policyRemoveCmd(), policySetDefaultCmd(), policyShowFilesCmd(),
		policyPendingCmd(), policyShowCmd(), policyCommentCmd(), policyDecideCmd(true), policyDecideCmd(false))
	return c
}

func policyTestCmd() *cobra.Command {
	var pid int32
	c := &cobra.Command{
		Use:   "test [--pid N] [-- <command> [args...]]",
		Short: "Show what each policy level says about a command or a live process",
		Example: "  fleet policy test -- dotnet ef database update\n" +
			"  fleet policy test --pid 4242 --format json",
	}
	f := addFormat(c)
	c.Flags().Int32Var(&pid, "pid", 0, "evaluate a live process (includes lineage and critical marks)")
	c.Flags().SetInterspersed(false)
	c.RunE = func(cmd *cobra.Command, args []string) error {
		a, err := openApp()
		if err != nil {
			return err
		}
		defer a.Close()
		var res policy.Result
		switch {
		case pid != 0:
			if res, err = a.TestPID(pid); err != nil {
				return err
			}
		case len(args) > 0:
			line := runner.JoinArgs(args)
			if len(args) == 1 {
				line = args[0] // a single quoted string is a whole command line
			}
			res = a.TestCommand(cmdtext.Argv0(line), line)
		default:
			return errors.New("give a command after -- or --pid")
		}
		if f.json() {
			return printJSON(os.Stdout, res)
		}
		printPolicyResult(res)
		return nil
	}
	return c
}

func printPolicyResult(r policy.Result) {
	fmt.Printf("input: %s\n", r.Input.Cmdline)
	for _, lv := range r.Levels {
		state := ""
		switch {
		case !lv.Exists:
			state = " (no file)"
		case lv.Error != "":
			state = " (INVALID: " + lv.Error + "; using last good copy)"
		}
		verdict := "-"
		if lv.Verdict != "" {
			verdict = string(lv.Verdict)
		}
		fmt.Printf("  %-8s %-6s %s%s\n", lv.Level, verdict, lv.File, state)
		for _, m := range lv.Matched {
			extra := ""
			if m.Grace != "" {
				extra = " grace " + m.Grace
			}
			fmt.Printf("           matched %s → %s%s  %s\n", m.ID, m.Action, extra, m.Reason)
		}
		if d := lv.Defaults; d != (policy.Defaults{}) {
			fmt.Printf("           defaults: unknown=%s grace=%s on_timeout=%s\n", orDash(string(d.Unknown)), orDash(d.Grace), orDash(d.OnTimeout))
		}
	}
	crit := "no"
	if r.Runtime.Critical {
		crit = "yes (" + r.Runtime.Reason + ")"
	}
	fmt.Printf("  %-8s critical: %s\n", "runtime", crit)
	fmt.Printf("final: %s  grace %s  on_timeout %s\n  decided by: %s\n", strings.ToUpper(string(r.Final.Action)),
		r.Final.Grace, r.Final.OnTimeout, strings.Join(r.Final.DecidedBy, ", "))
	for _, h := range r.Final.Lineage {
		fmt.Printf("  protected by %s pid %d (%s) → %s via %s\n", h.Relation, h.PID, trunc(h.Cmdline, 60), h.Action, h.By)
	}
}

type changeFlags struct {
	level  string
	reason string
}

func (cf *changeFlags) add(c *cobra.Command) {
	c.Flags().StringVar(&cf.level, "level", "user", "user | repo | global")
	c.Flags().StringVar(&cf.reason, "reason", "", "why (required; shown to the user and other agents)")
}

func submitChange(c policy.Change) error {
	a, err := openApp()
	if err != nil {
		return err
	}
	defer a.Close()
	res, err := a.ProposePolicy(c, isHumanTerminal(a))
	if err != nil {
		return err
	}
	fmt.Printf("#%d %s — %s change: %s\n", res.ProposalID, res.Summary, res.Class, res.Message)
	return nil
}

func policyAddCmd() *cobra.Command {
	var cf changeFlags
	var r policy.Rule
	var action string
	c := &cobra.Command{
		Use:   "add --id ID [--name a,b] [--args-contains S] [--cmdline-regex R] --action never|ask|kill --reason …",
		Short: "Add a rule (never/ask apply now; kill waits for the user's approval)",
		RunE: func(cmd *cobra.Command, args []string) error {
			r.Action = policy.Action(action)
			r.Reason = cf.reason
			return submitChange(policy.Change{Op: policy.OpAddRule, Level: cf.level, Rule: &r, Reason: cf.reason})
		},
	}
	cf.add(c)
	c.Flags().StringVar(&r.ID, "id", "", "stable rule id")
	c.Flags().StringSliceVar(&r.Name, "name", nil, "process names (basename, case-insensitive, .exe optional)")
	c.Flags().StringVar(&r.ArgsContains, "args-contains", "", "substring of the full command line")
	c.Flags().StringVar(&r.CmdlineRegex, "cmdline-regex", "", "regex over the full command line")
	c.Flags().StringVar(&action, "action", "", "never | ask | kill")
	c.Flags().StringVar(&r.Grace, "grace", "", "e.g. 3s")
	c.Flags().StringVar(&r.OnTimeout, "on-timeout", "", "notify | kill (for ask)")
	return c
}

func policyRemoveCmd() *cobra.Command {
	var cf changeFlags
	var id string
	c := &cobra.Command{
		Use:   "remove --id ID --reason …",
		Short: "Remove a rule (removing never/ask waits for the user's approval)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return submitChange(policy.Change{Op: policy.OpRemoveRule, Level: cf.level, RuleID: id, Reason: cf.reason})
		},
	}
	cf.add(c)
	c.Flags().StringVar(&id, "id", "", "rule id")
	return c
}

func policySetDefaultCmd() *cobra.Command {
	var cf changeFlags
	var d policy.Defaults
	var unknown string
	c := &cobra.Command{
		Use:   "set-default [--unknown never|ask|kill] [--grace 10s] [--on-timeout notify|kill] --reason …",
		Short: "Change level defaults (more conservative values apply now; others wait for approval)",
		RunE: func(cmd *cobra.Command, args []string) error {
			d.Unknown = policy.Action(unknown)
			return submitChange(policy.Change{Op: policy.OpSetDefault, Level: cf.level, Defaults: &d, Reason: cf.reason})
		},
	}
	cf.add(c)
	c.Flags().StringVar(&unknown, "unknown", "", "action for processes no rule matches")
	c.Flags().StringVar(&d.Grace, "grace", "", "default grace period")
	c.Flags().StringVar(&d.OnTimeout, "on-timeout", "", "notify | kill")
	return c
}

func policyShowFilesCmd() *cobra.Command {
	c := &cobra.Command{Use: "files", Short: "Show the policy files that apply here and their rules"}
	f := addFormat(c)
	c.RunE = func(cmd *cobra.Command, args []string) error {
		a, err := openApp()
		if err != nil {
			return err
		}
		defer a.Close()
		set := a.Policy()
		if f.json() {
			type lv struct {
				*policy.Level
				Policy policy.File `json:"policy"`
			}
			var out []lv
			for _, l := range set.Levels {
				out = append(out, lv{l, l.File})
			}
			return printJSON(os.Stdout, out)
		}
		eff, from := set.EffectiveDefaults()
		fmt.Printf("effective defaults: unknown=%s (%s) grace=%s (%s) on_timeout=%s (%s)\n\n",
			eff.Unknown, from["unknown"], eff.Grace, from["grace"], eff.OnTimeout, from["on_timeout"])
		for _, l := range set.Levels {
			state := ""
			if !l.Exists {
				state = " (no file)"
			} else if l.Error != "" {
				state = " (INVALID: " + l.Error + ")"
			}
			fmt.Printf("[%s] %s%s\n", l.Name, l.Path, state)
			for _, r := range l.File.Rules {
				var m []string
				if len(r.Name) > 0 {
					m = append(m, "name="+strings.Join(r.Name, ","))
				}
				if r.ArgsContains != "" {
					m = append(m, "args_contains="+strconv.Quote(r.ArgsContains))
				}
				if r.CmdlineRegex != "" {
					m = append(m, "regex="+strconv.Quote(r.CmdlineRegex))
				}
				fmt.Printf("  %-22s %-5s %s\n", r.ID, r.Action, strings.Join(m, " "))
			}
		}
		return nil
	}
	return c
}

func policyPendingCmd() *cobra.Command {
	var status string
	c := &cobra.Command{Use: "pending", Short: "List pending policy proposals with comments (agents: use --format json)"}
	f := addFormat(c)
	c.Flags().StringVar(&status, "status", store.PropPending, "pending | applied | approved | rejected | all")
	c.RunE = func(cmd *cobra.Command, args []string) error {
		a, err := openApp()
		if err != nil {
			return err
		}
		defer a.Close()
		if status == "all" {
			status = ""
		}
		views, err := a.ProposalViews(status, 100)
		if err != nil {
			return err
		}
		if f.json() {
			return printJSON(os.Stdout, map[string]any{"proposals": views,
				"how_to_approve": "the user runs `fleet policy approve <id>` (or `reject`) in their own terminal"})
		}
		if len(views) == 0 {
			fmt.Println("No proposals.")
			return nil
		}
		for _, v := range views {
			printProposal(v)
		}
		if status == store.PropPending {
			fmt.Println("Approve: fleet policy approve <id> [--note …]   Reject: fleet policy reject <id> --note …")
		}
		return nil
	}
	return c
}

func printProposal(v *app.ProposalView) {
	mine := ""
	if v.IsMine {
		mine = "  [yours]"
	}
	fmt.Printf("#%d  %s  (%s, %s)%s\n", v.ID, v.Summary, v.Class, v.Status, mine)
	fmt.Printf("    by %s at %s — reason: %s\n", v.ProposedName, v.ProposedAt.Format("2006-01-02 15:04"), v.ParsedChange.Reason)
	for _, c := range v.Comments {
		who := c.AgentID
		if who == "" {
			who = "human"
		}
		fmt.Printf("    • %s (%s): %s\n", who, c.Stance, c.Body)
	}
	if v.DecidedNote != "" {
		fmt.Printf("    decision note: %s\n", v.DecidedNote)
	}
}

func policyShowCmd() *cobra.Command {
	c := &cobra.Command{Use: "show <id>", Short: "Show one proposal", Args: cobra.ExactArgs(1)}
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
		v, err := a.ProposalView(id)
		if err != nil {
			return err
		}
		if v == nil {
			return fmt.Errorf("no proposal #%d", id)
		}
		if f.json() {
			return printJSON(os.Stdout, v)
		}
		printProposal(v)
		return nil
	}
	return c
}

func policyCommentCmd() *cobra.Command {
	var stance string
	c := &cobra.Command{
		Use:   "comment <id> --stance support|oppose|neutral \"text\"",
		Short: "Record your view on a proposal (the author is notified)",
		Args:  cobra.ExactArgs(2),
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
			if err := a.CommentProposal(id, stance, args[1]); err != nil {
				return err
			}
			fmt.Printf("comment recorded on #%d\n", id)
			return nil
		},
	}
	c.Flags().StringVar(&stance, "stance", "neutral", "support | oppose | neutral")
	return c
}

func policyDecideCmd(approve bool) *cobra.Command {
	name, short := "approve", "Approve and apply a pending proposal (user only, interactive terminal)"
	if !approve {
		name, short = "reject", "Reject a pending proposal (user only, interactive terminal)"
	}
	var note string
	c := &cobra.Command{
		Use:   name + " <id>",
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := openApp()
			if err != nil {
				return err
			}
			defer a.Close()
			if !isHumanTerminal(a) {
				return fmt.Errorf("only the user can %s proposals, from an interactive terminal outside any agent session", name)
			}
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return err
			}
			p, err := a.DecideProposal(id, approve, note)
			if err != nil {
				return err
			}
			fmt.Printf("#%d %s\n", p.ID, p.Status)
			return nil
		},
	}
	c.Flags().StringVar(&note, "note", "", "note recorded with the decision")
	return c
}

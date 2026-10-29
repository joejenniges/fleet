package cli

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/joejenniges/fleet/internal/dur"
	"github.com/joejenniges/fleet/internal/store"
)

func statusCmd() *cobra.Command {
	var all bool
	c := &cobra.Command{
		Use:   "status",
		Short: "Show active agents, their processes and ports, orphans, and pending policy changes",
	}
	f := addFormat(c)
	c.Flags().BoolVar(&all, "all", false, "include ended agents")
	c.RunE = func(cmd *cobra.Command, args []string) error {
		a, err := openApp()
		if err != nil {
			return err
		}
		defer a.Close()
		a.Housekeep()
		st, err := a.Status(all)
		if err != nil {
			return err
		}
		if f.json() {
			return printJSON(os.Stdout, st)
		}
		now := time.Now()
		if len(st.Agents) == 0 {
			fmt.Println("No active agents.")
		}
		for _, v := range st.Agents {
			me := ""
			if v.IsMe {
				me = "  ← you"
			}
			fmt.Printf("● %s  [%s] %s  pid %d  prio %d  seen %s%s\n", v.Name, v.Kind, v.Status, v.PID, v.Priority,
				dur.Ago(v.LastSeen, now), me)
			fmt.Printf("    %s\n", v.Cwd)
			printProcs(v.Processes)
			for _, w := range v.Waits {
				fmt.Printf("    ⏳ waiting on `%s` (%s, %s): %s\n", w.Signal, w.Mode, dur.Ago(w.CreatedAt, now), trunc(w.Description, 80))
			}
		}
		if st.AllWaiting {
			fmt.Println("\nWARNING: every active agent is waiting on a signal; nobody may be left to send one.")
		}
		if len(st.Orphans) > 0 {
			fmt.Printf("\nOrphaned processes (their agent ended):\n")
			printProcs(st.Orphans)
		}
		if st.Pending > 0 {
			fmt.Printf("\n%d pending policy change(s): `fleet policy pending` to review.\n", st.Pending)
		}
		return nil
	}
	return c
}

func printProcs(ps []*store.Process) {
	if len(ps) == 0 {
		return
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	for _, p := range ps {
		var ports []string
		for _, x := range p.Ports {
			ports = append(ports, strconv.Itoa(int(x)))
		}
		flags := ""
		if p.Critical {
			flags = " CRITICAL"
		}
		fmt.Fprintf(tw, "    pid %d\t%s\tports %s\t%s%s\n", p.PID, trunc(p.Cmdline, 60), orDash(strings.Join(ports, ",")),
			p.Purpose, flags)
	}
	tw.Flush()
}

func whoamiCmd() *cobra.Command {
	c := &cobra.Command{Use: "whoami", Short: "Show which agent this shell belongs to"}
	f := addFormat(c)
	c.RunE = func(cmd *cobra.Command, args []string) error {
		a, err := openApp()
		if err != nil {
			return err
		}
		defer a.Close()
		ag := a.Agent()
		if f.json() {
			return printJSON(os.Stdout, ag)
		}
		if ag == nil {
			fmt.Println("Not inside a registered agent session (you are the human).")
			return nil
		}
		fmt.Printf("%s (%s) id %s, pid %d, %s\n", ag.Name, ag.Kind, ag.ID, ag.PID, ag.Cwd)
		return nil
	}
	return c
}

func eventsCmd() *cobra.Command {
	var limit int
	c := &cobra.Command{Use: "events", Short: "Show the audit log (kills, policy changes, denials, registrations)"}
	f := addFormat(c)
	c.Flags().IntVar(&limit, "limit", 40, "number of events")
	c.RunE = func(cmd *cobra.Command, args []string) error {
		a, err := openApp()
		if err != nil {
			return err
		}
		defer a.Close()
		evs, err := a.St.RecentEvents(limit)
		if err != nil {
			return err
		}
		if f.json() {
			return printJSON(os.Stdout, evs)
		}
		for i := len(evs) - 1; i >= 0; i-- {
			e := evs[i]
			fmt.Printf("%s  %-18s %-12s %s\n", e.At.Format("01-02 15:04:05"), e.Kind, shortID(e.AgentID), trunc(e.Detail, 120))
		}
		return nil
	}
	return c
}

func priorityCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "priority <agent> <n>",
		Short: "Set an agent's priority (human only; higher wins preemption in phase 2)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := openApp()
			if err != nil {
				return err
			}
			defer a.Close()
			if !isHumanTerminal(a) {
				return fmt.Errorf("priority can only be set by the user from their own terminal")
			}
			n, err := strconv.Atoi(args[1])
			if err != nil {
				return err
			}
			ag, err := a.St.GetAgent(args[0])
			if err != nil || ag == nil {
				return fmt.Errorf("no agent %q", args[0])
			}
			return a.St.SetPriority(ag.ID, n)
		},
	}
}

func trunc(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func shortID(s string) string {
	if len(s) > 10 {
		return s[:10]
	}
	return orDash(s)
}

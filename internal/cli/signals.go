package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/joejenniges/fleet/internal/app"
	"github.com/joejenniges/fleet/internal/dur"
	"github.com/joejenniges/fleet/internal/store"
)

func signalCmd() *cobra.Command {
	var data, dataFile, to string
	var clear bool
	c := &cobra.Command{
		Use:   "signal <name> [--data \"what is ready and where\"] [--to agent] [--clear]",
		Short: "Fire a named signal; agents waiting on it are woken with --data",
		Long: "Signals are latched: once fired, a signal stays set, so a later `fleet wait <name>` returns at once.\n" +
			"Clear it with --clear when it stops being true (e.g. the server it announced was stopped).\n" +
			"--data is the answer the waiting agent receives: paths, URLs, ports, versions.",
		Example: `  fleet signal api:schema-published --data "OpenAPI spec written to api/openapi.yaml (v2.3)"
  fleet signal dev-server:web --data "http://localhost:5173"
  fleet signal dev-server:web --clear`,
		Args: cobra.ExactArgs(1),
	}
	f := addFormat(c)
	c.Flags().StringVar(&data, "data", "", "payload for the waiting agents")
	c.Flags().StringVar(&dataFile, "data-file", "", "read the payload from a file")
	c.Flags().StringVar(&to, "to", "", "only satisfy this agent's waits (name or id)")
	c.Flags().BoolVar(&clear, "clear", false, "un-set the signal instead of firing it")
	c.RunE = func(cmd *cobra.Command, args []string) error {
		a, err := openApp()
		if err != nil {
			return err
		}
		defer a.Close()
		name := args[0]
		if clear {
			if err := app.ValidSignalName(name); err != nil {
				return err
			}
			n, err := a.St.ClearSignal(name)
			if err != nil {
				return err
			}
			a.St.LogEvent(a.AgentID(), "signal.cleared", map[string]any{"signal": name})
			if f.json() {
				return printJSON(os.Stdout, map[string]any{"signal": name, "cleared": n})
			}
			fmt.Printf("signal `%s` cleared\n", name)
			return nil
		}
		if dataFile != "" {
			b, err := os.ReadFile(dataFile)
			if err != nil {
				return err
			}
			data = string(b)
		}
		sg, woken, err := a.FireSignal(name, data, to)
		if err != nil {
			return err
		}
		var names []string
		for _, w := range woken {
			n := "the user"
			if w.AgentID != "" {
				n = w.AgentID
				if ag, _ := a.St.GetAgent(w.AgentID); ag != nil {
					n = ag.Name
				}
			}
			names = append(names, n)
		}
		if f.json() {
			return printJSON(os.Stdout, map[string]any{"signal": sg, "woke": names})
		}
		if len(names) == 0 {
			fmt.Printf("signal `%s` set (#%d). Nobody is waiting yet; it stays set, so a later `fleet wait %s` returns at once "+
				"(un-set it with --clear).\n", name, sg.ID, name)
		} else {
			fmt.Printf("signal `%s` fired (#%d); satisfied the wait of: %s\n", name, sg.ID, strings.Join(names, ", "))
		}
		return nil
	}
	return c
}

func waitCmd() *cobra.Command {
	var in app.WaitInput
	var timeout string
	var cancel bool
	c := &cobra.Command{
		Use:   "wait <name> --desc \"what you need\" [--timeout 110s | --detach] [--fresh]",
		Short: "Wait for a named signal; other agents see --desc and can provide it",
		Long: "Registers that you need a signal and announces it, with --desc, to every other agent.\n\n" +
			"Blocking (default): returns when the signal arrives (exit 0, prints its data), or after --timeout\n" +
			"(exit 3). On timeout the wait stays registered: run the same command again, or end your turn and\n" +
			"fleet will wake you when it arrives. Keep --timeout below your shell tool's own timeout.\n\n" +
			"--detach: register and return immediately; end your turn and fleet's Stop hook wakes you.\n\n" +
			"Exit codes: 0 signal received, 3 timed out (still waiting), 4 wait cancelled.",
		Example: `  fleet wait api:schema-published --desc "OpenAPI spec for the orders API written to disk"
  fleet wait db:migrated --desc "orders DB migrated to the 2026-10 schema" --detach
  fleet wait api:schema-published --cancel`,
		Args: cobra.ExactArgs(1),
	}
	f := addFormat(c)
	c.Flags().StringVar(&in.Description, "desc", "", "what you need, specific enough for another agent to recognize (required)")
	c.Flags().StringVar(&timeout, "timeout", "110s", "how long to block (ignored with --detach)")
	c.Flags().BoolVar(&in.Detach, "detach", false, "register and return; fleet wakes you after you end your turn")
	c.Flags().BoolVar(&in.Fresh, "fresh", false, "ignore signals sent before this wait was registered")
	c.Flags().BoolVar(&cancel, "cancel", false, "cancel your open wait on this signal")
	c.RunE = func(cmd *cobra.Command, args []string) error {
		a, err := openApp()
		if err != nil {
			return err
		}
		defer a.Close()
		in.Signal = args[0]
		if cancel {
			id := a.AgentID()
			if id == "" {
				return errors.New("--cancel only works inside an agent session")
			}
			n, err := a.St.CancelWaits(id, in.Signal)
			if err != nil {
				return err
			}
			fmt.Printf("cancelled %d wait(s) on `%s`\n", n, in.Signal)
			return nil
		}
		to, err := dur.Parse(timeout)
		if err != nil {
			return err
		}
		w, _, err := a.RegisterWait(in)
		if err != nil {
			return err
		}
		if in.Detach {
			// If it is already satisfied, deliver now rather than making the agent sleep.
			if d, _ := a.St.DeliverWait(w.ID); d != nil {
				return printDelivery(f, a, d)
			}
			if f.json() {
				return printJSON(os.Stdout, map[string]any{"status": "registered", "wait": w})
			}
			fmt.Printf("Registered wait #%d on `%s`; other agents have been told what you need. End your turn now: fleet "+
				"will wake you when it is signaled.\n", w.ID, w.Signal)
			return nil
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()
		d, err := a.AwaitWait(ctx, w.ID, to)
		switch {
		case errors.Is(err, app.ErrWaitCancelled):
			fmt.Fprintf(os.Stderr, "fleet: the wait on `%s` was cancelled\n", w.Signal)
			return exitCode(4)
		case errors.Is(err, context.Canceled):
			fmt.Fprintf(os.Stderr, "fleet: interrupted; the wait on `%s` stays registered\n", w.Signal)
			return exitCode(130)
		case err != nil:
			return err
		case d == nil:
			if f.json() {
				_ = printJSON(os.Stdout, map[string]any{"status": "timeout", "wait": w})
			}
			fmt.Fprintf(os.Stderr, "fleet: `%s` has not been signaled yet (waited %s). The wait stays registered and other "+
				"agents can see it. Run the same command again to keep waiting, or end your turn and fleet will wake you "+
				"when it arrives.\n", w.Signal, to)
			return exitCode(3)
		}
		return printDeliveryD(f, d)
	}
	return c
}

func printDelivery(f *formatFlag, a *app.App, w *store.Wait) error {
	sg, _ := a.St.GetSignal(w.SatisfiedBy)
	d := &app.Delivery{Wait: w, Signal: sg, FromName: "the user"}
	if sg != nil && sg.From != "" {
		d.FromName = sg.From
		if ag, _ := a.St.GetAgent(sg.From); ag != nil {
			d.FromName = ag.Name
		}
	}
	return printDeliveryD(f, d)
}

func printDeliveryD(f *formatFlag, d *app.Delivery) error {
	if f.json() {
		return printJSON(os.Stdout, map[string]any{"status": "signaled", "delivery": d})
	}
	fmt.Print(app.RenderDeliveries([]*app.Delivery{d}))
	return nil
}

func waitsCmd() *cobra.Command {
	var mine, all bool
	c := &cobra.Command{
		Use:   "waits [--mine] [--all]",
		Short: "List open waits: what agents are waiting for, and why",
	}
	f := addFormat(c)
	c.Flags().BoolVar(&mine, "mine", false, "only your own waits")
	c.Flags().BoolVar(&all, "all", false, "include satisfied, delivered and cancelled waits")
	c.RunE = func(cmd *cobra.Command, args []string) error {
		a, err := openApp()
		if err != nil {
			return err
		}
		defer a.Close()
		var views []*app.WaitView
		if all || mine {
			flt := store.WaitFilter{OpenOnly: !all, Limit: 100}
			if mine {
				if flt.AgentID = a.AgentID(); flt.AgentID == "" {
					return errors.New("--mine only works inside an agent session")
				}
			}
			ws, err := a.St.ListWaits(flt)
			if err != nil {
				return err
			}
			for _, w := range ws {
				v := &app.WaitView{Wait: w, AgentName: "the user"}
				if ag, _ := a.St.GetAgent(w.AgentID); ag != nil {
					v.AgentName = ag.Name
				}
				views = append(views, v)
			}
		} else if views, err = a.OpenWaits(""); err != nil {
			return err
		}
		if f.json() {
			return printJSON(os.Stdout, views)
		}
		if len(views) == 0 {
			fmt.Println("Nobody is waiting on a signal.")
			return nil
		}
		now := time.Now()
		tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "SIGNAL\tWAITER\tSINCE\tSTATE\tNEEDS")
		for _, v := range views {
			state := v.Mode
			switch {
			case !v.CancelledAt.IsZero():
				state = "cancelled"
			case !v.DeliveredAt.IsZero():
				state = "delivered"
			case !v.SatisfiedAt.IsZero():
				state = "satisfied"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", v.Signal, v.AgentName, dur.Ago(v.CreatedAt, now), state, trunc(v.Description, 80))
		}
		tw.Flush()
		if a.AllWaiting() {
			fmt.Println("\nWARNING: every active agent is waiting on a signal; nobody may be left to send one.")
		}
		return nil
	}
	return c
}

func signalsCmd() *cobra.Command {
	var limit int
	c := &cobra.Command{Use: "signals", Short: "List recently fired signals"}
	f := addFormat(c)
	c.Flags().IntVar(&limit, "limit", 30, "max signals")
	c.RunE = func(cmd *cobra.Command, args []string) error {
		a, err := openApp()
		if err != nil {
			return err
		}
		defer a.Close()
		sgs, err := a.St.ListSignals(limit)
		if err != nil {
			return err
		}
		if f.json() {
			return printJSON(os.Stdout, sgs)
		}
		now := time.Now()
		tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "#\tSIGNAL\tFROM\tWHEN\tSTATE\tDATA")
		for _, s := range sgs {
			from := "user"
			if s.From != "" {
				from = s.From
				if ag, _ := a.St.GetAgent(s.From); ag != nil {
					from = ag.Name
				}
			}
			state := "set"
			if !s.ClearedAt.IsZero() {
				state = "cleared"
			}
			fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\n", s.ID, s.Name, from, dur.Ago(s.CreatedAt, now), state, trunc(s.Data, 70))
		}
		return tw.Flush()
	}
	return c
}

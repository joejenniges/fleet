package testkit

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/joejenniges/fleet/internal/app"
	"github.com/joejenniges/fleet/internal/hooks"
	"github.com/joejenniges/fleet/internal/store"
)

const (
	sessD = "selfcheck-session-d" // waits
	sessE = "selfcheck-session-e" // signals
	sessF = "selfcheck-session-f" // arrives later
	sessG = "selfcheck-session-g"
)

// appAs opens the app acting as a registered session.
func (s *selfcheck) appAs(session string) *app.App {
	a := s.app()
	if ag, _ := a.St.GetAgent(session); ag != nil {
		a.SetAgent(ag)
	}
	return a
}

// fleetAs runs the fleet binary as an agent session and returns stdout, stderr, exit code.
func (s *selfcheck) fleetAs(session string, args ...string) (string, string, int) {
	cmd := exec.Command(s.bin, args...)
	cmd.Dir = s.repo
	cmd.Env = append(os.Environ(), "FLEET_AGENT_ID="+session)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		code = -1
	}
	return out.String(), errb.String(), code
}

// stopHook runs the Stop hook in-process and returns its exit code and stderr.
func (s *selfcheck) stopHook(session, wake string) (int, string) {
	b, _ := json.Marshal(map[string]any{"session_id": session, "cwd": s.repo, "hook_event_name": "Stop"})
	var out, errb bytes.Buffer
	code := hooks.Main(hooks.Stop, "claude", wake, bytes.NewReader(b), &out, &errb)
	return code, errb.String()
}

func (s *selfcheck) fire(from, name, data, to string) ([]*store.Wait, error) {
	a := s.appAs(from)
	defer a.Close()
	_, woken, err := a.FireSignal(name, data, to)
	return woken, err
}

func (s *selfcheck) register(session, name, desc string, detach bool) (*store.Wait, error) {
	a := s.appAs(session)
	defer a.Close()
	w, _, err := a.RegisterWait(app.WaitInput{Signal: name, Description: desc, Detach: detach})
	return w, err
}

func (s *selfcheck) openWaits(session, name string) int {
	a := s.app()
	defer a.Close()
	ws, _ := a.St.ListWaits(store.WaitFilter{AgentID: session, Signal: name, OpenOnly: true})
	return len(ws)
}

func (s *selfcheck) runSignals() {
	for _, sess := range []string{sessD, sessE} {
		s.hook(hooks.SessionStart, sess, nil)
	}
	const schemaDesc = "OpenAPI schema for the orders API written to disk"

	s.check("A new wait is announced once to other agents, with its description", func() error {
		if _, err := s.register(sessD, "selfcheck:schema", schemaDesc, false); err != nil {
			return err
		}
		_, h := s.hook(hooks.UserPromptSubmit, sessE, nil)
		c := ctxOf(h)
		if !strings.Contains(c, schemaDesc) || !strings.Contains(c, "fleet signal selfcheck:schema") {
			return fmt.Errorf("E was not told about the wait: %q", c)
		}
		if out, _ := s.hook(hooks.UserPromptSubmit, sessE, nil); strings.Contains(out, schemaDesc) {
			return errors.New("announcement delivered twice")
		}
		if out, _ := s.hook(hooks.UserPromptSubmit, sessD, nil); strings.Contains(out, schemaDesc) {
			return errors.New("the waiter was told about its own wait")
		}
		return nil
	})

	s.check("Sessions that start later see open waits at SessionStart", func() error {
		_, h := s.hook(hooks.SessionStart, sessF, nil)
		c := ctxOf(h)
		if !strings.Contains(c, schemaDesc) || !strings.Contains(c, "fleet signal <name>") {
			return fmt.Errorf("open wait missing from session context: %q", c)
		}
		return nil
	})

	s.check("Blocking `fleet wait` returns the data when another agent signals", func() error {
		type res struct {
			out, errs string
			code      int
		}
		done := make(chan res, 1)
		go func() {
			o, e, c := s.fleetAs(sessD, "wait", "selfcheck:blocking", "--desc", "blocking test", "--timeout", "20s", "--format", "json")
			done <- res{o, e, c}
		}()
		if !WaitFor(10*time.Second, func() bool { return s.openWaits(sessD, "selfcheck:blocking") == 1 }) {
			return errors.New("wait was never registered")
		}
		if _, err := s.fire(sessE, "selfcheck:blocking", "spec at api/openapi.yaml", ""); err != nil {
			return err
		}
		select {
		case r := <-done:
			if r.code != 0 || !strings.Contains(r.out, "spec at api/openapi.yaml") || !strings.Contains(r.out, `"signaled"`) {
				return fmt.Errorf("exit %d, stdout %q, stderr %q", r.code, r.out, r.errs)
			}
		case <-time.After(10 * time.Second):
			return errors.New("`fleet wait` did not return after the signal")
		}
		if out, _ := s.hook(hooks.UserPromptSubmit, sessD, nil); strings.Contains(out, "api/openapi.yaml") {
			return errors.New("signal delivered again through the inbox")
		}
		return nil
	})

	s.check("A timed-out wait exits 3, stays registered, and re-running reuses it", func() error {
		for i := 0; i < 2; i++ {
			_, errs, code := s.fleetAs(sessD, "wait", "selfcheck:slow", "--desc", "never comes", "--timeout", "1s")
			if code != 3 || !strings.Contains(errs, "stays registered") {
				return fmt.Errorf("run %d: exit %d, stderr %q", i+1, code, errs)
			}
		}
		if n := s.openWaits(sessD, "selfcheck:slow"); n != 1 {
			return fmt.Errorf("want 1 open wait, got %d", n)
		}
		return nil
	})

	s.check("Signals are latched (a later wait returns at once); --fresh ignores earlier ones", func() error {
		if _, err := s.fire(sessE, "selfcheck:early", "already done", ""); err != nil {
			return err
		}
		start := time.Now()
		out, errs, code := s.fleetAs(sessD, "wait", "selfcheck:early", "--desc", "latched", "--timeout", "10s")
		if code != 0 || !strings.Contains(out, "already done") || time.Since(start) > 5*time.Second {
			return fmt.Errorf("latched wait: exit %d after %s, stdout %q, stderr %q", code, time.Since(start), out, errs)
		}
		if _, err := s.fire(sessE, "selfcheck:fresh", "old news", ""); err != nil {
			return err
		}
		if _, _, code := s.fleetAs(sessD, "wait", "selfcheck:fresh", "--desc", "fresh only", "--fresh", "--timeout", "1s"); code != 3 {
			return fmt.Errorf("--fresh wait should time out, got exit %d", code)
		}
		return nil
	})

	s.check("Targeted signals (--to) only satisfy the target's wait", func() error {
		for _, sess := range []string{sessD, sessE} {
			if _, err := s.register(sess, "selfcheck:targeted", "targeted test", false); err != nil {
				return err
			}
		}
		woken, err := s.fire(sessF, "selfcheck:targeted", "for E only", sessE)
		if err != nil {
			return err
		}
		if len(woken) != 1 || woken[0].AgentID != sessE {
			return fmt.Errorf("woke %d wait(s)", len(woken))
		}
		if s.openWaits(sessD, "selfcheck:targeted") != 1 {
			return errors.New("D's wait was satisfied by a signal targeted at E")
		}
		return nil
	})

	s.check("A signal arriving mid-turn is delivered through the hooks exactly once", func() error {
		if _, err := s.register(sessD, "selfcheck:midturn", "mid-turn test", true); err != nil {
			return err
		}
		if _, err := s.fire(sessE, "selfcheck:midturn", "midturn-payload", ""); err != nil {
			return err
		}
		_, h := s.hook(hooks.PostToolUse, sessD, bash("ls"))
		if !strings.Contains(ctxOf(h), "midturn-payload") {
			return fmt.Errorf("not delivered: %q", ctxOf(h))
		}
		if out, _ := s.hook(hooks.PostToolUse, sessD, bash("ls")); strings.Contains(out, "midturn-payload") {
			return errors.New("delivered twice")
		}
		return nil
	})

	s.check("Stop hook (async): exits 0 at once with nothing to wait for; exits 2 with the data when woken", func() error {
		a := s.app()
		_, _ = a.St.CancelWaits(sessD, "")
		a.Close()
		start := time.Now()
		if code, _ := s.stopHook(sessD, hooks.WakeAsync); code != 0 || time.Since(start) > 3*time.Second {
			return fmt.Errorf("idle Stop hook: exit %d after %s", code, time.Since(start))
		}
		if _, err := s.register(sessD, "selfcheck:wake", "dev server for the web app is up", true); err != nil {
			return err
		}
		var mu sync.Mutex
		code, stderr, finished := -1, "", false
		go func() {
			c, e := s.stopHook(sessD, hooks.WakeAsync)
			mu.Lock()
			code, stderr, finished = c, e, true
			mu.Unlock()
		}()
		time.Sleep(1500 * time.Millisecond)
		mu.Lock()
		early := finished
		mu.Unlock()
		if early {
			return fmt.Errorf("Stop hook returned before the signal (exit %d)", code)
		}
		if _, err := s.fire(sessE, "selfcheck:wake", "http://localhost:5173", ""); err != nil {
			return err
		}
		if !WaitFor(5*time.Second, func() bool { mu.Lock(); defer mu.Unlock(); return finished }) {
			return errors.New("Stop hook did not wake after the signal")
		}
		mu.Lock()
		defer mu.Unlock()
		if code != 2 || !strings.Contains(stderr, "http://localhost:5173") {
			return fmt.Errorf("exit %d, stderr %q", code, stderr)
		}
		if c, _ := s.stopHook(sessD, hooks.WakeAsync); c != 0 {
			return fmt.Errorf("next Stop should not wait again (exit %d)", c)
		}
		return nil
	})

	s.check("Stop hook (block mode) holds the turn until the signal, then exits 2", func() error {
		if _, err := s.register(sessD, "selfcheck:block", "block-mode test", true); err != nil {
			return err
		}
		go func() {
			time.Sleep(time.Second)
			_, _ = s.fire(sessE, "selfcheck:block", "block-payload", "")
		}()
		start := time.Now()
		code, stderr := s.stopHook(sessD, hooks.WakeBlock)
		if code != 2 || !strings.Contains(stderr, "block-payload") || time.Since(start) < 800*time.Millisecond {
			return fmt.Errorf("exit %d after %s, stderr %q", code, time.Since(start), stderr)
		}
		return nil
	})

	s.check("SessionEnd cancels the agent's waits, so others stop seeing them", func() error {
		const desc = "this wait must disappear at session end"
		if _, err := s.register(sessD, "selfcheck:end", desc, true); err != nil {
			return err
		}
		s.hook(hooks.SessionEnd, sessD, map[string]any{"source": "exit"})
		if n := s.openWaits(sessD, "selfcheck:end"); n != 0 {
			return fmt.Errorf("%d open wait(s) remain", n)
		}
		_, h := s.hook(hooks.SessionStart, sessG, nil)
		if strings.Contains(ctxOf(h), desc) {
			return errors.New("a new session still sees the ended agent's wait")
		}
		return nil
	})
}

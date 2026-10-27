package policy

import (
	"path/filepath"
	"testing"
)

func newSet(t *testing.T, user, repo File) *Set {
	t.Helper()
	b := Builtin()
	if err := b.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := user.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := repo.Validate(); err != nil {
		t.Fatal(err)
	}
	return &Set{Levels: []*Level{
		{Name: LevelBuiltin, File: b, Exists: true},
		{Name: LevelGlobal},
		{Name: LevelUser, File: user, Exists: true},
		{Name: LevelRepo, File: repo, Exists: true},
	}}
}

func userFile() File {
	return File{Rules: []Rule{
		{ID: "node-dev", Name: []string{"node", "vite"}, Action: Kill, Grace: "3s"},
		{ID: "oops-sqlcmd", Name: []string{"sqlcmd"}, Action: Kill},
	}}
}

func TestNormName(t *testing.T) {
	for in, want := range map[string]string{
		`C:\Program Files\nodejs\node.EXE`: "node",
		"/usr/bin/psql":                    "psql",
		"Vite":                             "vite",
	} {
		if got := NormName(in); got != want {
			t.Errorf("NormName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestKillRule(t *testing.T) {
	s := newSet(t, userFile(), File{})
	r := s.Evaluate(Input{Argv0: "node.exe", Cmdline: "node server.js"})
	if r.Final.Action != Kill || r.Final.Grace != "3s" {
		t.Fatalf("got %+v", r.Final)
	}
}

func TestMostRestrictiveWins(t *testing.T) {
	// user says kill sqlcmd, builtin says never: never must win regardless of level order.
	s := newSet(t, userFile(), File{})
	r := s.Evaluate(Input{Argv0: "sqlcmd", Cmdline: "sqlcmd -S . -Q select 1"})
	if r.Final.Action != Never {
		t.Fatalf("got %s", r.Final.Action)
	}
}

func TestUnknownDefaultsToAsk(t *testing.T) {
	s := newSet(t, File{}, File{})
	r := s.Evaluate(Input{Argv0: "mystery", Cmdline: "mystery --go"})
	if r.Final.Action != Ask || !r.Final.Unmatched {
		t.Fatalf("got %+v", r.Final)
	}
}

func TestDefaultsBySpecificity(t *testing.T) {
	s := newSet(t, File{}, File{Defaults: Defaults{Unknown: Kill, Grace: "2s"}})
	r := s.Evaluate(Input{Argv0: "mystery", Cmdline: "mystery"})
	if r.Final.Action != Kill || r.Final.Grace != "2s" {
		t.Fatalf("repo defaults should override builtin: %+v", r.Final)
	}
}

func TestCriticalMark(t *testing.T) {
	s := newSet(t, userFile(), File{})
	r := s.Evaluate(Input{Argv0: "node", Cmdline: "node x.js", Critical: true})
	if r.Final.Action != Never {
		t.Fatalf("critical should force never: %s", r.Final.Action)
	}
}

func TestMigrationRunningAsNodeIsProtected(t *testing.T) {
	s := newSet(t, userFile(), File{})
	target := Input{PID: 20, Argv0: "node", Cmdline: "node node_modules/.bin/prisma generate"}
	ancestors := []Input{{PID: 10, Argv0: "npm", Cmdline: "npm run db:migrate"}}
	r := s.EvaluateLineage(target, ancestors, nil)
	if r.Final.Action != Never || len(r.Final.Lineage) != 1 {
		t.Fatalf("ancestor migration should protect child: %+v", r.Final)
	}
}

func TestDescendantRaisesDecision(t *testing.T) {
	s := newSet(t, userFile(), File{})
	target := Input{PID: 10, Argv0: "node", Cmdline: "node runner.js"}
	desc := []Input{{PID: 11, Argv0: "psql", Cmdline: "psql -f seed.sql"}}
	r := s.EvaluateLineage(target, nil, desc)
	if r.Final.Action != Never {
		t.Fatalf("killing the tree would kill psql: %+v", r.Final)
	}
}

func TestAncestorAskDoesNotProtect(t *testing.T) {
	s := newSet(t, userFile(), File{})
	target := Input{PID: 20, Argv0: "vite", Cmdline: "vite"}
	ancestors := []Input{{PID: 10, Argv0: "mystery-wrapper", Cmdline: "mystery-wrapper"}}
	r := s.EvaluateLineage(target, ancestors, nil)
	if r.Final.Action != Kill {
		t.Fatalf("only `never` ancestors protect: %+v", r.Final)
	}
}

func TestClassify(t *testing.T) {
	s := newSet(t, userFile(), File{})
	cur := s.LevelFile(LevelUser)
	cases := []struct {
		name string
		c    Change
		want string
	}{
		{"add kill", Change{Op: OpAddRule, Level: LevelUser, Rule: &Rule{ID: "x", Name: []string{"x"}, Action: Kill}}, ClassPermissive},
		{"add never", Change{Op: OpAddRule, Level: LevelUser, Rule: &Rule{ID: "x", Name: []string{"x"}, Action: Never}}, ClassRestrictive},
		{"add ask", Change{Op: OpAddRule, Level: LevelUser, Rule: &Rule{ID: "x", Name: []string{"x"}, Action: Ask}}, ClassRestrictive},
		{"remove kill", Change{Op: OpRemoveRule, Level: LevelUser, RuleID: "node-dev"}, ClassRestrictive},
		{"unknown->kill", Change{Op: OpSetDefault, Level: LevelUser, Defaults: &Defaults{Unknown: Kill}}, ClassPermissive},
		{"unknown->never", Change{Op: OpSetDefault, Level: LevelUser, Defaults: &Defaults{Unknown: Never}}, ClassRestrictive},
		{"grace up", Change{Op: OpSetDefault, Level: LevelUser, Defaults: &Defaults{Grace: "30s"}}, ClassRestrictive},
		{"grace down", Change{Op: OpSetDefault, Level: LevelUser, Defaults: &Defaults{Grace: "1s"}}, ClassPermissive},
		{"timeout kill", Change{Op: OpSetDefault, Level: LevelUser, Defaults: &Defaults{OnTimeout: TimeoutKill}}, ClassPermissive},
	}
	for _, tc := range cases {
		got, err := Classify(s, cur, tc.c)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got != tc.want {
			t.Errorf("%s: got %s want %s", tc.name, got, tc.want)
		}
	}
	if _, err := Classify(s, cur, Change{Op: OpRemoveRule, Level: LevelUser, RuleID: "builtin-db-cli"}); err == nil {
		t.Error("removing a builtin rule must fail")
	}
}

func TestApplyAndRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "policy.toml")
	f, err := Apply(File{}, Change{Op: OpAddRule, Level: LevelUser,
		Rule: &Rule{ID: "chrome", Name: []string{"chrome"}, ArgsContains: "chrome-devtools-mcp", Action: Kill}})
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(p, f); err != nil {
		t.Fatal(err)
	}
	back, err := ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(back.Rules) != 1 || back.Rules[0].ID != "chrome" {
		t.Fatalf("round trip: %+v", back)
	}
	if _, err := Apply(back, Change{Op: OpAddRule, Level: LevelUser, Rule: &Rule{ID: "chrome", Name: []string{"c"}, Action: Ask}}); err == nil {
		t.Error("duplicate id must fail")
	}
}

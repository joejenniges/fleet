package cmdtext

import (
	"strings"
	"testing"
)

func TestNormalize(t *testing.T) {
	r, _ := NewRedactor(nil)
	cases := map[string]string{
		"dotnet   ef database update":                       "dotnet ef database update",
		"fleet run --critical -- dotnet ef database update": "dotnet ef database update",
		"cd src/api && dotnet test":                         "dotnet test",
		"FOO=1 NODE_ENV=test npm test":                      "npm test",
		`C:\tools\Node.EXE server.js`:                       "node server.js",
		`git commit -m "two words"`:                         `git commit -m "two words"`,
		"npm ci && npm test":                                "npm ci && npm test",
	}
	for in, want := range cases {
		if got := Normalize(in, r); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTokenPrefix(t *testing.T) {
	if !TokenPrefix("dotnet ef upgrade --verbose", "dotnet ef upgrade") {
		t.Error("expected prefix match")
	}
	if TokenPrefix("dotnet ef upgrader", "dotnet ef upgrade") {
		t.Error("token boundary must be respected")
	}
}

func TestSegments(t *testing.T) {
	got := Segments(`cd a && echo "x && y" | grep x; ls || true`)
	want := []string{"cd a", `echo "x && y"`, "grep x", "ls", "true"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %q", got)
	}
}

func TestIsFleetRun(t *testing.T) {
	if !IsFleetRun("fleet run -- ls") || IsFleetRun("fleet status") {
		t.Fatal("IsFleetRun")
	}
}

func TestRedact(t *testing.T) {
	r, _ := NewRedactor([]string{`internal-[0-9]{6}`})
	cases := map[string]string{
		"sqlcmd -S . -U sa -P hunter2 -Q select":                         "sqlcmd -S . -U sa -P *** -Q select",
		"mysql -uroot -psecret db":                                       "mysql -uroot -p*** db",
		"grep -P 'a+b' file":                                             "grep -P 'a+b' file",
		"psql postgres://app:s3cr3t@localhost/db":                        "psql postgres://app:***@localhost/db",
		"Server=.;Database=x;User Id=sa;Password=Pa55;":                  "Server=.;Database=x;User Id=sa;Password=***;",
		"GITHUB_TOKEN=abc123 gh pr list":                                 "GITHUB_TOKEN=*** gh pr list",
		"curl -H 'Authorization: Bearer eyJabc.def' https://x":           "curl -H 'Authorization: Bearer ***' https://x",
		"deploy --token=abcd1234":                                        "deploy --token=***",
		"key ghp_abcdefghijklmnopqrstuvwxyz0123 and internal-123456 end": "key *** and *** end",
	}
	for in, want := range cases {
		if got := r.Command(in); got != want {
			t.Errorf("Command(%q)\n got  %q\n want %q", in, got, want)
		}
	}
}

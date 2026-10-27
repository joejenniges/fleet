// Package cmdtext tokenizes, normalizes and redacts command lines so that the same
// command recorded by `fleet run`, by a hook, or typed into `fleet docs add` matches.
package cmdtext

import (
	"strings"
)

// Tokenize splits a command line shell-style, honoring single/double quotes. A
// backslash escapes only a following space, quote or backslash, so Windows paths
// like C:\tools\node.exe survive intact. Nothing is expanded.
func Tokenize(s string) []string {
	var out []string
	var cur strings.Builder
	inTok := false
	var quote rune
	rs := []rune(s)
	escapable := func(i int) bool {
		if i+1 >= len(rs) {
			return false
		}
		switch rs[i+1] {
		case ' ', '\t', '"', '\'', '\\':
			return true
		}
		return false
	}
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else if r == '\\' && quote == '"' && i+1 < len(rs) && (rs[i+1] == '"' || rs[i+1] == '\\') {
				i++
				cur.WriteRune(rs[i])
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote, inTok = r, true
		case r == '\\' && escapable(i):
			i++
			cur.WriteRune(rs[i])
			inTok = true
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			if inTok {
				out = append(out, cur.String())
				cur.Reset()
				inTok = false
			}
		default:
			cur.WriteRune(r)
			inTok = true
		}
	}
	if inTok {
		out = append(out, cur.String())
	}
	return out
}

// Segments splits a shell command on control operators (&&, ||, ;, |, newline) outside
// quotes. Each segment is a single simple command (best effort; no subshell parsing).
func Segments(s string) []string {
	var out []string
	var cur strings.Builder
	var quote rune
	rs := []rune(s)
	flush := func() {
		if t := strings.TrimSpace(cur.String()); t != "" {
			out = append(out, t)
		}
		cur.Reset()
	}
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		if quote != 0 {
			cur.WriteRune(r)
			if r == quote {
				quote = 0
			}
			continue
		}
		switch r {
		case '\'', '"':
			quote = r
			cur.WriteRune(r)
		case ';', '\n':
			flush()
		case '&', '|':
			flush()
			if i+1 < len(rs) && rs[i+1] == r {
				i++
			}
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return out
}

// Argv0 returns the normalized program name of a command: basename, lower-cased,
// .exe stripped.
func Argv0(cmd string) string {
	t := Tokenize(cmd)
	t = stripPrefixes(t)
	if len(t) == 0 {
		return ""
	}
	return normProg(t[0])
}

func normProg(s string) string {
	if i := strings.LastIndexAny(s, `/\`); i >= 0 {
		s = s[i+1:]
	}
	return strings.TrimSuffix(strings.ToLower(s), ".exe")
}

// stripPrefixes drops env assignments and a leading `fleet run [flags] --`.
func stripPrefixes(t []string) []string {
	for len(t) > 0 && isEnvAssign(t[0]) {
		t = t[1:]
	}
	if len(t) >= 2 && normProg(t[0]) == "fleet" && t[1] == "run" {
		for i := 2; i < len(t); i++ {
			if t[i] == "--" {
				return stripPrefixes(t[i+1:])
			}
		}
	}
	return t
}

func isEnvAssign(tok string) bool {
	i := strings.IndexByte(tok, '=')
	if i <= 0 {
		return false
	}
	for j, c := range tok[:i] {
		if !(c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (j > 0 && c >= '0' && c <= '9')) {
			return false
		}
	}
	return true
}

// IsFleetRun reports whether the command is itself `fleet run …` (already recorded).
func IsFleetRun(cmd string) bool {
	t := Tokenize(cmd)
	for len(t) > 0 && isEnvAssign(t[0]) {
		t = t[1:]
	}
	return len(t) >= 2 && normProg(t[0]) == "fleet" && t[1] == "run"
}

// Normalize produces the key used to match runs and docs:
// redacted, env assignments and `fleet run … --` stripped, leading `cd X &&` segments
// dropped, program name normalized, whitespace collapsed.
func Normalize(cmd string, r *Redactor) string {
	if r != nil {
		cmd = r.Command(cmd)
	}
	segs := Segments(cmd)
	// Drop leading `cd somewhere` segments: `cd api && dotnet test` counts as `dotnet test`.
	for len(segs) > 1 {
		t := Tokenize(segs[0])
		if len(t) > 0 && (normProg(t[0]) == "cd" || normProg(t[0]) == "pushd") {
			segs = segs[1:]
			continue
		}
		break
	}
	if len(segs) > 1 {
		// Multi-command lines are kept whole (whitespace-collapsed) so they only match
		// themselves.
		return strings.Join(strings.Fields(strings.Join(segs, " && ")), " ")
	}
	if len(segs) == 0 {
		return ""
	}
	t := stripPrefixes(Tokenize(segs[0]))
	if len(t) == 0 {
		return ""
	}
	t[0] = normProg(t[0])
	for i, tok := range t {
		if strings.ContainsAny(tok, " \t") {
			t[i] = `"` + tok + `"`
		}
	}
	return strings.Join(t, " ")
}

// TokenPrefix reports whether every token of prefix equals the start of cmd's tokens,
// after normalization. Used to match known-bad commands (`dotnet ef upgrade` matches
// `dotnet ef upgrade --verbose`).
func TokenPrefix(cmdNorm, prefixNorm string) bool {
	c, p := Tokenize(cmdNorm), Tokenize(prefixNorm)
	if len(p) == 0 || len(p) > len(c) {
		return false
	}
	for i := range p {
		if c[i] != p[i] {
			return false
		}
	}
	return true
}

// FleetSubcommand returns "<sub> <subsub>" (e.g. "policy approve") when cmd invokes
// fleet, else "". Env assignments are skipped.
func FleetSubcommand(cmd string) string {
	t := Tokenize(cmd)
	for len(t) > 0 && isEnvAssign(t[0]) {
		t = t[1:]
	}
	if len(t) < 2 || normProg(t[0]) != "fleet" {
		return ""
	}
	var parts []string
	for _, tok := range t[1:] {
		if strings.HasPrefix(tok, "-") {
			continue
		}
		parts = append(parts, tok)
		if len(parts) == 2 {
			break
		}
	}
	return strings.Join(parts, " ")
}

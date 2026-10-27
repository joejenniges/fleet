package cmdtext

import (
	"regexp"
	"strings"
)

const mask = "***"

// Redactor removes credentials from commands and output before they are stored.
// It is best-effort: `fleet run --no-record` exists for commands that must never be stored.
type Redactor struct {
	keyed []*regexp.Regexp // group 1 is kept, group 2 is masked
	whole []*regexp.Regexp // whole match is masked
}

var defaultKeyed = []string{
	// --password=x, --token x, -password x ...
	`(?i)(--?(?:password|passwd|pwd|pass|token|secret|client-secret|api[-_]?key|access[-_]?key|auth)(?:=|\s+))("[^"]*"|'[^']*'|\S+)`,
	// Connection strings / query strings: Password=x; pwd=x; AccountKey=x
	`(?i)(\b(?:password|passwd|pwd|token|secret|api[-_]?key|accountkey|sharedaccesskey|access_token|client_secret)\s*=\s*)([^;&\s"']+)`,
	// HTTP auth headers
	`(?i)(authorization:\s*(?:bearer|basic|token)\s+)([^\s'"]+)`,
	// Env assignments: FOO_TOKEN=x
	`(\b[A-Z][A-Z0-9_]*(?:TOKEN|SECRET|PASSWORD|PASSWD|PWD|API_KEY|APIKEY|ACCESS_KEY|PRIVATE_KEY)=)("[^"]*"|'[^']*'|\S+)`,
	// URL credentials: scheme://user:pass@host
	`(\b[a-z][a-z0-9+.-]*://[^:/\s@]+:)([^@\s]+)(?:@)`,
}

var defaultWhole = []string{
	`\bAKIA[0-9A-Z]{16}\b`,
	`\bgh[pousr]_[A-Za-z0-9]{20,}\b`,
	`\bgithub_pat_[A-Za-z0-9_]{20,}\b`,
	`\bsk-[A-Za-z0-9_-]{20,}\b`,
	`\bxox[abpr]-[A-Za-z0-9-]{10,}\b`,
	`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`,
}

// NewRedactor builds a redactor; extra are additional whole-match regexes from config.
// Invalid extra patterns are returned as errors but do not disable the defaults.
func NewRedactor(extra []string) (*Redactor, []error) {
	r := &Redactor{}
	for _, p := range defaultKeyed {
		r.keyed = append(r.keyed, regexp.MustCompile(p))
	}
	for _, p := range defaultWhole {
		r.whole = append(r.whole, regexp.MustCompile(p))
	}
	var errs []error
	for _, p := range extra {
		re, err := regexp.Compile(p)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		r.whole = append(r.whole, re)
	}
	return r, errs
}

// Text redacts free text (command output).
func (r *Redactor) Text(s string) string {
	if r == nil {
		return s
	}
	for _, re := range r.keyed {
		s = re.ReplaceAllStringFunc(s, func(m string) string {
			sub := re.FindStringSubmatchIndex(m)
			if len(sub) < 6 || sub[4] < 0 {
				return mask
			}
			return m[:sub[4]] + mask + m[sub[5]:]
		})
	}
	for _, re := range r.whole {
		s = re.ReplaceAllString(s, mask)
	}
	return s
}

// Tools whose single-letter flags carry passwords. Only applied to these programs so
// that e.g. `grep -P <regex>` is left alone.
var passwordFlags = map[string][]string{
	"sqlcmd":    {"-P"},
	"bcp":       {"-P"},
	"osql":      {"-P"},
	"mysql":     {"-p"},
	"mysqldump": {"-p"},
	"mariadb":   {"-p"},
	"redis-cli": {"-a"},
}

// Command redacts a command line: Text rules plus program-aware flag rules.
func (r *Redactor) Command(cmd string) string {
	cmd = r.Text(cmd)
	segs := Segments(cmd)
	if len(segs) == 0 {
		return cmd
	}
	for _, seg := range segs {
		toks := Tokenize(seg)
		toks = stripPrefixes(toks)
		if len(toks) == 0 {
			continue
		}
		flags := passwordFlags[normProg(toks[0])]
		if len(flags) == 0 {
			continue
		}
		for i := 1; i < len(toks); i++ {
			for _, f := range flags {
				switch {
				case toks[i] == f && i+1 < len(toks):
					cmd = replaceToken(cmd, toks[i+1])
				case strings.HasPrefix(toks[i], f) && len(toks[i]) > len(f):
					// attached form: -pSecret
					cmd = replaceToken(cmd, toks[i][len(f):])
				}
			}
		}
	}
	return cmd
}

func replaceToken(cmd, secret string) string {
	if secret == "" || secret == mask {
		return cmd
	}
	for _, q := range []string{`"` + secret + `"`, `'` + secret + `'`, secret} {
		if strings.Contains(cmd, q) {
			return strings.ReplaceAll(cmd, q, mask)
		}
	}
	return cmd
}

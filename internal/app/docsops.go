package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/joejenniges/fleet/internal/cmdtext"
	"github.com/joejenniges/fleet/internal/store"
)

type AddDocInput struct {
	Kind    string `json:"kind"`
	Text    string `json:"text"` // the command, or the title for behavior/note
	Note    string `json:"note,omitempty"`
	Scope   string `json:"scope,omitempty"` // repo | machine (default: repo inside a repo)
	FromRun int64  `json:"from_run,omitempty"`
	Fixes   int64  `json:"fixes,omitempty"` // known-bad doc this valid-command fixes
}

type AddDocResult struct {
	Doc      *store.Doc `json:"doc"`
	Existing bool       `json:"existing"`
	Message  string     `json:"message"`
}

func isCommandKind(k string) bool {
	return k == store.KindValid || k == store.KindBad || k == store.KindRecipe
}

// AddDoc records a knowledge entry. Command entries are tied to run evidence when
// possible; entries without evidence are kept but shown as unverified.
func (a *App) AddDoc(in AddDocInput) (*AddDocResult, error) {
	if !slices.Contains(store.DocKinds, in.Kind) {
		return nil, fmt.Errorf("kind must be one of %v", store.DocKinds)
	}
	in.Text = strings.TrimSpace(in.Text)
	if in.Text == "" {
		return nil, errors.New("text is required")
	}
	if in.Scope == "" {
		in.Scope = "machine"
		if a.Repo != "" {
			in.Scope = "repo"
		}
	}
	if in.Scope != "repo" && in.Scope != "machine" {
		return nil, errors.New("scope must be repo or machine")
	}
	if in.Scope == "repo" && a.Repo == "" {
		return nil, errors.New("scope=repo needs a git repository; use --scope machine")
	}
	d := &store.Doc{Kind: in.Kind, Scope: in.Scope, CreatedBy: a.AgentID()}
	if in.Scope == "repo" {
		d.RepoRoot = a.Repo
	}
	if isCommandKind(in.Kind) {
		d.Command = a.Red.Command(in.Text)
		d.NormCmd = cmdtext.Normalize(in.Text, a.Red)
		d.Body = in.Note
	} else {
		d.Title, d.Body = in.Text, in.Note
	}

	// Duplicate? Attach new evidence to the existing entry instead.
	if d.NormCmd != "" {
		if ex, _ := a.St.FindDoc(in.Kind, d.NormCmd, a.Repo); ex != nil {
			msg := fmt.Sprintf("already documented as #%d", ex.ID)
			if in.FromRun != 0 {
				if err := a.checkEvidence(ex, in.FromRun); err != nil {
					return nil, err
				}
				_ = a.St.MarkVerified(ex.ID, in.FromRun)
				msg += "; evidence attached"
			}
			if in.Fixes != 0 {
				if err := a.linkFix(in.Fixes, ex); err != nil {
					return nil, err
				}
			}
			ex, _ = a.St.GetDoc(ex.ID)
			return &AddDocResult{Doc: ex, Existing: true, Message: msg}, nil
		}
	}

	// Evidence.
	if in.FromRun != 0 {
		if err := a.checkEvidence(d, in.FromRun); err != nil {
			return nil, err
		}
		d.EvidenceRunID, d.Verified = in.FromRun, true
	} else if d.NormCmd != "" && in.Kind != store.KindRecipe {
		if r := a.findEvidence(d); r != nil {
			d.EvidenceRunID, d.Verified = r.ID, true
		}
	}
	if _, err := a.St.AddDoc(d); err != nil {
		return nil, err
	}
	if in.Fixes != 0 {
		if err := a.linkFix(in.Fixes, d); err != nil {
			return nil, err
		}
	}
	a.St.LogEvent(a.AgentID(), "docs.added", map[string]any{"id": d.ID, "kind": d.Kind, "label": d.Label()})
	msg := fmt.Sprintf("added #%d", d.ID)
	if !d.Verified && isCommandKind(d.Kind) {
		msg += " (unverified: no matching run found; run it through `fleet run -- …` and re-add with --from-run to verify)"
	}
	d, _ = a.St.GetDoc(d.ID)
	return &AddDocResult{Doc: d, Message: msg}, nil
}

func (a *App) checkEvidence(d *store.Doc, runID int64) error {
	r, err := a.St.GetRun(runID)
	if err != nil {
		return err
	}
	if r == nil {
		return fmt.Errorf("no run #%d", runID)
	}
	if d.NormCmd != "" && r.NormCmd != d.NormCmd && !cmdtext.TokenPrefix(r.NormCmd, d.NormCmd) {
		return fmt.Errorf("run #%d was %q, which does not match %q", runID, r.Command, d.Command)
	}
	switch d.Kind {
	case store.KindValid:
		if !r.Succeeded() {
			return fmt.Errorf("run #%d did not succeed (exit %s); a valid-command needs a successful run", runID, exitStr(r.ExitCode))
		}
	case store.KindBad:
		if r.Succeeded() {
			return fmt.Errorf("run #%d succeeded; a known-bad entry needs a failed run", runID)
		}
	}
	return nil
}

func (a *App) findEvidence(d *store.Doc) *store.Run {
	runs, _ := a.St.ListRuns(store.RunFilter{NormCmd: d.NormCmd, RepoRoot: d.RepoRoot, Limit: 20})
	for _, r := range runs {
		if r.ExitCode == nil {
			continue
		}
		if (d.Kind == store.KindValid && r.Succeeded()) || (d.Kind == store.KindBad && !r.Succeeded()) {
			return r
		}
	}
	return nil
}

func (a *App) linkFix(badID int64, good *store.Doc) error {
	bad, err := a.St.GetDoc(badID)
	if err != nil {
		return err
	}
	if bad == nil || bad.Kind != store.KindBad {
		return fmt.Errorf("#%d is not a known-bad entry", badID)
	}
	if good.Kind != store.KindValid && good.Kind != store.KindRecipe {
		return errors.New("--fixes is only valid on valid-command or recipe entries")
	}
	return a.St.LinkFix(badID, good.ID)
}

func exitStr(p *int) string {
	if p == nil {
		return "unknown"
	}
	return fmt.Sprint(*p)
}

// WithTally fills d.Tally from run history.
func (a *App) WithTally(d *store.Doc) *store.Doc {
	if d.NormCmd == "" {
		return d
	}
	repo := ""
	if d.Scope == "repo" {
		repo = d.RepoRoot
	}
	if t, err := a.St.TallyFor(d.NormCmd, repo); err == nil {
		d.Tally = &t
	}
	return d
}

// ---- instruction files & top commands ----

// InstructionFile is an agent instructions file that may already document commands.
type InstructionFile struct {
	Family string `json:"family"` // CLAUDE.md | AGENTS.md | GEMINI.md
	Path   string `json:"path"`
}

// InstructionFamilies returns the instructions-file families an agent kind reads.
func InstructionFamilies(kind string) []string {
	switch kind {
	case "claude":
		return []string{"CLAUDE.md"}
	case "codex", "cursor":
		return []string{"AGENTS.md"}
	case "gemini":
		return []string{"GEMINI.md"}
	}
	return []string{"AGENTS.md", "CLAUDE.md"}
}

// InstructionFiles lists existing instruction files for kind at repo/cwd/home.
func InstructionFiles(kind, repo, cwd string) []InstructionFile {
	home, _ := os.UserHomeDir()
	var cands []InstructionFile
	add := func(fam string, p ...string) {
		cands = append(cands, InstructionFile{Family: fam, Path: filepath.Join(p...)})
	}
	for _, fam := range InstructionFamilies(kind) {
		base := strings.TrimSuffix(fam, ".md")
		for _, dir := range uniq(repo, cwd) {
			add(fam, dir, fam)
			add(fam, dir, base+".local.md")
		}
		switch fam {
		case "CLAUDE.md":
			if repo != "" {
				add(fam, repo, ".claude", "CLAUDE.md")
			}
			add(fam, home, ".claude", "CLAUDE.md")
		case "AGENTS.md":
			add(fam, home, ".codex", "AGENTS.md")
		case "GEMINI.md":
			add(fam, home, ".gemini", "GEMINI.md")
		}
	}
	var out []InstructionFile
	seen := map[string]bool{}
	for _, c := range cands {
		if seen[c.Path] {
			continue
		}
		seen[c.Path] = true
		if st, err := os.Stat(c.Path); err == nil && !st.IsDir() {
			out = append(out, c)
		}
	}
	return out
}

func uniq(xs ...string) []string {
	var out []string
	for _, x := range xs {
		if x != "" && !slices.Contains(out, x) {
			out = append(out, x)
		}
	}
	return out
}

type HiddenDoc struct {
	Doc    *store.Doc `json:"doc"`
	Reason string     `json:"reason"` // "found in CLAUDE.md" | "marked known in CLAUDE.md until …"
}

type TopResult struct {
	Top    []*store.Doc `json:"top"`
	Hidden []HiddenDoc  `json:"hidden,omitempty"`
}

// TopDocs returns the most-executed documented commands for this repo, skipping ones
// the agent's instructions file already covers (found verbatim, or marked known).
func (a *App) TopDocs(kind string, n int) (*TopResult, error) {
	if n <= 0 {
		n = a.Cfg.Docs.TopN
	}
	since := a.St.Now().Add(-a.Cfg.Docs.TopWindow.Duration)
	docs, err := a.St.DocsByExecutions(a.Repo, since)
	if err != nil {
		return nil, err
	}
	fams := InstructionFamilies(kind)
	marks, _ := a.St.ActiveKnownMarks(a.Repo, fams)
	var content strings.Builder
	for _, f := range InstructionFiles(kind, a.Repo, a.Cwd) {
		if b, err := os.ReadFile(f.Path); err == nil && len(b) < 1<<20 {
			content.Write(b)
			content.WriteByte('\n')
		}
	}
	text := content.String()
	res := &TopResult{}
	for _, d := range docs {
		switch {
		case marks[d.ID]:
			res.Hidden = append(res.Hidden, HiddenDoc{Doc: d, Reason: "marked known in " + strings.Join(fams, "/")})
		case text != "" && d.Command != "" && strings.Contains(text, d.Command):
			res.Hidden = append(res.Hidden, HiddenDoc{Doc: d, Reason: "found in " + strings.Join(fams, "/")})
		case len(res.Top) < n:
			res.Top = append(res.Top, a.WithTally(d))
		}
	}
	return res, nil
}

// MarkKnown records that the agent's instructions file already documents a doc.
func (a *App) MarkKnown(docID int64, file string) (time.Time, error) {
	d, err := a.St.GetDoc(docID)
	if err != nil {
		return time.Time{}, err
	}
	if d == nil {
		return time.Time{}, fmt.Errorf("no doc #%d", docID)
	}
	if file == "" {
		kind := "other"
		if ag := a.Agent(); ag != nil {
			kind = ag.Kind
		}
		file = InstructionFamilies(kind)[0]
	}
	file = filepath.Base(file)
	ttl := a.Cfg.Docs.KnownMarkTTL.Duration
	if err := a.St.MarkKnown(docID, a.Repo, file, a.AgentID(), ttl); err != nil {
		return time.Time{}, err
	}
	return a.St.Now().Add(ttl), nil
}

// KnownBadMatch is a known-bad entry that a command matches, with its fix if linked.
type KnownBadMatch struct {
	Bad     *store.Doc `json:"bad"`
	Fix     *store.Doc `json:"fix,omitempty"`
	Segment string     `json:"segment"`
}

// MatchKnownBad checks every simple command in a shell line against known-bad entries.
func (a *App) MatchKnownBad(cmd string) (*KnownBadMatch, error) {
	bads, err := a.St.ListDocs(store.DocFilter{RepoRoot: a.Repo, Kinds: []string{store.KindBad}})
	if err != nil || len(bads) == 0 {
		return nil, err
	}
	for _, seg := range cmdtext.Segments(cmd) {
		norm := cmdtext.Normalize(seg, a.Red)
		for _, b := range bads {
			if b.NormCmd != "" && cmdtext.TokenPrefix(norm, b.NormCmd) {
				m := &KnownBadMatch{Bad: b, Segment: seg}
				if b.FixDocID != 0 {
					m.Fix, _ = a.St.GetDoc(b.FixDocID)
				}
				return m, nil
			}
		}
	}
	return nil, nil
}

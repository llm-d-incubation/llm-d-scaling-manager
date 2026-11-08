package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const maxSamples = 4

// ruleStat counts one rule in one path class.
type ruleStat struct {
	rule, class, action, pathClass string
}

// Occurrence is a place in the tree.
type Occurrence struct {
	Path string
	Line int
	Ctx  string
	Text string
}

// Stats collects what a run saw. Plan may visit the same literal more than
// once, so counting stays in one place.
type Stats struct {
	counts       map[ruleStat]int
	samples      map[string][]string
	unclassified []Occurrence
	held         []Occurrence
	idents       map[string]string
	identClass   map[string]string
}

// NewStats returns empty stats.
func NewStats() *Stats {
	return &Stats{
		counts:     map[ruleStat]int{},
		samples:    map[string][]string{},
		idents:     map[string]string{},
		identClass: map[string]string{},
	}
}

// Hit records a claimed span.
func (s *Stats) Hit(rule, class string, effective Action, pathClass, p string, src []byte, span Span, line int) {
	s.counts[ruleStat{rule, class, effective.String(), pathClass}]++
	n, text := lineAt(src, span.Start)
	if line > 0 {
		n += line - 1
	}
	if effective == HoldUntilMoved {
		s.held = append(s.held, Occurrence{p, n, rule, strings.TrimSpace(text)})
	}
	if len(s.samples[rule]) < maxSamples {
		old := string(src[span.Start:span.End])
		sample := fmt.Sprintf("%s:%d `%s`", p, n, old)
		if span.New != old {
			sample += fmt.Sprintf(" -> `%s`", span.New)
		}
		s.samples[rule] = append(s.samples[rule], sample)
	}
}

// Ident records a mapped identifier.
func (s *Stats) Ident(r *IdentRule, name, mapped, pathClass, p string) {
	s.counts[ruleStat{r.Name, r.Class, Rename.String(), pathClass}]++
	if len(s.samples[r.Name]) < maxSamples && s.idents[name] == "" {
		s.samples[r.Name] = append(s.samples[r.Name], fmt.Sprintf("%s `%s` -> `%s`", p, name, mapped))
	}
	s.idents[name] = mapped
	s.identClass[name] = r.Class
}

// Unclassified records an occurrence no rule claimed.
func (s *Stats) Unclassified(p, ctx string, src []byte, start, line int) {
	n, text := lineAt(src, start)
	if line > 0 {
		n += line - 1
	}
	s.unclassified = append(s.unclassified, Occurrence{p, n, ctx, strings.TrimSpace(text)})
}

// UnclassifiedIdent records an identifier no rule maps.
func (s *Stats) UnclassifiedIdent(p, name string, line int) {
	s.unclassified = append(s.unclassified, Occurrence{p, line, "go-ident", name})
}

// UnclassifiedCount is how many occurrences no rule or exclusion covers.
func (s *Stats) UnclassifiedCount() int { return len(s.unclassified) }

var longNameRe = regexp.MustCompile(`scaling-manager[a-z0-9.-]*[a-z0-9]`)

// collectLongNames notes every name in the output long enough to run into a
// Kubernetes limit once a suffix or a namespace is added.
func collectLongNames(into map[string]int, b []byte) {
	for _, m := range longNameRe.FindAll(b, -1) {
		if len(m) >= 48 {
			into[string(m)]++
		}
	}
}

// metricNames reads the metric names out of internal/constants through the
// syntax tree: the value of every string constant that starts with a metric
// prefix.
func metricNames(root string) ([]string, error) {
	dir := filepath.Join(root, "internal", "constants")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	fset := token.NewFileSet()
	for _, ent := range entries {
		if !strings.HasSuffix(ent.Name(), ".go") || strings.HasSuffix(ent.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, ent.Name()), nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			v, err := strconv.Unquote(lit.Value)
			if err == nil && (strings.HasPrefix(v, "wva_") || strings.HasPrefix(v, "llmsm_")) {
				seen[v] = true
			}
			return true
		})
	}
	if len(seen) == 0 {
		return nil, fmt.Errorf("%s has no metric names: an empty list is not a pass", dir)
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	return names, nil
}

// writeReport writes the markdown report.
func writeReport(opts Options, e *Engine, res *Result) error {
	var b bytes.Buffer
	mode := "dry run"
	if opts.Apply {
		mode = "apply"
	}
	fmt.Fprintf(&b, "# Rename report (%s)\n\n", mode)
	fmt.Fprintf(&b, "- module target: `%s`\n- code moved: %t\n- tracked files: %d\n", opts.Targets.Module, opts.Targets.CodeMoved, res.Files)
	fmt.Fprintf(&b, "- files changed: %d\n- paths renamed: %d\n- binary files never edited: %d\n", len(res.Changed), len(res.Renames), len(res.Binary))
	fmt.Fprintf(&b, "- metric names found in internal/constants: %d\n", len(res.Metrics))
	fmt.Fprintf(&b, "- unclassified occurrences: %d\n\n", e.Stats.UnclassifiedCount())

	b.WriteString("## Per rule and path class\n\n| rule | class | action | path class | count |\n|---|---|---|---|---:|\n")
	keys := make([]ruleStat, 0, len(e.Stats.counts))
	for k := range e.Stats.counts {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, c := keys[i], keys[j]
		if a.class != c.class {
			return a.class < c.class
		}
		if a.rule != c.rule {
			return a.rule < c.rule
		}
		return a.pathClass < c.pathClass
	})
	for _, k := range keys {
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %d |\n", k.rule, k.class, k.action, k.pathClass, e.Stats.counts[k])
	}

	b.WriteString("\n## Samples\n")
	names := make([]string, 0, len(e.Stats.samples))
	for n := range e.Stats.samples {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(&b, "\n### %s\n\n", n)
		for _, s := range e.Stats.samples[n] {
			fmt.Fprintf(&b, "- %s\n", s)
		}
	}

	b.WriteString("\n## Held and kept, and why\n\n| rule | action | why |\n|---|---|---|\n")
	for _, r := range e.Rules {
		if r.Action != Rename {
			fmt.Fprintf(&b, "| %s | %s | %s |\n", r.Name, e.effective(&r), r.Why)
		}
	}

	fmt.Fprintf(&b, "\n## Lines held until the code moves (%d)\n\n", len(e.Stats.held))
	for _, o := range e.Stats.held {
		fmt.Fprintf(&b, "- %s:%d [%s] %s\n", o.Path, o.Line, o.Ctx, o.Text)
	}

	b.WriteString("\n## Unclassified\n\n")
	if len(e.Stats.unclassified) == 0 {
		b.WriteString("None.\n")
	}
	for _, o := range e.Stats.unclassified {
		fmt.Fprintf(&b, "- %s:%d [%s] %s\n", o.Path, o.Line, o.Ctx, o.Text)
	}

	res.Sweep.write(&b)

	b.WriteString("\n## Identifier map\n\n")
	ids := make([]string, 0, len(e.Stats.idents))
	for n := range e.Stats.idents {
		ids = append(ids, n)
	}
	sort.Strings(ids)
	for _, n := range ids {
		fmt.Fprintf(&b, "- %s: `%s` -> `%s`\n", e.Stats.identClass[n], n, e.Stats.idents[n])
	}

	b.WriteString("\n## Names at 48 characters or more after the rename\n\n")
	long := make([]string, 0, len(res.LongName))
	for n := range res.LongName {
		long = append(long, n)
	}
	sort.Slice(long, func(i, j int) bool { return len(long[i]) > len(long[j]) })
	for _, n := range long {
		fmt.Fprintf(&b, "- %d chars: `%s`\n", len(n), n)
	}

	b.WriteString("\n## Paths renamed\n\n")
	for _, m := range res.Renames {
		fmt.Fprintf(&b, "- `%s` -> `%s`\n", m.From, m.To)
	}

	b.WriteString("\n## Binary and compressed files, never edited\n\n| path | mentions after decompressing |\n|---|---:|\n")
	for _, f := range res.Binary {
		if f.Mentions > 0 {
			fmt.Fprintf(&b, "| %s | %d |\n", f.Path, f.Mentions)
		}
	}

	b.WriteString("\n## Skipped (symlinks and submodules)\n\n")
	for _, s := range res.Skipped {
		fmt.Fprintf(&b, "- %s\n", s)
	}

	if err := os.MkdirAll(filepath.Dir(opts.Report), 0o755); err != nil {
		return err
	}
	return os.WriteFile(opts.Report, b.Bytes(), 0o644)
}

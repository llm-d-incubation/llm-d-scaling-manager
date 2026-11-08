package main

import (
	"bytes"
	"fmt"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// Action says what a rule does with the text it claims.
type Action int

const (
	// Rename replaces the claimed text.
	Rename Action = iota
	// Keep claims the text and leaves it as it is, on purpose.
	Keep
	// HoldUntilMoved leaves the text alone until --code-moved is given, and
	// renames it then. It is for names that only resolve once the code, the
	// issues and the images live in the new repository.
	HoldUntilMoved
)

func (a Action) String() string {
	switch a {
	case Keep:
		return "keep"
	case HoldUntilMoved:
		return "hold"
	default:
		return "rename"
	}
}

// Where a piece of text sits, which decides the rules that may touch it.
const (
	CtxText    = "text"       // a line of a file that is not Go
	CtxPath    = "path"       // a tracked path
	CtxComment = "go-comment" // a Go comment
	CtxString  = "go-string"  // a Go string or rune literal
	CtxTag     = "go-tag"     // a Go struct tag
	CtxImport  = "go-import"  // a Go import path
)

// Rule is one row of the rules table.
type Rule struct {
	// Name identifies the rule in the report.
	Name string
	// Class groups rules in the report (metric, env, k8s-object, ...).
	Class string
	// Pattern is an RE2 expression. When it has a group named "m", only that
	// group is claimed; the rest is context, which is how a rule says "not
	// preceded by" without a lookbehind.
	Pattern string
	// Replace is the replacement for the claimed text, with ${name} groups as
	// in regexp.Expand. Ignored when Fn is set or the action is Keep.
	Replace string
	// Fn computes the replacement from the claimed text and where it stands.
	Fn func(claimed string, c FnContext) string
	// Action is what happens to the claimed text.
	Action Action
	// Paths limits the rule to paths matching one of these globs (see
	// matchGlob). Empty means every path.
	Paths []string
	// NotPaths excludes paths matching one of these globs.
	NotPaths []string
	// Contexts limits the rule to these contexts. Empty means every context
	// except CtxTag, CtxImport and CtxPath, which a rule has to name.
	Contexts []string
	// Why is printed in the report for keep and hold rules.
	Why string

	re *regexp.Regexp
	m  int
}

// FnContext is what a rule's Fn knows about where its text stands.
type FnContext struct {
	// Before is what precedes the text on its line.
	Before string
	// Prev is the line above, comment marks and spaces off. Empty after a
	// blank line, and at the start of a file or a comment block.
	Prev string
	// Markdown says a # at the start of a line is a heading, not a comment.
	Markdown bool
}

// Heading reports whether the text opens a Markdown heading.
func (c FnContext) Heading() bool {
	return c.Markdown && strings.HasPrefix(c.Before, "#") && strings.Trim(c.Before, "# ") == ""
}

// StartsGoDoc reports whether the text is the first word of a Go comment line.
func (c FnContext) StartsGoDoc() bool {
	return c.Prev == "" && strings.TrimSpace(c.Before) == "//"
}

// IdentRule maps a whole Go identifier. Identifiers are mapped by name, never
// by position, so every use of a name lands on the same new name.
type IdentRule struct {
	Name    string
	Class   string
	Pattern string
	Replace string
	re      *regexp.Regexp
}

// compile prepares the regular expressions of a table. A pattern that does
// not compile is a bug in the table, so it stops the run.
func compileRules(rules []Rule) ([]Rule, error) {
	out := make([]Rule, len(rules))
	seen := map[string]bool{}
	for i, r := range rules {
		if r.Name == "" || seen[r.Name] {
			return nil, fmt.Errorf("rule %d: empty or repeated name %q", i, r.Name)
		}
		seen[r.Name] = true
		re, err := regexp.Compile(r.Pattern)
		if err != nil {
			return nil, fmt.Errorf("rule %s: %w", r.Name, err)
		}
		r.re = re
		r.m = re.SubexpIndex("m")
		out[i] = r
	}
	return out, nil
}

func compileIdentRules(rules []IdentRule) ([]IdentRule, error) {
	out := make([]IdentRule, len(rules))
	for i, r := range rules {
		re, err := regexp.Compile(r.Pattern)
		if err != nil {
			return nil, fmt.Errorf("ident rule %s: %w", r.Name, err)
		}
		r.re = re
		out[i] = r
	}
	return out, nil
}

// matchGlob matches a slash path against a glob in which "**" is any number of
// directories and a trailing "/**" takes everything below a directory.
func matchGlob(glob, p string) bool {
	if strings.HasSuffix(glob, "/**") {
		return strings.HasPrefix(p, strings.TrimSuffix(glob, "**"))
	}
	if rest, ok := strings.CutPrefix(glob, "**/"); ok {
		ok, _ = path.Match(rest, path.Base(p))
		return ok
	}
	ok, _ := path.Match(glob, p)
	return ok
}

func matchAny(globs []string, p string) bool {
	return slices.ContainsFunc(globs, func(g string) bool { return matchGlob(g, p) })
}

// applies reports whether the rule may touch text in this context of this path.
func (r *Rule) applies(p, ctx string) bool {
	if len(r.Contexts) > 0 {
		if !slices.Contains(r.Contexts, ctx) {
			return false
		}
	} else if ctx == CtxTag || ctx == CtxImport || ctx == CtxPath {
		return false
	}
	return r.pathApplies(p)
}

// pathApplies reports whether the rule covers this path, whatever the context.
func (r *Rule) pathApplies(p string) bool {
	if len(r.Paths) > 0 && !matchAny(r.Paths, p) {
		return false
	}
	return !matchAny(r.NotPaths, p)
}

// Span is a stretch of input a rule claimed.
type Span struct {
	Start, End int
	Rule       *Rule
	New        string
}

// Engine runs the rules table.
type Engine struct {
	Rules      []Rule
	Idents     []IdentRule
	CodeMoved  bool
	Stats      *Stats
	residualRe *regexp.Regexp
	// issueRe finds a bare issue number, which needs the table once the code
	// has moved even in a file with no old name in it.
	issueRe *regexp.Regexp
	// prevComment is the comment line before the one being rewritten, in a
	// comment block: RewriteGo sees each comment on its own.
	prevComment string
	markdown    bool
	// verify is verifyGo, as a field so a test can prove it decides the result.
	verify func(p string, before, after []byte) error
}

// NewEngine compiles the tables for a set of targets.
func NewEngine(t Targets, metrics []string) (*Engine, error) {
	rules, err := compileRules(RulesFor(t, metrics))
	if err != nil {
		return nil, err
	}
	idents, err := compileIdentRules(identRules)
	if err != nil {
		return nil, err
	}
	e := &Engine{
		Rules:      rules,
		Idents:     idents,
		CodeMoved:  t.CodeMoved,
		Stats:      NewStats(),
		residualRe: regexp.MustCompile(residualPattern),
		issueRe:    regexp.MustCompile(issueRefPattern),
	}
	e.verify = e.verifyGo
	return e, nil
}

// mentionsOld is a cheap test that saves running the table over a file that
// has nothing to do.
func (e *Engine) mentionsOld(b []byte) bool {
	return e.residualRe.Match(b) || (e.CodeMoved && e.issueRe.Match(b))
}

// RewriteText applies the table to text in one context of one path and returns
// the result. Rules run in table order and a span one rule claimed is out of
// reach of the rules after it, so the keep rows at the top protect what they
// match. Whatever the residual pattern still finds outside any claim is
// recorded as unclassified.
func (e *Engine) RewriteText(p, class, ctx string, src []byte, line int) []byte {
	if !e.mentionsOld(src) {
		return src
	}
	e.markdown = strings.HasSuffix(p, ".md")
	var spans []Span
	claimed := make([]bool, len(src))
	for i := range e.Rules {
		r := &e.Rules[i]
		if !r.applies(p, ctx) {
			continue
		}
		for _, loc := range r.re.FindAllSubmatchIndex(src, -1) {
			start, end := loc[0], loc[1]
			if r.m > 0 {
				start, end = loc[2*r.m], loc[2*r.m+1]
			}
			if start < 0 || start == end || isClaimed(claimed, start, end) {
				continue
			}
			for j := start; j < end; j++ {
				claimed[j] = true
			}
			spans = append(spans, Span{Start: start, End: end, Rule: r, New: e.replacement(r, src, loc, start, end)})
		}
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].Start < spans[j].Start })

	var out bytes.Buffer
	last := 0
	for _, s := range spans {
		out.Write(src[last:s.Start])
		out.WriteString(s.New)
		last = s.End
		e.Stats.Hit(s.Rule.Name, s.Rule.Class, e.effective(s.Rule), class, p, src, s, line)
	}
	out.Write(src[last:])

	for _, loc := range e.residualRe.FindAllIndex(src, -1) {
		if !isFullyClaimed(claimed, loc[0], loc[1]) {
			e.Stats.Unclassified(p, ctx, src, loc[0], line)
		}
	}
	return out.Bytes()
}

// effective is the action a rule really takes on this run.
func (e *Engine) effective(r *Rule) Action {
	if r.Action == HoldUntilMoved && e.CodeMoved {
		return Rename
	}
	return r.Action
}

func (e *Engine) replacement(r *Rule, src []byte, loc []int, start, end int) string {
	claimed := string(src[start:end])
	if e.effective(r) != Rename {
		return claimed
	}
	if r.Fn != nil {
		ls := bytes.LastIndexByte(src[:start], '\n') + 1
		prev := e.prevComment
		if ls > 0 {
			pl := bytes.LastIndexByte(src[:ls-1], '\n') + 1
			prev = strings.TrimRight(string(src[pl:ls-1]), "\r")
		}
		return r.Fn(claimed, FnContext{Before: string(src[ls:start]), Prev: strings.Trim(prev, " \t/#*>"), Markdown: e.markdown})
	}
	return string(r.re.Expand(nil, []byte(r.Replace), src, loc))
}

func isClaimed(claimed []bool, start, end int) bool {
	return slices.Contains(claimed[start:end], true)
}

func isFullyClaimed(claimed []bool, start, end int) bool {
	return !slices.Contains(claimed[start:end], false)
}

// MapIdent returns the new name of a Go identifier. ok is false for an
// identifier no rule touches.
func (e *Engine) MapIdent(name string) (mapped string, rule *IdentRule, ok bool) {
	for i := range e.Idents {
		r := &e.Idents[i]
		if r.re.MatchString(name) {
			return r.re.ReplaceAllString(name, r.Replace), r, true
		}
	}
	return name, nil, false
}

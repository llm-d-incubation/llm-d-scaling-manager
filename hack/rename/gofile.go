package main

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/scanner"
	"go/token"
	"slices"
	"sort"
	"strings"
)

// edit replaces src[start:end] with text.
type edit struct {
	start, end int
	text       string
}

// RewriteGo renames one Go file through its syntax tree.
//
// The tree says what each token is; nothing here matches a pattern over the
// source. Identifiers go through the identifier table by name. Comments,
// string literals, struct tags and import paths each go through the text table
// in their own context, so a rule that is safe in a comment does not reach a
// tag. The edits are spliced into the original bytes, which keeps every comment
// and blank line where it was, and gofmt then realigns what the new lengths
// moved and re-sorts the imports.
//
// src must be LF: the caller normalises and restores the line endings.
func (e *Engine) RewriteGo(p, class string, src []byte) ([]byte, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, p, src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	tf := fset.File(file.Pos())
	off := func(pos token.Pos) int { return tf.Offset(pos) }
	lineOf := func(pos token.Pos) int { return tf.Line(pos) }

	tags := map[*ast.BasicLit]bool{}
	imports := map[*ast.BasicLit]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.Field:
			if x.Tag != nil {
				tags[x.Tag] = true
			}
		case *ast.ImportSpec:
			imports[x.Path] = true
		}
		return true
	})

	var edits []edit
	text := func(ctx string, pos token.Pos, raw string) {
		start := off(pos)
		out := e.RewriteText(p, class, ctx, []byte(raw), lineOf(pos))
		if string(out) != raw {
			edits = append(edits, edit{start, start + len(raw), string(out)})
		}
	}
	ast.Inspect(file, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.Ident:
			if mapped, rule, ok := e.MapIdent(x.Name); ok {
				e.Stats.Ident(rule, x.Name, mapped, class, p)
				start := off(x.Pos())
				edits = append(edits, edit{start, start + len(x.Name), mapped})
			} else if e.residualRe.MatchString(x.Name) {
				e.Stats.UnclassifiedIdent(p, x.Name, lineOf(x.Pos()))
			}
		case *ast.BasicLit:
			switch {
			case x.Kind != token.STRING && x.Kind != token.CHAR:
			case tags[x]:
				text(CtxTag, x.Pos(), x.Value)
			case imports[x]:
				text(CtxImport, x.Pos(), x.Value)
			default:
				text(CtxString, x.Pos(), x.Value)
			}
		}
		return true
	})
	for _, group := range file.Comments {
		e.prevComment = ""
		for _, c := range group.List {
			text(CtxComment, c.Pos(), c.Text)
			e.prevComment = c.Text
		}
	}
	e.prevComment = ""
	if len(edits) == 0 {
		return src, nil
	}

	sort.Slice(edits, func(i, j int) bool { return edits[i].start < edits[j].start })
	var out bytes.Buffer
	last := 0
	for _, ed := range edits {
		if ed.start < last {
			return nil, fmt.Errorf("overlapping edits at offset %d", ed.start)
		}
		out.Write(src[last:ed.start])
		out.WriteString(ed.text)
		last = ed.end
	}
	out.Write(src[last:])

	formatted, err := format.Source(out.Bytes())
	if err != nil {
		return nil, fmt.Errorf("gofmt after rewrite: %w", err)
	}
	if err := e.verify(p, src, formatted); err != nil {
		return nil, err
	}
	return formatted, nil
}

// goToken is one token of a Go file, comments left out.
type goToken struct {
	tok  token.Token
	text string
}

// goTokens lists a file's tokens, without comments and without the imports,
// which it returns apart because gofmt may reorder them.
func goTokens(src []byte) (body, imports []goToken) {
	fset := token.NewFileSet()
	f := fset.AddFile("", fset.Base(), len(src))
	var s scanner.Scanner
	s.Init(f, src, nil, 0)
	inImports := false
	depth := 0
	for {
		_, tok, lit := s.Scan()
		if tok == token.EOF {
			return body, imports
		}
		text := lit
		if text == "" || tok == token.SEMICOLON {
			text = tok.String()
		}
		t := goToken{tok, text}
		switch {
		case tok == token.IMPORT:
			inImports = true
			depth = 0
		case inImports && tok == token.LPAREN:
			depth++
		case inImports && tok == token.RPAREN:
			depth--
			if depth == 0 {
				inImports = false
			}
		case inImports:
			imports = append(imports, t)
			if depth == 0 && tok == token.SEMICOLON {
				inImports = false
			}
		default:
			body = append(body, t)
		}
	}
}

// verifyGo is the positional check on a rewritten file: the Nth token of the
// result must be the Nth token of the input, or that token as the tables map
// it. Nothing may appear, vanish or move, and a literal may only change the way
// the table says, so an expression in a fixture survives verbatim. Comparing in
// document order is deliberate: fixtures reuse names, and a check keyed by name
// would hide a value that drifted between two uses of it.
func (e *Engine) verifyGo(p string, before, after []byte) error {
	oldBody, oldImports := goTokens(before)
	newBody, newImports := goTokens(after)
	if len(oldBody) != len(newBody) {
		return fmt.Errorf("verify: %d tokens became %d", len(oldBody), len(newBody))
	}
	for i := range oldBody {
		o, n := oldBody[i], newBody[i]
		if o.text == n.text {
			continue
		}
		switch o.tok {
		case token.IDENT:
			if mapped, _, ok := e.MapIdent(o.text); !ok || mapped != n.text {
				return fmt.Errorf("verify: identifier %d is %q, the table says %q", i, n.text, mapped)
			}
		case token.STRING, token.CHAR:
			// Stats off: this reads the mapping, it does not count it.
			quiet := *e
			quiet.Stats = NewStats()
			want := e.mapLiteral(&quiet, p, o.text)
			if !slices.Contains(want, n.text) {
				return fmt.Errorf("verify: literal %d is %q, the table says %q", i, n.text, want[0])
			}
		default:
			return fmt.Errorf("verify: token %d changed, %q to %q", i, o.text, n.text)
		}
	}
	return e.verifyImports(p, oldImports, newImports)
}

// verifyImports compares imports as a set: gofmt sorts them, and a new module
// path can sort differently.
func (e *Engine) verifyImports(p string, before, after []goToken) error {
	if len(before) != len(after) {
		return fmt.Errorf("verify: %d import tokens became %d", len(before), len(after))
	}
	quiet := *e
	quiet.Stats = NewStats()
	var want, got []string
	for _, t := range before {
		text := t.text
		switch t.tok {
		case token.IDENT:
			if mapped, _, ok := e.MapIdent(text); ok {
				text = mapped
			}
		case token.STRING:
			text = string(quiet.RewriteText(p, "verify", CtxImport, []byte(text), 0))
		}
		want = append(want, text)
	}
	for _, t := range after {
		got = append(got, t.text)
	}
	slices.Sort(want)
	slices.Sort(got)
	if !slices.Equal(want, got) {
		return errors.New("verify: the imports changed beyond what the table says")
	}
	return nil
}

// mapLiteral is what a literal may have become, in each context a literal can
// sit in; the token stream does not say which it was.
func (e *Engine) mapLiteral(q *Engine, p, lit string) []string {
	contexts := []string{CtxString, CtxTag, CtxImport}
	out := make([]string, 0, len(contexts))
	for _, ctx := range contexts {
		out = append(out, string(q.RewriteText(p, "verify", ctx, []byte(lit), 0)))
	}
	return out
}

// lineEndings says how a file ends its lines.
type lineEndings int

const (
	endLF lineEndings = iota
	endCRLF
	endMixed
)

// detectEndings classifies a file's line endings.
func detectEndings(b []byte) lineEndings {
	crlf := bytes.Count(b, []byte("\r\n"))
	lf := bytes.Count(b, []byte("\n"))
	if crlf == 0 {
		return endLF
	}
	if crlf == lf {
		return endCRLF
	}
	return endMixed
}

// toLF and toCRLF convert an all-CRLF file to LF and back.
func toLF(b []byte) []byte   { return bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n")) }
func toCRLF(b []byte) []byte { return bytes.ReplaceAll(b, []byte("\n"), []byte("\r\n")) }

// lineAt returns the 1-based line of an offset and the text of that line.
func lineAt(b []byte, off int) (int, string) {
	n := bytes.Count(b[:off], []byte("\n")) + 1
	start := bytes.LastIndexByte(b[:off], '\n') + 1
	end := len(b)
	if i := bytes.IndexByte(b[off:], '\n'); i >= 0 {
		end = off + i
	}
	return n, strings.TrimRight(string(b[start:end]), "\r")
}

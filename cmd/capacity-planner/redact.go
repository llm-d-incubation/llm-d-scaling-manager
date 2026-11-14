package main

import (
	"net/url"
	"slices"
	"strings"
)

// redactMark is what a secret of the Prometheus URL reads as in an error.
const redactMark = "REDACTED"

// minQueryValue is the shortest part of the URL redacted on its own,
// wherever it appears: a shorter one ("1", "E", "/t") would rewrite
// unrelated text. A shorter query value is still redacted as key=value,
// the form a URL quotes it in, and inside the whole query.
const minQueryValue = 4

// redactedURL is the Prometheus URL as capacity-planner prints it: its scheme and host
// alone. Any other part can carry a secret (a password in the userinfo, a
// token in the query, a key in the path: /t/TOKEN/api), and nothing tells
// which does, so all of them are dropped, not redacted. A string that does
// not parse prints as "(unparsable URL)".
func redactedURL(s string) string {
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return "(unparsable URL)"
	}
	return (&url.URL{Scheme: u.Scheme, Host: u.Host}).String()
}

// urlSecrets is every part of the Prometheus URL that must not be printed,
// longest first. At any length: the userinfo (user:password), the query
// (raw, with its "?", and decoded), and each query pair as key=value. Only
// when at least minQueryValue long, since a shorter one would rewrite
// unrelated text: the path (from its leading slash, without a trailing
// one, as the client joins it to /api/v1/...), the password alone,
// each query value alone, and the fragment. A value is matched decoded and
// in both escapings ("+" and "%20" for a space).
func urlSecrets(s string) []string {
	u, err := url.Parse(s)
	if err != nil {
		return []string{s}
	}
	var out []string
	add := func(vs ...string) {
		for _, v := range vs {
			if v != "" && !slices.Contains(out, v) {
				out = append(out, v)
			}
		}
	}
	long := func(vs ...string) {
		for _, v := range vs {
			if len(v) >= minQueryValue {
				add(v)
			}
		}
	}
	if u.User != nil {
		add(u.User.String())
		if p, ok := u.User.Password(); ok {
			long(escapings(p)...)
		}
	}
	long(strings.TrimRight(u.EscapedPath(), "/"), strings.TrimRight(u.Path, "/"))
	if u.RawQuery != "" {
		add("?"+u.RawQuery, u.RawQuery)
		if dec, err := url.QueryUnescape(u.RawQuery); err == nil {
			add(dec)
		}
		q, _ := url.ParseQuery(u.RawQuery)
		for k, vs := range q {
			for _, v := range vs {
				for _, enc := range escapings(v) {
					add(k + "=" + enc)
				}
				long(escapings(v)...)
			}
		}
	}
	long(u.Fragment, u.EscapedFragment())
	slices.SortStableFunc(out, func(a, b string) int { return len(b) - len(a) })
	return out
}

// escapings is v as it is, query-escaped ("+" for a space) and
// path-escaped ("%20").
func escapings(v string) []string {
	return []string{v, url.QueryEscape(v), url.PathEscape(v), strings.ReplaceAll(url.QueryEscape(v), "+", "%20")}
}

// redactedError is err with every secret of the Prometheus URL prom
// replaced by REDACTED (urlSecrets), in one pass of one replacer, so text
// it inserts is never scanned again: an error from the HTTP client quotes
// the request's whole URL. It unwraps to err, so it keeps err's exit code
// (exitCode) and what errors.Is finds in it.
func redactedError(err error, prom string) error {
	if err == nil || prom == "" {
		return err
	}
	secrets := urlSecrets(prom)
	pairs := make([]string, 0, 2*len(secrets))
	for _, s := range secrets {
		pairs = append(pairs, s, redactMark)
	}
	msg := err.Error()
	clean := strings.NewReplacer(pairs...).Replace(msg)
	if clean == msg {
		return err
	}
	return redacted{msg: clean, err: err}
}

// redacted is an error whose message is msg, a redacted copy of err's,
// that unwraps to err so that errors.Is and errors.As still find what it
// wraps.
type redacted struct {
	msg string
	err error
}

func (r redacted) Error() string { return r.msg }

func (r redacted) Unwrap() error { return r.err }

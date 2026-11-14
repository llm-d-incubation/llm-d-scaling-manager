package main

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

// redactedURL keeps the scheme and host, and nothing else: a path, a
// query, a userinfo or a fragment can each carry a secret.
func TestRedactedURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://prom.example:9091":                   "https://prom.example:9091",
		"https://prom.example:9091/prom/":             "https://prom.example:9091",
		"https://gw.example/t/s3cr3t/api":             "https://gw.example",
		"https://u:p4ss@prom.example/?token=s3cr3t#f": "https://prom.example",
		"https://prom.example/?":                      "https://prom.example",
		"http://prom.example/a?token=s%2Fcr%3Dt&b=1":  "http://prom.example",
		"http://u@prom.example":                       "http://prom.example",
		"http://[::1]:9090/k3y":                       "http://[::1]:9090",
		"::not a url":                                 "(unparsable URL)",
		"/just/a/path":                                "(unparsable URL)",
	} {
		assert.Equal(t, want, redactedURL(in), in)
	}
}

// redactedError replaces each secret of the URL wherever an error quotes
// it: the raw query, or a query value or password alone, decoded or
// escaped, as a client that reorders the query, or a proxy that quotes one
// value, prints it. What it wraps is still found.
func TestRedactedError(t *testing.T) {
	const prom = "https://u:pw%21x@prom.example/?b=tk%2F9z&a=s3cr3t#frag"
	inner := usageError{errors.New("inner")}
	for _, msg := range []string{
		"Post https://prom.example/api?b=tk%2F9z&a=s3cr3t: refused",
		"Post https://prom.example/api?a=s3cr3t&b=tk%2F9z: refused",
		"value s3cr3t rejected",
		"value tk/9z rejected",
		"auth pw!x refused",
		"auth u:pw%21x refused",
		"fragment frag",
	} {
		err := redactedError(fmt.Errorf("%s: %w", msg, inner), prom)
		for _, leak := range []string{"s3cr3t", "tk/9z", "tk%2F9z", "pw!x", "pw%21x", "frag"} {
			assert.NotContains(t, err.Error(), leak, msg)
		}
		assert.Contains(t, err.Error(), "REDACTED", msg)
		assert.Equal(t, 2, exitCode(err), "%s: the usage error is still found", msg)
	}
	// A token in the path, as the client joins it to the API's path.
	err := redactedError(errors.New(`Post "https://gw.example/t/k3y-9f8e/api/v1/query_range": refused`), "https://gw.example/t/k3y-9f8e/")
	assert.Equal(t, `Post "https://gw.exampleREDACTED/api/v1/query_range": refused`, err.Error())

	plain := errors.New("nothing to hide")
	assert.Equal(t, plain, redactedError(plain, prom), "an error with no secret is returned as it is")
	assert.NoError(t, redactedError(nil, prom))
}

// A query value too short to be told from other text is redacted only as
// key=value and inside the whole query, so it does not rewrite the rest of
// the message.
func TestRedactedError_ShortValuesDoNotMangle(t *testing.T) {
	const prom = "https://prom.example/?x=1&e=E&tok=s3cr3t"
	msg := "step 1 of 10: EOF at https://prom.example/api?e=E&x=1: try E again (x=1)"
	got := redactedError(errors.New(msg), prom).Error()
	// The query reordered: each pair on its own.
	assert.Equal(t, "step 1 of 10: EOF at https://prom.example/api?REDACTED&REDACTED: try E again (REDACTED)", got)
}

// One pass of one replacer: a secret inside the text another put in is not
// replaced again, and a value that overlaps another (ACTED in REDACTED,
// s3cr3t inside s3cr3t-long) leaves no part of either.
func TestRedactedError_OverlappingValuesDoNotChain(t *testing.T) {
	const prom = "https://prom.example/?a=ACTED&b=s3cr3t&c=s3cr3t-long"
	msg := "a=ACTED then ACTED, s3cr3t-long and s3cr3t"
	got := redactedError(errors.New(msg), prom).Error()
	assert.Equal(t, "REDACTED then REDACTED, REDACTED and REDACTED", got)
	assert.NotContains(t, got, "REDREDACTED")
	assert.NotContains(t, got, "-long")
}

// A space in a value is redacted in each way a URL writes it: "+" in a
// query, "%20" in a path or by a client that escapes it so, and as it is.
func TestRedactedError_PlusAndPercent20(t *testing.T) {
	const prom = "https://prom.example/?k=pw+d+long"
	for _, msg := range []string{
		"value pw d long rejected",
		"value pw+d+long rejected",
		"value pw%20d%20long rejected",
		"query k=pw%20d%20long rejected",
	} {
		got := redactedError(errors.New(msg), prom).Error()
		for _, leak := range []string{"pw d", "pw+d", "pw%20d", "long"} {
			assert.NotContains(t, got, leak, msg)
		}
		assert.Contains(t, got, redactMark, msg)
	}
}

// The pieces only one rule catches: a userinfo whose password is too short
// to be redacted alone, a query quoted in its own order (one REDACTED, not
// one per pair), and a value whose "&" only the query escaping writes,
// with its space as "%20".
func TestRedactedError_EachRuleHasItsCase(t *testing.T) {
	got := redactedError(errors.New("auth admin:pw refused"), "https://admin:pw@prom.example/").Error()
	assert.Equal(t, "auth REDACTED refused", got)

	got = redactedError(errors.New("GET /api?b=tk9z&a=s3cr3t: refused"), "https://prom.example/?b=tk9z&a=s3cr3t").Error()
	assert.Equal(t, "GET /apiREDACTED: refused", got)

	got = redactedError(errors.New("value pw%20d%26long1 rejected"), "https://prom.example/?k=pw+d%26long1").Error()
	assert.Equal(t, "value REDACTED rejected", got)
}

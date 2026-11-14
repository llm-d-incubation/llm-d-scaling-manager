package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The old name, and only the old name, prints the deprecation note.
func TestInvokedAsDeprecated(t *testing.T) {
	for _, c := range []struct {
		argv0 string
		want  bool
	}{
		{"whatif", true},
		{"bin/whatif", true},
		{"/usr/local/bin/whatif", true},
		{"whatif.exe", true},
		{"capacity-planner", false},
		{"bin/capacity-planner", false},
		{"capacity-planner.exe", false},
		{"whatif-old", false},
		{"/tmp/go-build123/b001/exe/capacity-planner", false},
	} {
		assert.Equal(t, c.want, invokedAsDeprecated(c.argv0), c.argv0)
	}
}

// The note names the new command, so a user of the old one knows what to run.
func TestDeprecationNoteNamesTheNewCommand(t *testing.T) {
	assert.Contains(t, deprecationNote, progName)
	assert.Contains(t, deprecationNote, "deprecated")
}

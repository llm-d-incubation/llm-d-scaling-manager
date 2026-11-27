package main

import (
	"encoding/json"
	"io"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// serve --prefill-hit-at-count reads every source as a request asking for
// prefillHitAtCount is read on a server without the flag: the same inputs,
// echoed and under the same id, apart from the default's.
func TestServeReplayDefaults_AsIfEachRequestAsked(t *testing.T) {
	inputs := func(on bool, asked bool) whatifapi.InputsResponse {
		t.Helper()
		f := serveFlags{recordings: q32Dir, listen: "127.0.0.1:0", auth: whatifapi.AuthNone, noSelfCheck: true,
			replay: replayDefaults{hitAtCount: on}}
		h, err := f.handler(newFlags("serve", io.Discard), io.Discard)
		require.NoError(t, err)
		code, body := loopbackPost(t, h, "/v1/inputs", whatifapi.InputsRequest{PrefillHitAtCount: asked,
			Source: &whatifapi.SourceRequest{Kind: whatifapi.SourceRecording, Name: filepath.Base(q32Recording)}})
		require.Equal(t, 200, code, "%s", body)
		var in whatifapi.InputsResponse
		require.NoError(t, json.Unmarshal(body, &in))
		return in
	}
	plain, asked, served := inputs(false, false), inputs(false, true), inputs(true, false)
	assert.False(t, plain.PrefillHitAtCount)
	assert.True(t, served.PrefillHitAtCount, "the inputs say the switch was on")
	assert.Equal(t, asked.InputsID, served.InputsID, "the served default is the same as asking")
	assert.NotEqual(t, plain.InputsID, served.InputsID)
}

// serve takes each opt-in switch by the name every other command gives it.
func TestServeReplayDefaults_Flags(t *testing.T) {
	fs := newFlags("serve", io.Discard)
	var d replayDefaults
	d.register(fs)
	require.NoError(t, fs.Parse([]string{"--" + vpPlateauFlag, "--" + hitAtCountFlag}))
	assert.Equal(t, replayDefaults{vpPlateau: true, hitAtCount: true}, d)
	var f answerFlags
	d.apply(&f)
	assert.True(t, f.vpPlateau)
	assert.True(t, f.hitAtCount)
	f = answerFlags{hitAtCount: true}
	replayDefaults{}.apply(&f)
	assert.True(t, f.hitAtCount, "a request's own switch is never turned off")
}

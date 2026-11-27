package main

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
)

// backtest --experimental-ttft=v2 with no word on the draws answers what
// --rank-mixture-stratified answers, byte for byte, and not what
// --rank-mixture-stratified=false answers: the stratified service moments
// are the default.
func TestRankMixtureStratified_DefaultIsOn(t *testing.T) {
	v2 := append(slices.Clone(backtestQ32), "--experimental-ttft=v2", "-o", "json")
	def, defCode := runCmd(t, v2...)
	on, onCode := runCmd(t, append(slices.Clone(v2), "--rank-mixture-stratified=true")...)
	off, offCode := runCmd(t, append(slices.Clone(v2), "--rank-mixture-stratified=false")...)
	assert.Equal(t, 0, defCode, def)
	assert.Equal(t, defCode, onCode)
	assert.Equal(t, defCode, offCode)
	assert.Equal(t, on, def, "the default is --rank-mixture-stratified")
	assert.NotEqual(t, off, def, "the default is not --rank-mixture-stratified=false")
}

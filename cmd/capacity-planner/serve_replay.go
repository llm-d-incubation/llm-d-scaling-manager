package main

import "flag"

// serve turns the planner's opt-in replay switches on for every source it
// resolves with the CLI's own flags, as plain --experimental-ttft does
// (servedSources.experimentalTTFT): each source is then read as if its
// request had asked for the switch (whatifapi.InputsRequest
// PrefillVPPlateau, PrefillHitAtCount). A request's own field turns a switch
// on as well, never off. The switch is applied before the replay, so the
// inputs echo it and their id names it (servedSources.resolved), and every
// figure it moves carries its own flag, as when a request asks. A new
// opt-in switch is served by one field here, one line in apply and one in
// register.

// replayDefaults are the opt-in replay switches serve turns on for every
// source.
type replayDefaults struct {
	// vpPlateau is serve's --prefill-vp-plateau (vp_plateau.go).
	vpPlateau bool
	// hitAtCount is serve's --prefill-hit-at-count (hit_at_count.go).
	hitAtCount bool
}

// register registers each switch's flag on serve, by the name and help
// every other command gives it.
func (d *replayDefaults) register(fs *flag.FlagSet) {
	const all = "; on serve, for every source it resolves, as if each request asked"
	fs.BoolVar(&d.vpPlateau, vpPlateauFlag, false, vpPlateauUsage+all)
	fs.BoolVar(&d.hitAtCount, hitAtCountFlag, false, hitAtCountUsage+all)
}

// apply turns on in f, a source's answer flags, each switch serve turns on.
func (d replayDefaults) apply(f *answerFlags) {
	f.vpPlateau = f.vpPlateau || d.vpPlateau
	f.hitAtCount = f.hitAtCount || d.hitAtCount
}

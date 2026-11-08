# Frozen learned-state documents

`v0.10.0-v1.json` and `v0.10.1-v1.json` are frozen artifacts. They stand for
the learned-state document (ConfigMap data key `v1`) that v0.10.0 and v0.10.1
(and v0.10.2, which has the same document types) wrote. The current code must
restore them. Do not regenerate or edit them to make a test pass: they are the
old documents, and a change to today's types, keys or goldens must not change
them.

They are hand-built, not captured from a cluster. No capture of either
release's ConfigMap exists. `TestLearnedStateFixtures_Generate`
(`../../learned_state_backcompat_test.go`) built them once, with
`LEARNED_STATE_WRITE_FIXTURES=1`:

- the state is the last learned state the mu-paths golden recording exported;
- it is put into the frozen v0.10.1 types (`learned_state_v0101_test.go`);
- every map that export left empty gets an entry, written by hand in the
  frozen types and in the key formats v0.10.1 wrote (`withEveryMap`);
- the document is encoded with `json.Marshal`, as v0.10.1's encoder does.

The v0.10.0 file is the same document without `itlSlopes`. That is the only
field v0.10.0 (823ddcd74) lacks in these types.

A captured document from a real v0.10.0 or v0.10.1 controller may replace one
of them. Add it beside the old file rather than over it.

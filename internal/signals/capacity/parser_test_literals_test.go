package capacity

// Literals the parser tests share, named so a test asserting a dtype cannot
// silently drift from the one the fixture sets.
//
// Deliberately NOT the parser's own defaults: a fixture whose values equal the
// defaults cannot tell a successful parse from a failed one, which is a trap
// this package's tests have fallen into before.
const (
	testWeightDtype  = "bfloat16"
	testQuantization = "fp8"
)

// Values the engine-identity tests mutate the version-3 fields to.
const (
	testSpeculativeConfig = `{"method":"mtp"}`
	testKVConnector       = "NixlConnector"
	testOffloadBackend    = "native"
	testModelPath         = "org/model-FP8"
	testMemoryLayout      = "kv_cache_memory_bytes=40G"
	testModelRevision     = "abc123"
)

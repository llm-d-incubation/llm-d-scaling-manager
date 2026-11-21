package whatifapi

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The KV pull's JSON: no kvPull key where none is priced, and no wait key
// where the wait is unpriced, rather than a wait of 0.
func TestReportKVPull_JSONOmitsWhatIsUnpriced(t *testing.T) {
	var d ReportDecodeUse
	b, err := json.Marshal(d)
	require.NoError(t, err)
	assert.NotContains(t, string(b), `"kvPull"`)

	d.KVPull = &ReportKVPull{Utilization: 1.2}
	b, err = json.Marshal(d)
	require.NoError(t, err)
	assert.Contains(t, string(b), `"kvPull":{"utilization":1.2,"setsTTFT":false}`)

	w := 0.05
	d.KVPull = &ReportKVPull{Utilization: 0.62, Wait: &w, SetsTTFT: true}
	b, err = json.Marshal(d)
	require.NoError(t, err)
	assert.Contains(t, string(b), `"kvPull":{"utilization":0.62,"wait":0.05,"setsTTFT":true}`)
}

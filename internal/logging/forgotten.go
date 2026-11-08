package logging

import (
	"strings"

	"github.com/go-logr/logr"
)

// ForgottenKeyMessage is the line logged once per key a forget takes, at the
// default verbosity, beside the engine's one line per variant.
const ForgottenKeyMessage = "Learned state forgotten for one key, on request"

// ForgottenKeys logs each key a forget took, one line per key, on the
// caller's logger: the keys the saturation analyzer files in its learner
// report (learn.Report.Forgotten), which an analyzer with no learner report
// logs. Every key here is forgotten by the operator, so the line names no
// cause. A key's NUL separators (the throughput analyzer's variantKey) are
// logged as "|", the slo analyzer's: a NUL makes grep call a log binary.
func ForgottenKeys(logger logr.Logger, analyzer, learner, namespace, modelID, variant string, keys []string) {
	for _, key := range keys {
		logger.Info(ForgottenKeyMessage, "analyzer", analyzer, "learner", learner,
			"namespace", namespace, "modelID", modelID, "variant", variant,
			"key", strings.ReplaceAll(key, "\x00", "|"))
	}
}

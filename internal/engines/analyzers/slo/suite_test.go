package slo

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
)

// testLine is ITL(k) = 40 ms * k + 20 ms: 26 ms at the bottom of the fitted range,
// 44 ms at k = 0.6, 52 ms at the top. The whatif package's tests price the
// same line.
var testLine = itl.Model{A: 0.040, B: 0.020}

func TestSLO(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "SLO Analyzer Suite")
}

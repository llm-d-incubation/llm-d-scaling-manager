package allocation

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/logging"
)

func TestScaleToZero(t *testing.T) {
	logging.NewTestLogger()
	RegisterFailHandler(Fail)
	RunSpecs(t, "ScaleToZero Suite")
}

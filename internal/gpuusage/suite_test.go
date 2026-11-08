package gpuusage

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestGPUUsage(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "GPU Usage Suite")
}

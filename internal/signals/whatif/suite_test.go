package whatif

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestWhatif(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Whatif Suite")
}

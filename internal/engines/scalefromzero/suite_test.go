package scalefromzero

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestScaleFromZero(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Scale-From-Zero Suite")
}

/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package steadystate

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
	// +kubebuilder:scaffold:imports
)

// These tests use Ginkgo (BDD-style Go testing framework). Refer to
// http://onsi.github.io/ginkgo/ to learn more about Ginkgo.

var (
	ctx       context.Context
	cancel    context.CancelFunc
	testEnv   *envtest.Environment
	cfg       *rest.Config
	k8sClient client.Client
)

func TestSaturationEngine(t *testing.T) {
	RegisterFailHandler(Fail)

	RunSpecs(t, "Saturation Engine Suite")
}

// The borrow record is process-wide (allocation.DefaultBorrowInFlight): every
// spec starts with it empty, so one spec's draws are not another's.
var _ = BeforeEach(func() { allocation.DefaultBorrowInFlight.Reset() })

var _ = BeforeSuite(func() {
	logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))

	ctx, cancel = context.WithCancel(context.TODO()) //nolint:fatcontext // shared across BeforeSuite/AfterSuite

	var err error
	Expect(kedav1alpha1.AddToScheme(scheme.Scheme)).To(Succeed())

	// +kubebuilder:scaffold:scheme

	By("bootstrapping test environment")
	// Variants are discovered from KEDA ScaledObjects, so the suite needs that
	// CRD. Variant is no longer a CRD, and the HPA discovery path is
	// gone, so nothing else is required.
	var crdPaths []string
	if dir := getKEDACRDDir(); dir != "" {
		crdPaths = append(crdPaths, dir)
	}
	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     crdPaths,
		ErrorIfCRDPathMissing: true,
	}

	// Retrieve the first found binary directory to allow running tests from IDEs
	if getFirstFoundEnvTestBinaryDir() != "" {
		testEnv.BinaryAssetsDirectory = getFirstFoundEnvTestBinaryDir()
	}

	// cfg is defined in this file globally.
	cfg, err = testEnv.Start()
	Expect(err).NotTo(HaveOccurred())
	Expect(cfg).NotTo(BeNil())

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme.Scheme})
	Expect(err).NotTo(HaveOccurred())
	Expect(k8sClient).NotTo(BeNil())
})

var _ = AfterSuite(func() {
	By("tearing down the test environment")
	cancel()
	err := testEnv.Stop()
	Expect(err).NotTo(HaveOccurred())
})

// getFirstFoundEnvTestBinaryDir locates the first binary in the specified path.
func getFirstFoundEnvTestBinaryDir() string {
	basePath := filepath.Join("..", "..", "..", "bin", "k8s")
	entries, err := os.ReadDir(basePath)
	if err != nil {
		logf.Log.Error(err, "Failed to read directory", "path", basePath)
		return ""
	}
	for _, entry := range entries {
		if entry.IsDir() {
			return filepath.Join(basePath, entry.Name())
		}
	}
	return ""
}

// getKEDACRDDir locates the KEDA ScaledObject CRD in the module cache so envtest
// can install it. Returns "" when it cannot be found, which surfaces as an
// ErrorIfCRDPathMissing failure rather than a silently CRD-less cluster.
func getKEDACRDDir() string {
	moduleDir, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "github.com/kedacore/keda/v2").Output()
	if err == nil && strings.TrimSpace(string(moduleDir)) != "" {
		dir := filepath.Join(strings.TrimSpace(string(moduleDir)), "config", "crd", "bases")
		if _, err := os.Stat(dir); err == nil {
			return dir
		}
	}
	// Fall back to GOMODCACHE when the module is in the download cache but not
	// checked out locally (go list -m {{.Dir}} returns empty in that case).
	version, err := exec.Command("go", "list", "-m", "-f", "{{.Version}}", "github.com/kedacore/keda/v2").Output()
	if err != nil || strings.TrimSpace(string(version)) == "" {
		return ""
	}
	cache, err := exec.Command("go", "env", "GOMODCACHE").Output()
	if err != nil {
		return ""
	}
	dir := filepath.Join(strings.TrimSpace(string(cache)),
		"github.com", "kedacore", "keda", "v2@"+strings.TrimSpace(string(version)),
		"config", "crd", "bases")
	if _, err := os.Stat(dir); err != nil {
		return ""
	}
	return dir
}

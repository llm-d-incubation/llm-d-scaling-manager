package e2e

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"github.com/llm-d-incubation/llm-d-scaling-manager/test/e2e/fixtures"
	"github.com/llm-d-incubation/llm-d-scaling-manager/test/utils"
)

// watchNamespaceEnv is the controller's environment variable for the one
// namespace a namespace-scoped install watches (internal/config/loader.go).
const watchNamespaceEnv = "WATCH_NAMESPACE"

// A namespace-scoped controller cannot list nodes, so the utilization-share
// optimizer plans by GPU count alone (allocation.NodeUsageWanted): 1-GPU
// replicas can never fund an 8-GPU one, however they are packed. This is the
// documented limit, pinned on the very layout where a cluster-scoped
// controller funds G ("funded by small models").
//
// The scope is set the way a namespace-scoped install sets it -- the
// controller's watch namespace -- on the suite's own controller, for this
// spec only; the controller's RBAC stays as installed. The models therefore
// live in the controller's namespace.
var _ = Describe("Utilization share with a whole-node model: a namespace-scoped controller", Label("utilization-share-wholenode"), Ordered, Serial, func() {
	const (
		quotaGPUs = 18
		msG       = "uwn-g"
		msS1      = "uwn-s1"
		msS2      = "uwn-s2"
		modelG    = "uwn-model-g"
		modelS1   = "uwn-model-s1"
		modelS2   = "uwn-model-s2"
		loadG     = "uwn-g-load"
	)
	depG, depS1, depS2 := msG+"-decode", msS1+"-decode", msS2+"-decode"
	soG, soS1, soS2 := msG+"-so", msS1+"-so", msS2+"-so"
	var e *wholeNodeEnv

	BeforeAll(func() {
		e = findWholeNodes(3)
		e.ns, e.shared = cfg.LLMDNamespace, true
		Expect(cfg.LLMSMNamespace).To(Equal(cfg.LLMDNamespace),
			"a namespace-scoped controller manages its own namespace; the models must be there")
		e.fillerNS = newSpecNamespace("uwn-filler-")

		By("Scoping the controller to its own namespace")
		Expect(setControllerWatchNamespace(cfg.LLMDNamespace)).To(Succeed())
		DeferCleanup(func() {
			Expect(setControllerWatchNamespace("")).To(Succeed(), "could not return the controller to cluster scope")
		})

		restore, err := fixtures.SetNamespaceQuota(ctx, k8sClient, cfg.LLMSMNamespace, scalingPolicyConfigMapName(),
			e.ns, e.product, quotaGPUs, fixtures.WithOptimizer(map[string]any{"type": "utilizationShare"}))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = restore(context.Background()) })
		layOutPacked(e, msG, modelG, msS1, modelS1, msS2, modelS2)
		e.register(soG, depG, modelG, 1, 2, "")
		e.register(soS1, depS1, modelS1, 5, 5, "")
		e.register(soS2, depS2, modelS2, 5, 5, "")
		e.load(loadG, msG, modelG, 16)
	})

	// The cluster-scoped controller funds G on this layout with one set of
	// eight within about two minutes of the floors dropping. Here no transfer
	// toward G starts. The small models are idle, so their spare goes back to
	// the quota by returns, and the idle fill may then raise G into it -- blind
	// to nodes, so G's pod lands only if the returns happened to empty a node.
	// The spec records which happened.
	It("starts no transfer toward G from the small models", func() {
		start := time.Now()
		wholeNodeProm()
		Eventually(func(g Gomega) {
			short, read := e.shortOf(modelG, start.Add(-5*time.Minute))
			g.Expect(short).To(BeTrue(), read)
		}, 8*time.Minute, 10*time.Second).Should(Succeed())
		By("Lowering S1's and S2's floors to 1")
		e.lowerFloor(soS1, depS1, modelS1, 5, "")
		e.lowerFloor(soS2, depS2, modelS2, 5, "")
		floorDropped := time.Now()

		By("Holding eight minutes: nothing is given for G, and G says why")
		e.holdUnfunded("namespace scope", modelG, depG, []string{depS1, depS2}, start, floorDropped, 8*time.Minute, true)
	})
})

// setControllerWatchNamespace sets the controller's watch namespace ("" for
// cluster scope) and waits until the restarted controller leads and serves.
func setControllerWatchNamespace(ns string) error {
	Eventually(func(g Gomega) {
		dep, err := k8sClient.AppsV1().Deployments(cfg.LLMSMNamespace).Get(ctx, utils.ControllerDeploymentName, metav1.GetOptions{})
		g.Expect(err).NotTo(HaveOccurred())
		c := managerContainer(dep)
		g.Expect(c).NotTo(BeNil(), "no manager container in %s", utils.ControllerDeploymentName)
		for _, a := range c.Args {
			g.Expect(a).NotTo(HavePrefix("--watch-namespace"),
				"the controller is installed namespace-scoped already (%s); this spec flips the scope of a cluster-scoped install", a)
		}
		c.Env = slices.DeleteFunc(c.Env, func(v corev1.EnvVar) bool { return v.Name == watchNamespaceEnv })
		if ns != "" {
			c.Env = append(c.Env, corev1.EnvVar{Name: watchNamespaceEnv, Value: ns})
		}
		_, err = k8sClient.AppsV1().Deployments(cfg.LLMSMNamespace).Update(ctx, dep, metav1.UpdateOptions{})
		g.Expect(err).NotTo(HaveOccurred())
	}, time.Minute, 2*time.Second).Should(Succeed())
	// The env change rolls the controller once; the restart waits for the
	// rollout, the new leader and KEDA's route to it.
	if err := restartScalingManagerController(ctx); err != nil {
		return fmt.Errorf("restart the controller with %s=%q: %w", watchNamespaceEnv, ns, err)
	}
	return nil
}

// managerContainer is the controller Deployment's manager container: the one
// named manager, else the first.
func managerContainer(dep *appsv1.Deployment) *corev1.Container {
	cs := dep.Spec.Template.Spec.Containers
	for i := range cs {
		if cs[i].Name == utils.ControllerContainerName {
			return &cs[i]
		}
	}
	if len(cs) == 0 {
		return nil
	}
	return &cs[0]
}

// Kueue beside the utilization-share optimizer, against a live Kueue: the
// optimizer budgets the GPUs Kueue guarantees and starts no borrowing.
//
//   - A flat cohort of two ClusterQueues: a, nominal 9 GPUs, holds large
//     model G (one 8-GPU replica) and small model S (one 1-GPU replica); b,
//     nominal 8, lends and runs nothing. Both reclaim Any, so b's 8 GPUs are
//     borrowable.
//   - The policy: gpu-inventory and a namespace quota bounded by Kueue, and
//     the optimizer active. G is loaded.
//   - With the optimizer, G stays at one replica and queue a borrows
//     nothing. Then the optimizer is switched off: today's optimizer borrows
//     b's 8 GPUs for a second G replica -- the negative control.
//
// It also records whether the installed Kueue evicts pods that are slow to be
// Ready (waitForPodsReady): a large model's start can exceed its timeout.
var _ = Describe("Utilization share with a whole-node model: under Kueue", Label("utilization-share-wholenode"), Ordered, Serial, func() {
	const (
		cohortName = "e2e-ushare-kueue"
		flavorName = "e2e-ushare-kueue"
		queueA     = "e2e-ushare-kueue-a"
		queueB     = "e2e-ushare-kueue-b"
		nsA        = "e2e-ushare-kueue-a"
		nsB        = "e2e-ushare-kueue-b"
		msG        = "uwk-g"
		msS        = "uwk-s"
		modelG     = "uwk-model-g"
		modelS     = "uwk-model-s"
		loadG      = "uwk-g-load"
	)
	depG, depS := msG+"-decode", msS+"-decode"
	soG, soS := msG+"-so", msS+"-so"
	var e *wholeNodeEnv
	var cohort fixtures.KueueCohort

	borrowed := func(g Gomega) int {
		st, err := fixtures.ReadKueueQueueStatus(ctx, crClient, queueA, flavorName, e.gpuRN)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(st.HasStatus).To(BeTrue(), "ClusterQueue %s has no reservation status; is Kueue running?", queueA)
		return st.Borrowed
	}
	queued := func(d *appsv1.Deployment) {
		if d.Labels == nil {
			d.Labels = map[string]string{}
		}
		d.Labels[fixtures.KueueQueueNameLabel] = queueA
	}

	BeforeAll(func() {
		if !fixtures.KueueInstalled(crClient) {
			wholeNodeUnavailable("the Kueue CRDs are not installed; make install-kueue-e2e installs Kueue")
		}
		running, what, err := fixtures.KueueControllerRunning(ctx, k8sClient)
		Expect(err).NotTo(HaveOccurred())
		if !running {
			wholeNodeUnavailable("no Kueue controller is running (" + what + "); make install-kueue-e2e KUEUE_CONTROLLER=true")
		}
		wnMeasure("kueue: waitForPodsReady", kueueWaitForPodsReady())

		e = findWholeNodes(3)
		e.ns = nsA
		for _, ns := range []string{nsA, nsB} {
			createKueueNamespace(ns)
			name := ns
			DeferCleanup(func() {
				_ = k8sClient.CoreV1().Namespaces().Delete(context.Background(), name, metav1.DeleteOptions{})
			})
		}
		Expect(fixtures.GrantPodWrites(ctx, k8sClient, nsA, cfg.LLMSMNamespace)).To(Succeed())
		cohort = fixtures.KueueCohort{
			Cohort: cohortName, Flavor: flavorName, ProductLabel: e.key, Product: e.product, Resource: e.gpuRN,
			Queues: []fixtures.KueueCohortQueue{
				{Name: queueA, Namespace: nsA, GPUs: 9, Reclaim: fixtures.KueueReclaimAny},
				{Name: queueB, Namespace: nsB, GPUs: 8, Reclaim: fixtures.KueueReclaimAny},
			},
		}
		Expect(fixtures.CreateKueueCohort(ctx, crClient, cohort)).To(Succeed())
		DeferCleanup(func() {
			bg := context.Background()
			_ = fixtures.DeleteScaledObject(bg, crClient, nsA, soG)
			_ = fixtures.DeleteScaledObject(bg, crClient, nsA, soS)
			errs := []error{
				fixtures.WaitDeploymentGone(bg, k8sClient, nsA, depG, "app="+depG, 3*time.Minute),
				fixtures.WaitDeploymentGone(bg, k8sClient, nsA, depS, "app="+depS, 3*time.Minute),
				fixtures.DeleteKueueCohort(bg, crClient, cohort),
			}
			for _, err := range errs {
				Expect(err).NotTo(HaveOccurred(), "cleanup left Kueue state behind")
			}
		})

		restore, err := fixtures.SetNamespaceQuota(ctx, k8sClient, cfg.LLMSMNamespace, scalingPolicyConfigMapName(),
			nsA, e.product, -1, fixtures.WithKueue("5s"), fixtures.WithKueueTimings("30s", "4m", "10s"),
			fixtures.WithPhysicalLimiter(), fixtures.WithOptimizer(map[string]any{"type": "utilizationShare"}))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = restore(context.Background()) })

		By("Creating G (8 GPUs) and S (1 GPU) on queue a")
		e.model(msG, modelG, 1, 8, queued)
		e.model(msS, modelS, 1, 1, queued)
		e.waitReady(map[string]int32{depG: 1, depS: 1}, 5*time.Minute)
		e.register(soG, depG, modelG, 1, 2, "")
		e.register(soS, depS, modelS, 1, 1, "")
		e.load(loadG, msG, modelG, 16)
	})

	It("with the optimizer, keeps G within queue a's guaranteed GPUs and borrows nothing", func() {
		start := time.Now()
		pc := wholeNodeProm()
		Eventually(func(g Gomega) {
			short, read := e.shortOf(modelG, start.Add(-5*time.Minute))
			g.Expect(short).To(BeTrue(), read)
		}, 8*time.Minute, 10*time.Second).Should(Succeed())
		By("Holding five minutes: G stays at one replica, queue a borrows nothing")
		Consistently(func(g Gomega) {
			g.Expect(e.specReplicas(g, depG)).To(Equal(int32(1)))
			g.Expect(borrowed(g)).To(BeZero())
		}, 5*time.Minute, 10*time.Second).Should(Succeed())
		reasons, err := blockedReasons(pc, e.ns, modelG)
		Expect(err).NotTo(HaveOccurred())
		wnMeasure("kueue with optimizer: G blocked reasons", reasons)
		lines := shareLines(controllerLogsSince(start), e.ns)
		Expect(lines).NotTo(BeEmpty(), "the optimizer never planned queue a's group")
	})

	It("without the optimizer, borrows queue b's GPUs for a second G replica", func() {
		By("Switching the optimizer off")
		_, err := fixtures.SetNamespaceQuota(ctx, k8sClient, cfg.LLMSMNamespace, scalingPolicyConfigMapName(),
			nsA, e.product, -1, fixtures.WithKueue("5s"), fixtures.WithKueueTimings("30s", "4m", "10s"),
			fixtures.WithPhysicalLimiter(), fixtures.WithoutOptimizer())
		Expect(err).NotTo(HaveOccurred())
		switched := time.Now()
		Eventually(func(g Gomega) {
			g.Expect(e.readyReplicas(g, depG)).To(Equal(int32(2)), unreadyPods(e.ns))
			g.Expect(borrowed(g)).To(Equal(8))
		}, 10*time.Minute, 10*time.Second).Should(Succeed())
		wnMeasure("kueue without optimizer: switch to borrowed G replica Ready", time.Since(switched))
	})
})

// kueueWaitForPodsReady says whether the installed Kueue's configuration
// turns on waitForPodsReady, which evicts a workload whose pods are not Ready
// within its timeout (5 minutes by default) -- shorter than a large model's
// cold start. It reads the configuration as YAML: the release manifest ships
// the block commented out.
func kueueWaitForPodsReady() string {
	cms, err := k8sClient.CoreV1().ConfigMaps("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return "unknown: " + err.Error()
	}
	for _, cm := range cms.Items {
		if !strings.HasPrefix(cm.Name, "kueue-manager-config") {
			continue
		}
		for key, v := range cm.Data {
			var conf struct {
				WaitForPodsReady map[string]any `json:"waitForPodsReady"`
			}
			if err := yaml.Unmarshal([]byte(v), &conf); err != nil {
				return fmt.Sprintf("unknown: %s/%s %s: %v", cm.Namespace, cm.Name, key, err)
			}
			if conf.WaitForPodsReady == nil {
				return fmt.Sprintf("off (%s/%s sets no waitForPodsReady)", cm.Namespace, cm.Name)
			}
			return fmt.Sprintf("on in %s/%s: %v", cm.Namespace, cm.Name, conf.WaitForPodsReady)
		}
	}
	return "unknown: no kueue-manager-config ConfigMap"
}

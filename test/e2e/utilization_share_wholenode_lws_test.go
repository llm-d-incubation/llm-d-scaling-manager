package e2e

import (
	"fmt"
	"sort"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"

	"github.com/llm-d-incubation/llm-d-scaling-manager/test/e2e/fixtures"
)

// A large model served by a LeaderWorkerSet whose replica is two 8-GPU pods
// on two whole nodes, beside 1-GPU models: as donor, and as receiver.
//
//   - Nodes 1 to 4 hold large model G, two groups of a leader and a worker,
//     8 GPUs each. Node 5 holds small models S1 and S2, one 1-GPU replica
//     each. Every other GPU is closed.
//   - One namespace quota of 34 GPUs: spent.
//   - S1 is loaded. G, its floor lowered to 1, gives one group: its highest
//     group index goes, two nodes empty, S1 is raised onto them.
//   - Then S1's load stops and G is loaded: G must get a group of two whole
//     nodes back.
var _ = Describe("Utilization share with a whole-node model: a two-node LeaderWorkerSet", Label("utilization-share-wholenode"), Ordered, func() {
	const (
		quotaGPUs = 34
		msG       = "uwm-g"
		msS1      = "uwm-s1"
		msS2      = "uwm-s2"
		modelG    = "uwm-model-g"
		modelS1   = "uwm-model-s1"
		modelS2   = "uwm-model-s2"
		loadS1    = "uwm-s1-load"
		loadG     = "uwm-g-load"
	)
	lwsG, depS1, depS2 := msG+"-decode", msS1+"-decode", msS2+"-decode"
	soG, soS1, soS2 := msG+"-so", msS1+"-so", msS2+"-so"
	var e *wholeNodeEnv

	groups := func(g Gomega) map[string][]string {
		pods, err := k8sClient.CoreV1().Pods(e.ns).List(ctx, metav1.ListOptions{LabelSelector: lwsv1.SetNameLabelKey + "=" + lwsG})
		g.Expect(err).NotTo(HaveOccurred())
		out := map[string][]string{}
		for _, p := range pods.Items {
			if p.DeletionTimestamp == nil {
				out[p.Labels[lwsv1.GroupIndexLabelKey]] = append(out[p.Labels[lwsv1.GroupIndexLabelKey]], p.Spec.NodeName)
			}
		}
		for k := range out {
			sort.Strings(out[k])
		}
		return out
	}
	readyGroups := func(g Gomega) int32 {
		var l lwsv1.LeaderWorkerSet
		g.Expect(crClient.Get(ctx, client.ObjectKey{Namespace: e.ns, Name: lwsG}, &l)).To(Succeed())
		return l.Status.ReadyReplicas
	}

	BeforeAll(func() {
		e = setUpWholeNode("uwm-", 5)
		restore, err := fixtures.SetNamespaceQuota(ctx, k8sClient, cfg.LLMSMNamespace, scalingPolicyConfigMapName(),
			e.ns, e.product, quotaGPUs, fixtures.WithOptimizer(map[string]any{"type": "utilizationShare"}))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = restore(ctx) })

		By("Closing every GPU but nodes 1 to 4 whole and two GPUs on node 5")
		e.closeAllBut(map[string]int{e.nodes[0]: 8, e.nodes[1]: 8, e.nodes[2]: 8, e.nodes[3]: 8, e.nodes[4]: 2})
		By("Creating G, two groups of two 8-GPU pods: only nodes 1 to 4 hold one")
		Expect(fixtures.EnsureModelServiceLWS(ctx, crClient, e.ns, msG, msG+"-pool", modelG, cfg.UseSimulator, 2, 2,
			fixtures.WithLWSGPUs(e.key, e.product, e.gpuRN, 8), fixtures.WithLWSReplicas(2))).To(Succeed())
		// Traffic to the group leaders only, as llm-d routes it: a replica is
		// read from its leader.
		e.serve(msG, lwsG, fixtures.WithServiceSelector(lwsv1.WorkerIndexLabelKey, "0"))
		Eventually(func(g Gomega) {
			g.Expect(readyGroups(g)).To(Equal(int32(2)), unreadyPods(e.ns))
		}, 6*time.Minute, 5*time.Second).Should(Succeed())
		By("Creating S1 and S2, one 1-GPU replica each: only node 5 has room")
		e.model(msS1, modelS1, 1, 1)
		e.model(msS2, modelS2, 1, 1)
		e.waitReady(map[string]int32{depS1: 1, depS2: 1}, 5*time.Minute)
		wnMeasure("lws: G groups by node", groups(Default))

		By("Registering all three; G's floor is 2 until S1 reads short")
		e.register(soG, lwsG, modelG, 2, 2, "LeaderWorkerSet")
		e.register(soS1, depS1, modelS1, 1, 4, "")
		e.register(soS2, depS2, modelS2, 1, 2, "")
		e.load(loadS1, msS1, modelS1, 8)
	})

	It("lends G's highest group, two whole nodes, to S1", func() {
		start := time.Now()
		Eventually(func(g Gomega) {
			short, read := e.shortOf(modelS1, start.Add(-5*time.Minute))
			g.Expect(short).To(BeTrue(), read)
		}, 8*time.Minute, 10*time.Second).Should(Succeed())
		By("Lowering G's floor to 1")
		floorDropped := time.Now()
		e.lowerFloor(soG, lwsG, modelG, 2, "LeaderWorkerSet")

		By("Waiting for a transfer from G to S1 to complete")
		var set transferSet
		Eventually(func(g Gomega) {
			lines := shareLines(controllerLogsSince(start), e.ns)
			for _, s := range setsTo(lines, modelS1) {
				for _, m := range s.Members {
					if roleOf(m.Donor, modelG) {
						set = s
					}
				}
			}
			g.Expect(set.Outcome).To(Equal("done"), "no transfer from G to S1 is done:\n%s", describeShare(lines, 40))
			g.Expect(e.readyReplicas(g, depS1)).To(BeNumerically(">=", 2))
		}, 15*time.Minute, 5*time.Second).Should(Succeed())
		wnMeasure("lws lend: floor dropped to started", set.Started.Sub(floorDropped))
		wnMeasure("lws lend: started to S1 raised", set.Raised.Sub(set.Started))
		wnMeasure("lws lend: donor pods marked", set.Members[0].Pods)
		Eventually(func(g Gomega) {
			gs := groups(g)
			g.Expect(gs).To(HaveLen(1))
			g.Expect(gs).To(HaveKey("0"), "the highest group, 1, must be the one that goes")
		}, 3*time.Minute, 5*time.Second).Should(Succeed())
		time.Sleep(2 * time.Minute)
		wnMeasure("lws lend: S1 pods by node two minutes after", e.podsByNode(depS1))
	})

	It("gives G a two-node group back on its burst", func() {
		start := time.Now()
		e.stopLoad(loadS1)
		e.load(loadG, msG, modelG, 16)
		wnMeasure("lws reclaim: S1 pods by node at G's burst", e.podsByNode(depS1))
		var lines []shareLine
		Eventually(func(g Gomega) {
			lines = shareLines(controllerLogsSince(start), e.ns)
			g.Expect(readyGroups(g)).To(Equal(int32(2)), "G groups %v, S1 on %v\n%s", groups(g), e.podsByNode(depS1), describeShare(lines, 40))
		}, 25*time.Minute, 10*time.Second).Should(Succeed())
		wnMeasure("lws reclaim: G load to two Ready groups", time.Since(start))
		var how []string
		for _, s := range setsTo(lines, modelG) {
			how = append(how, fmt.Sprintf("set %s of %d (outcome %s)", s.ID, len(s.Members), s.Outcome))
		}
		for _, l := range withMsg(lines, "filling from idle GPUs") {
			if roleOf(l.Receiver, modelG) {
				how = append(how, "idle fill "+l.ID)
			}
		}
		for _, l := range withMsg(lines, "spare returned") {
			how = append(how, "return from "+l.Donor)
		}
		wnMeasure("lws reclaim: how G grew", strings.Join(how, "; "))
		wnMeasure("lws reclaim: G groups by node", groups(Default))
	})
})

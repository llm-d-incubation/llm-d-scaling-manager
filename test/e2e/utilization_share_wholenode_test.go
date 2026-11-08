package e2e

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/llm-d-incubation/llm-d-scaling-manager/test/e2e/fixtures"
)

// The utilization-share optimizer with a large model whose replica is one
// whole 8-GPU node, beside 1-GPU models (docs/well-lit-paths/utilization-share/).
// Emulated GPUs on kind: these show the shapes a real run of a large
// mixture-of-experts model cannot afford to try -- lending a whole node,
// funding one from small pods, a fragmented cluster, a released node taken by
// someone else. Each spec closes every GPU it does not lay a model on with
// pause pods, so where each pod can land is decided by the spec, not the
// scheduler.
//
// Label utilization-share-wholenode, outside the full tier: the specs need
// several nodes of exactly 8 GPUs of one product, which the default kind
// cluster does not have. make test-e2e-ushare-wholenode-with-setup builds that
// cluster and runs them.

// A large model lends a whole node to a small one when idle, and takes it
// back on its own burst.
//
//   - Nodes 1 and 2 hold large model G, two 8-GPU replicas. Node 3 holds small
//     models S1 and S2, one 1-GPU replica each. Every other GPU is closed.
//   - One namespace quota of 18 GPUs: spent.
//   - S1 is loaded. G, its floor lowered to 1, gives one replica: its pod
//     goes, the node empties, S1 is raised onto it.
//   - Then S1's load stops and G is loaded: G must get a whole node back,
//     from S1's pods on the node it gave.
var _ = Describe("Utilization share with a whole-node model: lending a node", Label("utilization-share-wholenode"), Ordered, func() {
	const (
		quotaGPUs = 18
		msG       = "uwl-g"
		msS1      = "uwl-s1"
		msS2      = "uwl-s2"
		modelG    = "uwl-model-g"
		modelS1   = "uwl-model-s1"
		modelS2   = "uwl-model-s2"
		loadS1    = "uwl-s1-load"
		loadG     = "uwl-g-load"
	)
	depG, depS1, depS2 := msG+"-decode", msS1+"-decode", msS2+"-decode"
	soG, soS1, soS2 := msG+"-so", msS1+"-so", msS2+"-so"
	var e *wholeNodeEnv
	var lentNode string

	BeforeAll(func() {
		e = setUpWholeNode("uwl-", 3)
		restore, err := fixtures.SetNamespaceQuota(ctx, k8sClient, cfg.LLMSMNamespace, scalingPolicyConfigMapName(),
			e.ns, e.product, quotaGPUs, fixtures.WithOptimizer(map[string]any{"type": "utilizationShare"}))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = restore(ctx) })

		By("Closing every GPU but nodes 1 and 2 whole and two GPUs on node 3")
		e.closeAllBut(map[string]int{e.nodes[0]: 8, e.nodes[1]: 8, e.nodes[2]: 2})
		By("Creating G first, two 8-GPU replicas: only nodes 1 and 2 hold one")
		e.model(msG, modelG, 2, 8)
		e.waitReady(map[string]int32{depG: 2}, 5*time.Minute)
		By("Creating S1 and S2, one 1-GPU replica each: only node 3 has room")
		e.model(msS1, modelS1, 1, 1)
		e.model(msS2, modelS2, 1, 1)
		e.waitReady(map[string]int32{depS1: 1, depS2: 1}, 5*time.Minute)
		Expect(e.podsByNode(depS1)).To(HaveKey(e.nodes[2]))

		By("Registering all three; G's floor is 2 until S1 reads short")
		e.register(soG, depG, modelG, 2, 2, "")
		e.register(soS1, depS1, modelS1, 1, 4, "")
		e.register(soS2, depS2, modelS2, 1, 2, "")
		e.load(loadS1, msS1, modelS1, 8)
	})

	It("lends one of G's nodes to S1: G's pod goes first, then S1 grows onto the node", func() {
		start := time.Now()
		By("Waiting until the controller sees S1 short of a whole replica")
		Eventually(func(g Gomega) {
			short, read := e.shortOf(modelS1, start.Add(-5*time.Minute))
			g.Expect(short).To(BeTrue(), read)
		}, 8*time.Minute, 10*time.Second).Should(Succeed())

		By("Lowering G's floor to 1, so it has a node to give")
		floorDropped := time.Now()
		e.lowerFloor(soG, depG, modelG, 2, "")

		By("Waiting for a transfer from G to S1")
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
			g.Expect(set.ID).NotTo(BeEmpty(), "no transfer from G to S1:\n%s", describeShare(lines, 40))
		}, 12*time.Minute, 5*time.Second).Should(Succeed())
		wnMeasure("lend: floor dropped to transfer started", set.Started.Sub(floorDropped))

		By("Waiting for G's pod to go, S1 to be raised and the transfer to complete")
		before := e.podsByNode(depG)
		Eventually(func(g Gomega) {
			lines := shareLines(controllerLogsSince(start), e.ns)
			for _, s := range setsTo(lines, modelS1) {
				if s.ID == set.ID {
					set = s
				}
			}
			g.Expect(set.Raised.IsZero()).To(BeFalse(), "S1 not raised yet:\n%s", describeShare(lines, 20))
			g.Expect(set.Outcome).To(Equal("done"), "transfer not done:\n%s", describeShare(lines, 20))
			g.Expect(e.readyReplicas(g, depS1)).To(BeNumerically(">=", 2))
		}, 10*time.Minute, 5*time.Second).Should(Succeed())
		after := e.podsByNode(depG)
		for n := range before {
			if after[n] == 0 {
				lentNode = n
			}
		}
		Expect(lentNode).NotTo(BeEmpty(), "G still runs on %v, as before (%v)", after, before)
		wnMeasure("lend: transfer started to S1 raised", set.Raised.Sub(set.Started))
		wnMeasure("lend: transfer started to done", set.Ended.Sub(set.Started))
		wnMeasure("lend: G pods by node after", after)

		By("Watching S1 grow into the lent node for two minutes")
		time.Sleep(2 * time.Minute)
		s1 := e.podsByNode(depS1)
		wnMeasure("lend: S1 pods by node two minutes after", s1)
		Expect(s1[lentNode]).To(BeNumerically(">=", 1), "S1 grew, but not onto the node G gave (%s): %v", lentNode, s1)
	})

	It("gives G a whole node back on its burst", func() {
		start := time.Now()
		By("Stopping S1's load and loading G")
		e.stopLoad(loadS1)
		e.load(loadG, msG, modelG, 16)
		wnMeasure("reclaim: S1 pods by node at G's burst", e.podsByNode(depS1))

		By("Waiting for G to run two replicas again")
		var lines []shareLine
		Eventually(func(g Gomega) {
			lines = shareLines(controllerLogsSince(start), e.ns)
			g.Expect(e.readyReplicas(g, depG)).To(Equal(int32(2)),
				"G not back to two replicas; S1 on %v, G pods %v\n%s", e.podsByNode(depS1), e.podsByNode(depG), describeShare(lines, 40))
		}, 25*time.Minute, 10*time.Second).Should(Succeed())
		wnMeasure("reclaim: G load to two Ready replicas", time.Since(start))
		wnMeasure("reclaim: S1 pods by node after", e.podsByNode(depS1))
		var how []string
		for _, s := range setsTo(lines, modelG) {
			how = append(how, fmt.Sprintf("set %s of %d (outcome %s, planned %v)", s.ID, len(s.Members), s.Outcome, s.Members[0].Planned))
		}
		for _, l := range withMsg(lines, "filling from idle GPUs") {
			if roleOf(l.Receiver, modelG) {
				how = append(how, "idle fill "+l.ID)
			}
		}
		for _, l := range withMsg(lines, "spare returned") {
			how = append(how, "return from "+l.Donor)
		}
		wnMeasure("reclaim: how G grew", strings.Join(how, "; "))
	})
})

// Small 1-GPU models fund the large model from one node, and the large
// model's long start is measured and paced on.
//
//   - Node 1 holds large model G, one 8-GPU replica, whose replicas take
//     startDelay to become Ready -- a large model's load. Node 2 holds four
//     replicas each of small models S1 and S2; node 3 holds their fifth, the
//     replica each keeps at its floor. Every other GPU is closed.
//   - One namespace quota of 18 GPUs: spent.
//   - G is loaded. The only way to a second G replica is to empty node 2:
//     eight 1-GPU donor replicas as one node-aware set.
var _ = Describe("Utilization share with a whole-node model: funded by small models", Label("utilization-share-wholenode"), Ordered, func() {
	const (
		quotaGPUs  = 18
		startDelay = 3 * time.Minute
		msG        = "uwf-g"
		msS1       = "uwf-s1"
		msS2       = "uwf-s2"
		modelG     = "uwf-model-g"
		modelS1    = "uwf-model-s1"
		modelS2    = "uwf-model-s2"
		loadG      = "uwf-g-load"
	)
	depG, depS1, depS2 := msG+"-decode", msS1+"-decode", msS2+"-decode"
	soG, soS1, soS2 := msG+"-so", msS1+"-so", msS2+"-so"
	var e *wholeNodeEnv
	var packed string
	var setUp time.Time

	BeforeAll(func() {
		setUp = time.Now()
		e = setUpWholeNode("uwf-", 3)
		restore, err := fixtures.SetNamespaceQuota(ctx, k8sClient, cfg.LLMSMNamespace, scalingPolicyConfigMapName(),
			e.ns, e.product, quotaGPUs, fixtures.WithOptimizer(map[string]any{"type": "utilizationShare"}))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = restore(ctx) })
		packed = layOutPacked(e, msG, modelG, msS1, modelS1, msS2, modelS2, fixtures.WithStartDelay(startDelay))

		By("Registering all three; S1 and S2 are held at five until G reads short")
		// G's long start is seeded, as a real whole-node model's is: until
		// three of its pods have started, the published estimate blends
		// the seed with the measurement (learner PR 5c), and one replica
		// unseeded would publish (2 x 70 + 180) / 3 = 107 s. A seed of
		// startDelay makes the gauge read the seed whether or not G is
		// measured, so the spec below also asserts the measured path. An
		// unseeded G's blend is covered by the saturation unit tests
		// (start_blend_test.go) and the learner replay goldens.
		e.register(soG, depG, modelG, 1, 2, "",
			fixtures.WithReplicaStartSeconds(strconv.Itoa(int(startDelay.Seconds()))))
		e.register(soS1, depS1, modelS1, 5, 5, "")
		e.register(soS2, depS2, modelS2, 5, 5, "")
		e.load(loadG, msG, modelG, 16)
	})

	It("measures G's long start", func() {
		pc := wholeNodeProm()
		Eventually(func(g Gomega) {
			v, ok := shareGauge(pc, fmt.Sprintf(`max(llmsm_replica_start_seconds_estimate{exported_namespace=%q,variant_name=~%q})`, e.ns, msG+"-.*"))
			g.Expect(ok).To(BeTrue(), "no start estimate for G yet")
			g.Expect(v).To(BeNumerically(">=", 0.8*startDelay.Seconds()), "G's start estimate")
			wnMeasure("long start: G start estimate (s)", v)
		}, 8*time.Minute, 15*time.Second).Should(Succeed())

		By("G's start was measured, not only seeded")
		Eventually(func(g Gomega) {
			// The gauge's source leaves seed once a pod of G is measured:
			// blended for one or two pods, measured from three.
			_, measured := shareGauge(pc, fmt.Sprintf(
				`max(llmsm_replica_start_seconds_estimate{exported_namespace=%q,variant_name=~%q,source=~"measured|blended"})`,
				e.ns, msG+"-.*"))
			g.Expect(measured).To(BeTrue(), "G's start estimate is still the seed: no pod of G was measured")
			// And the measurement itself is G's long start: the line's
			// pods and sizedSeconds, which is never under a measured start.
			line, found := lastStartMeasured(controllerLogsSince(setUp), e.ns, msG)
			g.Expect(found).To(BeTrue(), "no replica-start-measured line names G")
			g.Expect(line.Pods).To(BeNumerically(">=", 1))
			g.Expect(line.SizedSeconds).To(BeNumerically(">=", 0.8*startDelay.Seconds()), "G's measured start")
		}, 2*time.Minute, 15*time.Second).Should(Succeed())
	})

	It("empties node 2 for G with one set of S1 and S2 pods, then raises G onto it", func() {
		start := time.Now()
		By("Waiting until the controller sees G short of a whole replica")
		Eventually(func(g Gomega) {
			short, read := e.shortOf(modelG, start.Add(-10*time.Minute))
			g.Expect(short).To(BeTrue(), read)
		}, 8*time.Minute, 10*time.Second).Should(Succeed())

		By("Lowering S1's and S2's floors to 1, so they have pods to give")
		floorDropped := time.Now()
		e.lowerFloor(soS1, depS1, modelS1, 5, "")
		e.lowerFloor(soS2, depS2, modelS2, 5, "")

		By("Waiting for the transfers that fund a G replica to complete")
		var sets []transferSet
		var lines []shareLine
		Eventually(func(g Gomega) {
			lines = shareLines(controllerLogsSince(start), e.ns)
			sets = setsTo(lines, modelG)
			done := 0
			for _, s := range sets {
				if s.Outcome == "done" {
					done++
				}
			}
			g.Expect(done).To(BeNumerically(">=", 1), "no transfer to G has completed:\n%s", describeShare(lines, 40))
		}, 15*time.Minute, 5*time.Second).Should(Succeed())
		for _, s := range sets {
			donors := map[string]int{}
			planned := 0
			for _, m := range s.Members {
				donors[m.Donor]++
				if m.Planned {
					planned++
				}
			}
			wnMeasure("fund: set "+s.ID, fmt.Sprintf("%d members %v, %d planned, skippedWaits %v, outcome %q, start +%s, raised +%s",
				len(s.Members), donors, planned, s.Members[0].SkippedWaits, s.Outcome,
				s.Started.Sub(floorDropped).Round(time.Second), s.Raised.Sub(floorDropped).Round(time.Second)))
		}
		var raised time.Time
		for _, s := range sets {
			if s.Outcome == "done" && raised.IsZero() {
				raised = s.Raised
			}
		}

		By("Waiting for G's second replica to be Ready, on node 2")
		Eventually(func(g Gomega) {
			g.Expect(e.readyReplicas(g, depG)).To(Equal(int32(2)), unreadyPods(e.ns))
		}, startDelay+5*time.Minute, 5*time.Second).Should(Succeed())
		ready := time.Now()
		wnMeasure("fund: floors dropped to G raised", raised.Sub(floorDropped))
		wnMeasure("fund: G raised to second replica Ready", ready.Sub(raised))
		Expect(ready.Sub(raised)).To(BeNumerically(">=", startDelay-30*time.Second),
			"G's replica was Ready sooner than its start delay; the start was not emulated")
		Expect(e.podsByNode(depG)).To(HaveKeyWithValue(packed, 1), "G's new pod is not on the node S1 and S2 gave")
		Expect(lines).NotTo(ContainElement(HaveField("Outcome", "wrong-pod")), "a donor lost a pod it was not asked for")

		pc := wholeNodeProm()
		for _, p := range []string{"release-timeout", "fill-timeout", "reversal-hold", "swing-window"} {
			v, ok := shareGauge(pc, fmt.Sprintf(`max(llmsm_utilization_share_effective_seconds{scope=%q,param=%q})`, e.ns, p))
			if ok {
				wnMeasure("long start: effective "+p+" (s)", v)
			}
		}

		By("G, a whole-node model at no PriorityClass, is named by the controller's warning (issue #234)")
		Expect(controllerLogsSince(setUp)).To(MatchRegexp(`set no priorityClassName.*`+e.ns+"/"+msG),
			"no whole-node priority warning names G")
	})
})

// layOutPacked builds the packed layout: G, one 8-GPU replica, alone on node
// 1; four 1-GPU replicas each of S1 and S2 filling node 2; and their fifth
// replicas on node 3. It returns node 2. Every other GPU is closed. The order
// of creation decides placement: G first, while only nodes 1 and 3 have room
// and node 3 too little; then one replica each of S1 and S2, onto node 3; then
// node 2 opens and S1 and S2 grow to five, onto it.
func layOutPacked(e *wholeNodeEnv, msG, modelG, msS1, modelS1, msS2, modelS2 string, gOpts ...fixtures.ModelServiceOption) string {
	GinkgoHelper()
	depG, depS1, depS2 := msG+"-decode", msS1+"-decode", msS2+"-decode"
	By("Closing every GPU but node 1 whole and two GPUs on node 3")
	held := e.closeAllBut(map[string]int{e.nodes[0]: 8, e.nodes[2]: 2})
	By("Creating G, one 8-GPU replica: only node 1 holds it")
	e.model(msG, modelG, 1, 8, gOpts...)
	e.waitPlaced(depG, 1)
	By("Creating S1 and S2, one 1-GPU replica each: only node 3 has room")
	e.model(msS1, modelS1, 1, 1)
	e.model(msS2, modelS2, 1, 1)
	e.waitReady(map[string]int32{depS1: 1, depS2: 1}, 5*time.Minute)
	Expect(e.podsByNode(depS1)).To(Equal(map[string]int{e.nodes[2]: 1}))
	Expect(e.podsByNode(depS2)).To(Equal(map[string]int{e.nodes[2]: 1}))

	By("Opening node 2 and growing S1 and S2 to five: four each land on node 2")
	for _, p := range held[e.nodes[1]] {
		Expect(k8sClient.CoreV1().Pods(e.fillerNS).Delete(ctx, p, metav1.DeleteOptions{GracePeriodSeconds: new(int64)})).To(Succeed())
	}
	Eventually(func(g Gomega) {
		leaving, err := fixtures.TerminatingPodsOnNodes(ctx, k8sClient, e.key, e.product)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(leaving).To(BeEmpty(), "pods still terminating on the product's nodes, by namespace: %v", leaving)
	}, 5*time.Minute, 2*time.Second).Should(Succeed())
	e.scale(depS1, 5)
	e.scale(depS2, 5)
	e.waitReady(map[string]int32{depG: 1, depS1: 5, depS2: 5}, 10*time.Minute)
	Expect(e.podsByNode(depG)).To(Equal(map[string]int{e.nodes[0]: 1}))
	Expect(e.podsByNode(depS1)).To(Equal(map[string]int{e.nodes[1]: 4, e.nodes[2]: 1}))
	Expect(e.podsByNode(depS2)).To(Equal(map[string]int{e.nodes[1]: 4, e.nodes[2]: 1}))
	return e.nodes[1]
}

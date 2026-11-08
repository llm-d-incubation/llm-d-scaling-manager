package e2e

import (
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/llm-d-incubation/llm-d-scaling-manager/test/e2e/fixtures"
)

// A fragmented cluster: the GPUs a large replica needs exist in the quota and
// in total, but on no one node.
//
//   - Node 1 holds large model G, one 8-GPU replica. Nodes 2 and 3 each hold
//     four 1-GPU replicas of small models S1 and S2 beside four GPUs of pause
//     pods outside the scaling manager; node 4 holds the last two of their ten replicas.
//     Every other GPU is closed.
//   - One namespace quota of 18 GPUs: spent. It holds a second G replica once
//     S1 and S2 are at their floors of one, so G's target is two replicas,
//     and the small donors can pay its 8 GPUs of quota.
//   - G is loaded. No node can be emptied: the optimizer must plan nothing,
//     say why, and leave S1 and S2 their spare -- returned, it would sit
//     unassigned in the quota, since no node holds G either way.
//   - Then the optimizer is switched off and the quota raised to 26: today's
//     optimizer orders a G replica that no node can hold.
var _ = Describe("Utilization share with a whole-node model: a fragmented cluster", Label("utilization-share-wholenode"), Ordered, func() {
	const (
		quotaGPUs = 18
		msG       = "uwx-g"
		msS1      = "uwx-s1"
		msS2      = "uwx-s2"
		modelG    = "uwx-model-g"
		modelS1   = "uwx-model-s1"
		modelS2   = "uwx-model-s2"
		loadG     = "uwx-g-load"
	)
	depG, depS1, depS2 := msG+"-decode", msS1+"-decode", msS2+"-decode"
	soG, soS1, soS2 := msG+"-so", msS1+"-so", msS2+"-so"
	var e *wholeNodeEnv

	BeforeAll(func() {
		e = setUpWholeNode("uwx-", 4)
		restore, err := fixtures.SetNamespaceQuota(ctx, k8sClient, cfg.LLMSMNamespace, scalingPolicyConfigMapName(),
			e.ns, e.product, quotaGPUs, fixtures.WithOptimizer(map[string]any{"type": "utilizationShare"}))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = restore(ctx) })

		By("Closing every GPU but node 1 whole and four GPUs each on nodes 2 and 3 and two on node 4")
		e.closeAllBut(map[string]int{e.nodes[0]: 8, e.nodes[1]: 4, e.nodes[2]: 4, e.nodes[3]: 2})
		e.model(msG, modelG, 1, 8)
		e.waitPlaced(depG, 1)
		e.model(msS1, modelS1, 5, 1)
		e.model(msS2, modelS2, 5, 1)
		e.waitReady(map[string]int32{depG: 1, depS1: 5, depS2: 5}, 5*time.Minute)
		small := map[string]int{}
		for _, dep := range []string{depS1, depS2} {
			for n, c := range e.podsByNode(dep) {
				small[n] += c
			}
		}
		Expect(small).To(Equal(map[string]int{e.nodes[1]: 4, e.nodes[2]: 4, e.nodes[3]: 2}))
		wnMeasure("fragmented: S1 pods by node", e.podsByNode(depS1))
		wnMeasure("fragmented: S2 pods by node", e.podsByNode(depS2))

		By("Registering all three; S1 and S2 are held at five until G reads short")
		e.register(soG, depG, modelG, 1, 2, "")
		e.register(soS1, depS1, modelS1, 5, 5, "")
		e.register(soS2, depS2, modelS2, 5, 5, "")
		e.load(loadG, msG, modelG, 16)
	})

	// No node can hold a G replica, even if every small pod went: G reports
	// no-node-fits. S1 and S2 hold spare replicas above their targets of one
	// and are idle, but G is the only model below its target and no node can
	// hold its replica, so nothing is returned: the returned GPUs would only
	// sit unassigned. S1 and S2 keep their five replicas. The cause does not
	// change, so neither does the reason: once reported, no-node-fits is in
	// every reading (issue ev-shindin/llm-scaling-manager#236).
	It("starts no transfer for G, reports why, and returns no spare for it", func() {
		start := time.Now()
		wholeNodeProm()
		Eventually(func(g Gomega) {
			short, read := e.shortOf(modelG, start.Add(-5*time.Minute))
			g.Expect(short).To(BeTrue(), read)
		}, 8*time.Minute, 10*time.Second).Should(Succeed())
		By("Lowering S1's and S2's floors to 1, so they have replicas to give")
		e.lowerFloor(soS1, depS1, modelS1, 5, "")
		e.lowerFloor(soS2, depS2, modelS2, 5, "")
		floorDropped := time.Now()

		By("Holding eight minutes: nothing is given for G, G orders no replica, and says why")
		e.holdUnfunded("fragmented", modelG, depG, []string{depS1, depS2}, start, floorDropped, 8*time.Minute, false)

		By("S1 and S2 kept their spare: nothing was returned for a G no node can hold")
		lines := shareLines(controllerLogsSince(start), e.ns)
		Expect(withMsg(lines, "spare returned")).To(BeEmpty(), describeShare(lines, 40))
		Expect(e.specReplicas(Default, depS1)).To(Equal(int32(5)))
		Expect(e.specReplicas(Default, depS2)).To(Equal(int32(5)))
	})

	It("without the optimizer, orders a G replica that no node can hold", func() {
		By("Switching the optimizer off and raising the quota to 26")
		_, err := fixtures.SetNamespaceQuota(ctx, k8sClient, cfg.LLMSMNamespace, scalingPolicyConfigMapName(),
			e.ns, e.product, 26, fixtures.WithoutOptimizer())
		Expect(err).NotTo(HaveOccurred())
		switched := time.Now()

		By("Waiting for G to be raised and its new pod to wait for a node")
		var why string
		Eventually(func(g Gomega) {
			g.Expect(e.specReplicas(g, depG)).To(Equal(int32(2)))
			pods, err := k8sClient.CoreV1().Pods(e.ns).List(ctx, metav1.ListOptions{LabelSelector: "app=" + depG})
			g.Expect(err).NotTo(HaveOccurred())
			pending := 0
			for i := range pods.Items {
				if pods.Items[i].Spec.NodeName == "" {
					pending++
					why = unschedulableReason(&pods.Items[i])
				}
			}
			g.Expect(pending).To(Equal(1))
		}, 8*time.Minute, 10*time.Second).Should(Succeed())
		wnMeasure("fragmented, no optimizer: switch to G raised", time.Since(switched))
		wnMeasure("fragmented, no optimizer: pending G pod", why)
		Consistently(func(g Gomega) {
			g.Expect(e.readyReplicas(g, depG)).To(Equal(int32(1)))
		}, 3*time.Minute, 15*time.Second).Should(Succeed())
		wnMeasure("fragmented, no optimizer: S1 pods by node at the end", e.podsByNode(depS1))
		wnMeasure("fragmented, no optimizer: S2 pods by node at the end", e.podsByNode(depS2))
	})
})

// A node released for the large model, with a pod outside the scaling manager waiting for a
// GPU on it, and the large model at a higher PriorityClass (issue ev-shindin/llm-scaling-manager#234).
//
//   - The packed layout: G on node 1; four replicas each of S1 and S2 on node
//     2, their fifth on node 3. Quota 18, spent. G runs at a PriorityClass
//     above the pod outside the scaling manager, which runs at none.
//   - A pause pod outside the scaling manager waits for one GPU on node 2.
//   - G is loaded and the small models' floors drop: their pods on node 2
//     go, and the waiting pod takes the first GPU that frees. G's raised
//     replica preempts it and lands on node 2: the transfer completes, and G
//     is funded. Without the class the waiting pod keeps the GPU, the
//     transfer ends fill-timeout with release-taken, and G's raise is undone.
var _ = Describe("Utilization share with a whole-node model: a released node taken", Label("utilization-share-wholenode"), Ordered, func() {
	const (
		quotaGPUs = 18
		msG       = "uwt-g"
		msS1      = "uwt-s1"
		msS2      = "uwt-s2"
		modelG    = "uwt-model-g"
		modelS1   = "uwt-model-s1"
		modelS2   = "uwt-model-s2"
		loadG     = "uwt-g-load"
		priorityG = "uwt-share-receiver"
	)
	depG, depS1, depS2 := msG+"-decode", msS1+"-decode", msS2+"-decode"
	soG, soS1, soS2 := msG+"-so", msS1+"-so", msS2+"-so"
	var e *wholeNodeEnv
	var packed string
	var setUp time.Time

	BeforeAll(func() {
		setUp = time.Now()
		e = setUpWholeNode("uwt-", 3)
		restore, err := fixtures.SetNamespaceQuota(ctx, k8sClient, cfg.LLMSMNamespace, scalingPolicyConfigMapName(),
			e.ns, e.product, quotaGPUs, fixtures.WithOptimizer(map[string]any{"type": "utilizationShare"}))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = restore(ctx) })
		removePC, err := fixtures.CreatePriorityClass(ctx, k8sClient, priorityG, 1000)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = removePC(ctx) })
		packed = layOutPacked(e, msG, modelG, msS1, modelS1, msS2, modelS2, fixtures.WithPriorityClass(priorityG))
		e.register(soG, depG, modelG, 1, 2, "")
		e.register(soS1, depS1, modelS1, 5, 5, "")
		e.register(soS2, depS2, modelS2, 5, 5, "")
		e.load(loadG, msG, modelG, 16)
	})

	It("preempts the pod that took the released GPU, completes the transfer and funds G", func() {
		start := time.Now()
		Eventually(func(g Gomega) {
			short, read := e.shortOf(modelG, start.Add(-5*time.Minute))
			g.Expect(short).To(BeTrue(), read)
		}, 8*time.Minute, 10*time.Second).Should(Succeed())

		By("Queueing a pod outside the scaling manager for one GPU on node 2")
		squatter, err := fixtures.QueueGPUPod(ctx, k8sClient, e.fillerNS, packed, e.gpuRN, 1)
		Expect(err).NotTo(HaveOccurred())
		Consistently(func(g Gomega) {
			p, err := k8sClient.CoreV1().Pods(e.fillerNS).Get(ctx, squatter, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(p.Spec.NodeName).To(BeEmpty(), "node 2 had room before any release")
		}, 20*time.Second, 5*time.Second).Should(Succeed())

		By("Lowering S1's and S2's floors to 1")
		e.lowerFloor(soS1, depS1, modelS1, 5, "")
		e.lowerFloor(soS2, depS2, modelS2, 5, "")
		floorDropped := time.Now()

		By("Watching the queued pod take a released GPU, then waiting for the transfer to G to end")
		var squatterBound bool
		var lines []shareLine
		var set transferSet
		Eventually(func(g Gomega) {
			if p, err := k8sClient.CoreV1().Pods(e.fillerNS).Get(ctx, squatter, metav1.GetOptions{}); err == nil && p.Spec.NodeName == packed {
				squatterBound = true
			}
			lines = shareLines(controllerLogsSince(start), e.ns)
			for _, s := range setsTo(lines, modelG) {
				if s.Outcome != "" {
					set = s
				}
			}
			g.Expect(set.Outcome).NotTo(BeEmpty(), "no transfer to G has ended:\n%s", describeShare(lines, 40))
		}, 20*time.Minute, 2*time.Second).Should(Succeed())
		wnMeasure("taken: set", fmt.Sprintf("%s, %d members, outcome %s, started +%s, raised +%s, ended +%s", set.ID, len(set.Members),
			set.Outcome, set.Started.Sub(floorDropped).Round(time.Second), set.Raised.Sub(floorDropped).Round(time.Second),
			set.Ended.Sub(floorDropped).Round(time.Second)))
		wnMeasure("taken: queued pod seen bound to node 2", squatterBound)
		// Without it G lands on a node nothing took, and "done" says nothing
		// about the class. The pod binds as the first GPU frees and stays
		// bound until G is raised, at least a planning cycle and a KEDA poll
		// later: far longer than one poll.
		Expect(squatterBound).To(BeTrue(), "the queued pod never took a released GPU on node 2: nothing was preempted")
		Expect(set.Outcome).To(Equal("done"), describeShare(lines, 40))
		for _, l := range lines {
			Expect(l.Reason).NotTo(Equal("release-taken"), "release-taken logged:\n%s", describeShare(lines, 40))
		}

		By("G runs two replicas, its new one on node 2, and the queued pod no longer holds a GPU there")
		Eventually(func(g Gomega) {
			g.Expect(e.readyReplicas(g, depG)).To(Equal(int32(2)), unreadyPods(e.ns))
			g.Expect(e.podsByNode(depG)).To(HaveKeyWithValue(packed, 1))
			p, err := k8sClient.CoreV1().Pods(e.fillerNS).Get(ctx, squatter, metav1.GetOptions{})
			if err == nil {
				g.Expect(p.Spec.NodeName).NotTo(Equal(packed), "the queued pod still holds a GPU on node 2")
			}
		}, 5*time.Minute, 5*time.Second).Should(Succeed())

		By("The scheduler preempted the queued pod")
		evs, err := k8sClient.CoreV1().Events(e.fillerNS).List(ctx, metav1.ListOptions{
			FieldSelector: "involvedObject.name=" + squatter + ",reason=Preempted"})
		Expect(err).NotTo(HaveOccurred())
		Expect(evs.Items).NotTo(BeEmpty(), "no Preempted event on the queued pod")
		wnMeasure("taken: preempted", evs.Items[0].Message)

		By("G, at a PriorityClass, is not named by the whole-node priority warning")
		Expect(controllerLogsSince(setUp)).NotTo(MatchRegexp(`set no priorityClassName.*`+e.ns+"/"+msG),
			"the controller warned about G, which runs at a PriorityClass")
	})
})

package e2e

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d-incubation/llm-d-scaling-manager/test/e2e/fixtures"
	testutils "github.com/llm-d-incubation/llm-d-scaling-manager/test/utils"
)

// Fast Model Actuation attribution.
//
// FMA splits a model server across two pods: a REQUESTER Deployment carrying the
// llm-d identity, which a scaler moves, and LAUNCHER pods owned by a
// LauncherConfig, which hold the GPU and run the engine. The ownerReferences gap
// is deliberate — FMA patches the provider's labels so no ReplicaSet adopts it —
// so the collector's owner walk ends at nothing and falls back to the pairing
// FMA declares on the pods themselves.
//
// Before that fallback existed, an FMA variant reported demand 0 under load: the
// only pods reporting engine metrics were attributed to no scale target, and the
// requester that WAS attributable runs no engine. A benchmark showed a variant
// flat at one replica through a 155-deep queue and read as healthy.
//
// The layout here is built without FMA installed, because the labels are the
// whole contract: a launcher pod owned by a kind the walk cannot follow, paired
// by dual-pods.llm-d.ai/dual to a requester pod that a Deployment owns. No CRD
// and no controller are required — walkOwnersUp stops at an unknown owner kind
// before it tries to fetch it, which is exactly the production path.
var _ = Describe("FMA launcher attribution", Label("full"), Label("fma"), Ordered, func() {
	const (
		layoutName = "e2e-fma"
		modelID    = "default/default"
		modelLabel = "e2e-fma-model"
		maxNumSeqs = 4
	)

	var (
		ctx    context.Context
		layout *fixtures.FMALayout
	)

	BeforeAll(func() {
		ctx = context.Background()

		DeferCleanup(func() {
			_ = fixtures.DeleteFMALauncherPodMonitor(ctx, crClient, cfg.LLMDNamespace)
			_ = fixtures.DeleteFMALayout(ctx, k8sClient, cfg.LLMDNamespace, layout)
		})

		By("creating an FMA layout: a requester Deployment, a bound launcher, and one warm spare")
		var err error
		layout, err = fixtures.CreateFMALayout(ctx, k8sClient, cfg.LLMDNamespace,
			layoutName, modelLabel, modelID, maxNumSeqs)
		Expect(err).NotTo(HaveOccurred())

		By("scraping the launchers the way the shipped PodMonitor does")
		Expect(fixtures.EnsureFMALauncherPodMonitor(ctx, crClient, cfg.LLMDNamespace)).To(Succeed())

		// Without this the spec proves nothing. The scaling manager is only ever asked about
		// workloads KEDA calls it about, so with no ScaledObject there is no
		// registered variant, the collector never runs for this model, and no pod
		// — launcher or otherwise — is ever attributed. The first version of this
		// spec omitted it and failed for exactly that reason: five minutes of
		// waiting for a line the controller had no reason to write.
		By("registering the requester, which is what makes the scaling manager collect for this model at all")
		scalerAddress := "scaling-manager-external-scaler." + cfg.LLMSMNamespace + ".svc.cluster.local:9090"
		Expect(fixtures.EnsureScaledObject(ctx, crClient, cfg.LLMDNamespace,
			layout.RequesterDeployment, layout.RequesterDeployment, layout.RequesterDeployment+"-llmsm",
			1, 2, cfg.MonitoringNS,
			fixtures.WithScalingManagerTriggerMetadata(modelID, "10.0"),
			fixtures.WithExternalScalerTrigger(scalerAddress),
		)).To(Succeed())
		DeferCleanup(func() {
			_ = fixtures.DeleteScaledObject(ctx, crClient, cfg.LLMDNamespace, layout.RequesterDeployment)
		})
	})

	It("attributes a bound launcher to the requester's scale target", func() {
		// The controller says so once per cycle when the hop carries anything.
		// Asserting on the log rather than on a metric keeps this independent of
		// how long Prometheus takes to make a new series visible, and the line
		// names the pair so a failure says which one.
		Eventually(func() bool {
			ok, _, err := testutils.ControllerLogsContain(ctx, k8sClient, cfg.LLMSMNamespace,
				testutils.ControllerDeploymentName, "Attributed FMA launcher pods through their dual-pods pairing", 300)
			return err == nil && ok
		}, 5*time.Minute, 15*time.Second).Should(BeTrue(),
			"the collector never reported attributing a launcher through its pairing; "+
				"check that the launcher pod is a scrape target and that its dual-pods.llm-d.ai/dual "+
				"label names a pod whose ownerReferences reach a ScaledObject")
	})

	It("does not attribute a launcher with no bound instance", func() {
		// A warm spare must never be counted as capacity. It carries no pairing
		// label and no server-port annotation, so the PodMonitor's keep rule
		// drops it before a target is generated: it is not scraped, never reaches
		// the collector, and is therefore never attributed.
		//
		// The first version of this spec asserted the opposite — that the
		// controller would file it under reason=unbound_launcher. That reason
		// exists, but in a correctly configured namespace it is nearly
		// unreachable: classification happens only for pods that were scraped,
		// and the whole point of the keep rule is that a spare is not. It fires
		// only when something ELSE scrapes launchers, which is the case it was
		// written for. Asserting it here was asserting that our own scrape design
		// had failed.
		//
		// So the assertion is the absence: the spare never turns up in an
		// attribution line, however long we watch.
		//
		// Every poll must READ the log. A failed read (no controller Pod, or a
		// GetLogs error) has no launcher in it either, and used to pass. Nothing
		// restarts the controller during these specs, so no read error is
		// expected and none is tolerated.
		Consistently(func(g Gomega) {
			ok, _, err := testutils.ControllerLogsContain(ctx, k8sClient, cfg.LLMSMNamespace,
				testutils.ControllerDeploymentName, layout.UnboundLauncherPod, 600)
			g.Expect(err).NotTo(HaveOccurred(),
				"could not read the controller log, so the launcher's absence proves nothing")
			g.Expect(ok).To(BeFalse(),
				"the unbound launcher appeared in the controller's logs; a warm spare must be "+
					"neither scraped nor attributed, so it should be invisible to the collector entirely")
		}, 90*time.Second, 30*time.Second).Should(Succeed())
	})

	It("never files a bound launcher as unresolved", func() {
		// The negative that matters. `unresolved` means the walk reached nothing
		// AND no pairing rescued it — for a bound launcher that is a regression in
		// the hop, and it is silent: demand simply reads low.
		// Read strictly, for the reason given in the spec above.
		Consistently(func(g Gomega) {
			ok, _, err := testutils.ControllerLogsContain(ctx, k8sClient, cfg.LLMSMNamespace,
				testutils.ControllerDeploymentName,
				layout.LauncherPod+`", "instance`, 300)
			g.Expect(err).NotTo(HaveOccurred(),
				"could not read the controller log, so the skip message's absence proves nothing")
			g.Expect(ok).To(BeFalse(),
				"the bound launcher appeared in a skip message; the pairing hop is not resolving it")
		}, 60*time.Second, 20*time.Second).Should(Succeed())
	})
})

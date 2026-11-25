package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	promoperator "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	"github.com/prometheus/common/model"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
	"sigs.k8s.io/yaml"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/dsetwriter"
	"github.com/llm-d-incubation/llm-d-scaling-manager/test/e2e/fixtures"
	"github.com/llm-d-incubation/llm-d-scaling-manager/test/utils"
)

// The utilization-share optimizer in passive mode on a P/D model served by a
// DisaggregatedSet (docs/reference/disaggregatedset.md, "Passive mode";
// docs/guides/try-in-passive-mode). One namespace, a GPU quota it spends in
// full, three models:
//
//   - X, loaded: a DisaggregatedSet whose roles are External, scaled by KEDA
//     through their DisaggregatedSetRoleScalers. Prefill one 1-GPU pod per
//     group, decode two 1-GPU pods per group, one group each.
//   - S, idle: a DisaggregatedSet whose roles are Static, read and written by
//     the role writer. One 1-GPU prefill group, and two decode groups of two
//     1-GPU pods: one more decode group than it needs, the GPUs X's decode
//     needs.
//   - A, idle: a Deployment of three 1-GPU replicas, held at its floor of
//     three, so S is the only donor.
//
// The settings are the release settings ConfigMap (config/release), whose
// passive "true" is the release default, with the share keys the guide sets:
// limiter quota, a quota of exactly what the three hold, utilizationShare
// active. With the quota spent, X's decode can grow only by a transfer, and
// its would-be count stays capped at what it holds: the share's target is
// where the growth shows. The specs then check, in order:
//
//  1. the optimizer computes and publishes shares for the group, in shadow
//     because it holds a passive model, and the controller publishes a
//     would-be count for both role kinds, with PassiveRecommendation events;
//  2. over several minutes nothing moves: no role's replicas on either set,
//     no role scaler, no child LeaderWorkerSet, no Deployment, no pod, no
//     share mark, no transfer counted;
//  3. the control: with passive turned off, a transfer moves a decode group
//     from S (Static: the role writer lowers it) to X (External: KEDA raises
//     its role scaler). That shows the checks in 2 can see movement, so a
//     window that saw none means something.
//
// Not emulated here: whole 8-GPU nodes per replica and data-parallel ranks.
// The simulator serves one rank per pod, and the kind nodes are not
// partitioned into whole nodes (the utilization-share-wholenode suite covers
// whole-node packing for LeaderWorkerSets, not passive).
var _ = Describe("Passive mode: utilization share on a DisaggregatedSet P/D model", Serial, Label("full", "passive"), Ordered, func() {
	const (
		setX, setS = "dsetpas-x", "dsetpas-s"
		msA        = "dsetpas-a"
		loadJob    = "dsetpas-x-load"
		window     = int32(30)
		// holdFor is the "nothing moves" window: past KEDA's polling, three
		// HPA syncs, the 30 s scale-down stabilization, the role writer's
		// 15 s pass, and several share-planner cycles.
		holdFor = 5 * time.Minute
	)
	var (
		ns, product, key, gpuRes string
		modelX, modelS, modelA   string
		quotaGPUs                int
		began                    time.Time
		pc                       *utils.PrometheusClient
	)
	depA := msA + "-decode"
	soA := msA
	soXP, soXD := setX+"-prefill", setX+"-decode"
	variantXD := fixtures.ScaledObjectNameFor(soXD)
	variantSD := dsetwriter.EntryName(setS, domain.RoleDecode)
	roleApp := func(set, role string) string { return set + "-" + role + "-app" }

	BeforeAll(func() {
		began = time.Now()
		modelX, modelS, modelA = suiteModelID("dsetpas-x"), suiteModelID("dsetpas-s"), suiteModelID("dsetpas-a")
		requireDisaggregatedSetAPI()
		var ok bool
		key, product, gpuRes, ok = fixtures.DiscoverAccelerator(ctx, k8sClient)
		Expect(ok).To(BeTrue(), "no GPU product label on any schedulable node")
		// X: 1 + 2, S: 1 + 2*2, A: 3.
		quotaGPUs = 11
		Expect(fixtures.AllocatableGPUsForProduct(ctx, k8sClient, product)).To(BeNumerically(">=", quotaGPUs+2),
			"needs a node with %d %s GPUs: the quota, and the decode group the transfer adds", quotaGPUs+2, product)
		pc = passiveProm()

		nsObj, err := k8sClient.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "dsetpas-"},
		}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		ns = nsObj.Name
		DeferCleanup(func() {
			_ = k8sClient.CoreV1().Namespaces().Delete(context.Background(), ns, metav1.DeleteOptions{})
		})
		// scaling-manager-activate.yaml's grant: the share marks donor pods.
		Expect(fixtures.GrantPodWrites(ctx, k8sClient, ns, cfg.LLMSMNamespace)).To(Succeed())

		By("Turning the role writer on: its grant, and DSET_ROLE_WRITER=true")
		grantRoleWriter()
		original := setManagerConfigKey("DSET_ROLE_WRITER", "true")
		DeferCleanup(func() { restoreManagerConfig(original) })
		expectLog(time.Now().Add(-3*time.Minute), 2*time.Minute, "DisaggregatedSet role writer is on")

		By(fmt.Sprintf("Applying the release settings: passive as shipped, limiter quota, %s=%d, utilizationShare active", product, quotaGPUs))
		restoreSettings := applyReleaseSettings(map[string]string{
			config.SettingLimiter:          "quota",
			config.SettingQuotas:           fmt.Sprintf("%s=%d", product, quotaGPUs),
			config.SettingUtilizationShare: "active",
		})
		DeferCleanup(restoreSettings)

		By("Creating model A (a Deployment), and models X (External roles) and S (Static roles) as DisaggregatedSets")
		Expect(fixtures.EnsureModelService(ctx, k8sClient, ns, msA, msA+"-pool", modelA, cfg.UseSimulator, 2,
			fixtures.WithPoolGuide(passiveGuide),
			func(d *appsv1.Deployment) { d.Spec.Replicas = ptr.To(int32(3)) })).To(Succeed())
		guide := func(l *lwsv1.LeaderWorkerSet) {
			for _, t := range []*corev1.PodTemplateSpec{l.Spec.LeaderWorkerTemplate.LeaderTemplate, &l.Spec.LeaderWorkerTemplate.WorkerTemplate} {
				if t != nil {
					t.Labels["llm-d.ai/guide"] = passiveGuide
				}
			}
		}
		gpus := fixtures.WithLWSGPUs(key, product, gpuRes, 1)
		Expect(fixtures.CreateModelServiceDSet(ctx, crClient, ns, setX, modelX, cfg.UseSimulator, 2, nil, []fixtures.DSetRole{
			{Name: domain.RolePrefill, External: true, Size: 1, Replicas: 1},
			{Name: domain.RoleDecode, External: true, Size: 2, Replicas: 1},
		}, gpus, guide)).To(Succeed())
		// Created, so the create owns replicas: "force" lets the writer take them.
		Expect(fixtures.CreateModelServiceDSet(ctx, crClient, ns, setS, modelS, cfg.UseSimulator, 2, map[string]string{
			dsetwriter.OptInAnnotation:      "force",
			dsetwriter.ModelIDAnnotation:    modelS,
			constants.MinReplicasAnnotation: "1",
			constants.MaxReplicasAnnotation: "3",
		}, []fixtures.DSetRole{
			{Name: domain.RolePrefill, Size: 1, Replicas: 1},
			{Name: domain.RoleDecode, Size: 2, Replicas: 2},
		}, gpus, guide)).To(Succeed())
		DeferCleanup(func() {
			_ = fixtures.DeleteDisaggregatedSet(context.Background(), crClient, ns, setX)
			_ = fixtures.DeleteDisaggregatedSet(context.Background(), crClient, ns, setS)
		})

		type monitoredRole struct{ app, set, role string }
		monitored := map[string]monitoredRole{msA: {app: depA}}
		for _, set := range []string{setX, setS} {
			for _, role := range []string{domain.RolePrefill, domain.RoleDecode} {
				monitored[set+"-"+role] = monitoredRole{app: roleApp(set, role), set: set, role: role}
			}
		}
		for base, m := range monitored {
			var opts []fixtures.ServiceOption
			if m.set != "" {
				// The DisaggregatedSet controller sets the pods' app label
				// itself (<set>-<slice>-<role>), so the Service selects them
				// by the set's labels. Leaders only, as llm-d routes: a
				// request on a worker queues where no metric the scaling
				// manager reads can see it.
				opts = append(opts, func(s *corev1.Service) {
					s.Spec.Selector = map[string]string{
						constants.DisaggregatedSetNameLabelKey: m.set,
						constants.DisaggregatedSetRoleLabelKey: m.role,
						lwsv1.WorkerIndexLabelKey:              "0",
					}
				})
			}
			Expect(fixtures.EnsureService(ctx, k8sClient, ns, base, m.app, 8000, opts...)).To(Succeed())
			Expect(fixtures.EnsureServiceMonitor(ctx, crClient, cfg.MonitoringNS, ns, base, m.app)).To(Succeed())
			monitor := base + "-monitor"
			DeferCleanup(func() {
				_ = crClient.Delete(context.Background(), &promoperator.ServiceMonitor{
					ObjectMeta: metav1.ObjectMeta{Name: monitor, Namespace: cfg.MonitoringNS},
				})
			})
		}

		By("Waiting for every replica and every group to be Ready, and for X's role scalers")
		Eventually(func(g Gomega) {
			a, err := k8sClient.AppsV1().Deployments(ns).Get(ctx, depA, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(a.Status.ReadyReplicas).To(Equal(int32(3)), unreadyPods(ns))
			// From the child LeaderWorkerSets: LWS v0.10.0 writes no status on the set.
			x, err := fixtures.ChildReadyGroups(ctx, crClient, ns, setX)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(x).To(Equal(map[string]int64{domain.RolePrefill: 1, domain.RoleDecode: 1}), unreadyPods(ns))
			s, err := fixtures.ChildReadyGroups(ctx, crClient, ns, setS)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(s).To(Equal(map[string]int64{domain.RolePrefill: 1, domain.RoleDecode: 2}), unreadyPods(ns))
			for _, rs := range []string{soXP, soXD} {
				_, status, err := fixtures.RoleScalerReplicas(ctx, crClient, ns, rs)
				g.Expect(err).NotTo(HaveOccurred(), "the LeaderWorkerSet controller creates role scaler %s for an External role", rs)
				g.Expect(status).To(Equal(int64(1)))
			}
		}, time.Duration(cfg.PodReadyTimeout)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())

		By("Registering A and X's two roles with KEDA (the scaling manager's external scaler); A's floor is its count, so it gives nothing")
		Expect(fixtures.EnsureScaledObject(ctx, crClient, ns, soA, depA, depA, 3, 3, cfg.MonitoringNS,
			fixtures.WithScalingManagerTriggerMetadata(modelA, "10.0"),
			fixtures.WithExternalScalerTrigger(externalScalerAddress()),
			fixtures.WithScaledObjectScaleDownStabilizationWindow(window))).To(Succeed())
		for _, rs := range []string{soXP, soXD} {
			Expect(fixtures.EnsureScaledObject(ctx, crClient, ns, rs, rs, rs, 1, 3, cfg.MonitoringNS,
				fixtures.WithScaledObjectScaleTargetKind(constants.DisaggregatedSetRoleScalerKind),
				fixtures.WithScalingManagerTriggerMetadata(modelX, "10.0"),
				fixtures.WithExternalScalerTrigger(externalScalerAddress()),
				fixtures.WithScaledObjectScaleDownStabilizationWindow(window))).To(Succeed())
		}
		DeferCleanup(func() {
			for _, so := range []string{soA, soXP, soXD} {
				_ = fixtures.DeleteScaledObject(context.Background(), crClient, ns, so)
			}
		})
		for _, w := range []struct{ target, so string }{{depA, soA}, {soXP, soXP}, {soXD, soXD}} {
			waitForKEDAExternalMetricWired(ns, w.target, w.so)
		}

		By("Loading X's decode well past one group's capacity")
		job := buildThroughputSustainedLoadJob(ns, loadJob, fmt.Sprintf("http://%s-service:8000/v1/completions", soXD),
			modelX, 8, int64(3600))
		_, err = k8sClient.BatchV1().Jobs(ns).Create(ctx, job, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
	})

	It("computes and publishes the shares in shadow, and would-be counts for both role kinds", func() {
		sel := fmt.Sprintf(`exported_namespace=%q`, ns)
		Eventually(func(g Gomega) {
			g.Expect(promValue(pc, fmt.Sprintf(`max(llmsm_utilization_share_passive_shadow{scope=%q})`, ns))).To(Equal(1.0),
				"the group is planned in shadow because it holds a passive model")
			for _, m := range []string{modelX, modelS, modelA} {
				g.Expect(promValue(pc, fmt.Sprintf(`count(llmsm_utilization_share_target_gpus{%s,model_name=%q})`, sel, m))).To(BeNumerically(">", 0),
					"no target_gpus for %s", m)
				g.Expect(promValue(pc, fmt.Sprintf(`count(llmsm_utilization_share_actionable{%s,model_name=%q})`, sel, m))).To(BeNumerically(">", 0),
					"no actionable for %s", m)
			}
			// External: KEDA asks. The quota is spent, so the would-be count
			// stays at the one group; the share would give X's decode more
			// than its 2 GPUs, and a move from S would fix it.
			g.Expect(promValue(pc, fmt.Sprintf(`max(llmsm_desired_replicas{%s,variant_name=%q})`, sel, variantXD))).To(BeNumerically(">=", 1),
				"no would-be count for X's decode")
			g.Expect(promValue(pc, fmt.Sprintf(`max(llmsm_utilization_share_target_gpus{%s,model_name=%q,role="decode"})`, sel, modelX))).To(BeNumerically(">", 2),
				"the share does not yet give X's decode more than the 2 GPUs it holds")
			g.Expect(promValue(pc, fmt.Sprintf(`max(llmsm_utilization_share_actionable{%s,model_name=%q,role="decode"})`, sel, modelX))).To(Equal(1.0),
				"X's decode is not yet one a move would fix")
			g.Expect(promValue(pc, fmt.Sprintf(`max(llmsm_passive_mode{%s,variant_name=%q,mode="hold"})`, sel, variantXD))).To(Equal(1.0))
			// Static: the role writer registers it; the would-be count is below.
			d := promValue(pc, fmt.Sprintf(`max(llmsm_desired_replicas{%s,variant_name=%q})`, sel, variantSD))
			g.Expect(d).To(BeNumerically(">=", 0), "no would-be count for S's decode, registered by the role writer")
			g.Expect(d).To(BeNumerically("<", 2), "S's decode: the controller does not yet want fewer than its two groups")
			g.Expect(promValue(pc, fmt.Sprintf(`max(llmsm_passive_mode{%s,variant_name=%q,mode="hold"})`, sel, variantSD))).To(Equal(1.0))
		}, 8*time.Minute, 10*time.Second).Should(Succeed())

		// The event comes with a would-be count that differs from the held
		// one: S's decode's. X's is capped at what it holds by the spent
		// quota, so its growth shows in the share's target, not in an event.
		// A Static role has no ScaledObject; its events name the registry
		// entry <set>-<role>-llmsm as one (variant.EventTarget).
		Eventually(func(g Gomega) {
			evs, err := k8sClient.CoreV1().Events(ns).List(ctx, metav1.ListOptions{
				FieldSelector: "reason=" + constants.K8SEventPassiveRecommendation + ",involvedObject.name=" + variantSD,
			})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(evs.Items).NotTo(BeEmpty(), "no PassiveRecommendation event for %s", variantSD)
			GinkgoWriter.Printf("DSET-PASSIVE-EVIDENCE event %s: %s\n", variantSD, evs.Items[0].Message)
		}, 2*time.Minute, 10*time.Second).Should(Succeed())

		// Logged when the roles a move would fix change, not every cycle.
		Eventually(func(g Gomega) {
			var line string
			for _, l := range strings.Split(controllerLogsSince(began), "\n") {
				if strings.Contains(l, "Utilization share: would rebalance (shadow: the group holds a passive model)") &&
					strings.Contains(l, `"scope": "`+ns+`"`) {
					line = l
				}
			}
			g.Expect(line).NotTo(BeEmpty(), "no would-rebalance line for %s", ns)
			GinkgoWriter.Printf("DSET-PASSIVE-EVIDENCE %s\n", line)
		}, 2*time.Minute, 10*time.Second).Should(Succeed())

		for _, q := range []string{
			fmt.Sprintf(`llmsm_utilization_share_target_gpus{%s}`, sel),
			fmt.Sprintf(`llmsm_utilization_share_actionable{%s}`, sel),
			fmt.Sprintf(`llmsm_utilization_share_replicas_to_move{scope=%q}`, ns),
			fmt.Sprintf(`llmsm_desired_replicas{%s}`, sel),
			fmt.Sprintf(`llmsm_current_replicas{%s}`, sel),
		} {
			GinkgoWriter.Printf("DSET-PASSIVE-EVIDENCE %s:\n%s\n", q, promSeries(pc, q))
		}
	})

	It("moves nothing for several minutes", func() {
		rec := recordFleet(ns, []string{setX, setS}, []string{soXP, soXD}, depA)
		start := time.Now()
		time.Sleep(holdFor)
		samples := rec.Stop()
		distinct := map[string]bool{}
		for _, s := range samples {
			if s.err == "" {
				distinct[s.state] = true
			}
		}
		// One read every 2 s: a window with few good reads saw nothing.
		Expect(len(samples)).To(BeNumerically(">=", int(holdFor/(4*time.Second))), "too few reads of the fleet")
		Expect(distinct).To(HaveLen(1), "the fleet changed during the hold; timeline above")

		sel := fmt.Sprintf(`exported_namespace=%q`, ns)
		By("Checking the controller still wanted to move X and S at the end of the window")
		Expect(promValue(pc, fmt.Sprintf(`max(llmsm_utilization_share_target_gpus{%s,model_name=%q,role="decode"})`, sel, modelX))).To(BeNumerically(">", 2))
		Expect(promValue(pc, fmt.Sprintf(`max(llmsm_desired_replicas{%s,variant_name=%q})`, sel, variantSD))).To(BeNumerically("<", 2))
		Expect(promValue(pc, fmt.Sprintf(`max(llmsm_utilization_share_passive_shadow{scope=%q})`, ns))).To(Equal(1.0))

		By("Checking no transfer was counted, started or marked, and the role writer wrote nothing")
		Expect(promValue(pc, fmt.Sprintf(`sum(llmsm_utilization_share_transfers_total{scope=%q})`, ns))).To(BeNumerically("<=", 0))
		Expect(promValue(pc, fmt.Sprintf(`sum(llmsm_utilization_share_starts_total{scope=%q})`, ns))).To(BeNumerically("<=", 0))
		Expect(markedSharePods(Default, ns)).To(BeEmpty(), "a pod carries a share mark")
		pods, err := k8sClient.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
		Expect(err).NotTo(HaveOccurred())
		for _, p := range pods.Items {
			Expect(p.Annotations).NotTo(HaveKey(corev1.PodDeletionCost), "pod %s carries a deletion cost", p.Name)
		}
		evs, err := k8sClient.CoreV1().Events(ns).List(ctx, metav1.ListOptions{})
		Expect(err).NotTo(HaveOccurred())
		for _, e := range evs.Items {
			Expect(e.Reason).NotTo(Or(HavePrefix("UtilizationShare"), Equal(dsetwriter.EventScaled)),
				"event %s on %s/%s: %s", e.Reason, e.InvolvedObject.Kind, e.InvolvedObject.Name, e.Message)
		}
		logs := controllerLogsSince(start)
		for _, line := range strings.Split(logs, "\n") {
			if strings.Contains(line, ns) {
				Expect(line).NotTo(ContainSubstring("transfer started"))
			}
		}
	})

	It("control: with passive off, a transfer moves a decode group from S to X", func() {
		start := time.Now()
		By("Turning passive off in the settings ConfigMap")
		patchSettings(map[string]string{config.SettingPassive: "false"})

		By("Waiting for a transfer to start whose receiver is X's decode")
		receiver := regexp.MustCompile(`"scope": "` + ns + `".*"receiver": "[^"]*/` + regexp.QuoteMeta(modelX) + `/decode"`)
		Eventually(func(g Gomega) {
			logs := controllerLogsSince(start)
			found := ""
			for _, line := range strings.Split(logs, "\n") {
				if strings.Contains(line, "transfer started") && receiver.MatchString(line) {
					found = line
					break
				}
			}
			g.Expect(found).NotTo(BeEmpty(), "no transfer to X's decode has started\n%s", sharePlannerLines(logs, ns, modelX, 80))
			GinkgoWriter.Printf("DSET-PASSIVE-EVIDENCE %s\n", found)
			g.Expect(found).To(ContainSubstring("/"+modelS+"/decode"), "the donor is not S's decode")
		}, 12*time.Minute, 10*time.Second).Should(Succeed())

		By("Waiting for the role writer to lower S's decode, and KEDA to raise X's decode role scaler")
		Eventually(func(g Gomega) {
			s, err := fixtures.ReadDisaggregatedSet(ctx, crClient, ns, setS)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(s.Spec[domain.RoleDecode]).To(Equal(int64(1)), "S's decode spec.replicas")
			spec, status, err := fixtures.RoleScalerReplicas(ctx, crClient, ns, soXD)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(spec).To(BeNumerically(">=", 2), "role scaler %s spec.replicas", soXD)
			g.Expect(status).To(BeNumerically(">=", 2), "role scaler %s status.replicas", soXD)
			g.Expect(promValue(pc, fmt.Sprintf(`sum(llmsm_utilization_share_starts_total{scope=%q})`, ns))).To(BeNumerically(">", 0))
			GinkgoWriter.Printf("DSET-PASSIVE-EVIDENCE %s roles %v, %s spec=%d status=%d at %s\n",
				setS, s.Spec, soXD, spec, status, time.Now().UTC().Format(time.RFC3339))
		}, 15*time.Minute, 10*time.Second).Should(Succeed(), "%s", sharePlannerLines(controllerLogsSince(start), ns, modelX, 60))
	})
})

// requireDisaggregatedSetAPI fails, not skips, without the DisaggregatedSet
// role scaler: the passive tier's setup installs LeaderWorkerSet v0.10.0 for
// this spec, and a skip would let the coverage go missing unseen.
func requireDisaggregatedSetAPI() {
	GinkgoHelper()
	list, err := k8sClient.Discovery().ServerResourcesForGroupVersion(constants.DisaggregatedSetAPIVersion)
	Expect(err).NotTo(HaveOccurred(), "no %s API: install LeaderWorkerSet v0.10.0 or later (make test-e2e-passive-with-setup does, DEPLOY_LWS=true)",
		constants.DisaggregatedSetAPIVersion)
	names := make([]string, 0, len(list.APIResources))
	for _, r := range list.APIResources {
		names = append(names, r.Name)
	}
	Expect(names).To(ContainElements("disaggregatedsets", "disaggregatedsetrolescalers/scale"))
}

// dsetRoleWriterGrant is the role writer's grant as config/components/dset-role-writer
// makes it, under e2e names.
const dsetRoleWriterGrant = "e2e-dset-role-writer"

// grantRoleWriter gives the controller get, list and patch on
// DisaggregatedSets, cluster-wide, and removes it at cleanup.
func grantRoleWriter() {
	GinkgoHelper()
	role := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: dsetRoleWriterGrant, Labels: map[string]string{"test-resource": "true"}},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{"disaggregatedset.x-k8s.io"},
			Resources: []string{"disaggregatedsets"},
			Verbs:     []string{"get", "list", "patch"},
		}},
	}
	binding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: dsetRoleWriterGrant, Labels: map[string]string{"test-resource": "true"}},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: dsetRoleWriterGrant},
		Subjects: []rbacv1.Subject{{
			Kind: rbacv1.ServiceAccountKind, Name: utils.ControllerServiceAccountName, Namespace: cfg.LLMSMNamespace,
		}},
	}
	if _, err := k8sClient.RbacV1().ClusterRoles().Create(ctx, role, metav1.CreateOptions{}); err != nil && !errors.IsAlreadyExists(err) {
		Expect(err).NotTo(HaveOccurred())
	}
	if _, err := k8sClient.RbacV1().ClusterRoleBindings().Create(ctx, binding, metav1.CreateOptions{}); err != nil && !errors.IsAlreadyExists(err) {
		Expect(err).NotTo(HaveOccurred())
	}
	DeferCleanup(func() {
		_ = k8sClient.RbacV1().ClusterRoleBindings().Delete(context.Background(), dsetRoleWriterGrant, metav1.DeleteOptions{})
		_ = k8sClient.RbacV1().ClusterRoles().Delete(context.Background(), dsetRoleWriterGrant, metav1.DeleteOptions{})
	})
}

// setManagerConfigKey writes one key into config.yaml of the manager ConfigMap
// and restarts the controller, which reads it only at start. It returns the
// ConfigMap as it was, for restoreManagerConfig.
func setManagerConfigKey(key, value string) *corev1.ConfigMap {
	GinkgoHelper()
	cm, err := k8sClient.CoreV1().ConfigMaps(cfg.LLMSMNamespace).Get(ctx, passiveManagerConfigMap, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	original := cm.DeepCopy()
	settings := map[string]any{}
	Expect(yaml.Unmarshal([]byte(cm.Data["config.yaml"]), &settings)).To(Succeed())
	settings[key] = value
	out, err := yaml.Marshal(settings)
	Expect(err).NotTo(HaveOccurred())
	cm.Data["config.yaml"] = string(out)
	_, err = k8sClient.CoreV1().ConfigMaps(cfg.LLMSMNamespace).Update(ctx, cm, metav1.UpdateOptions{})
	Expect(err).NotTo(HaveOccurred())
	Expect(restartScalingManagerController(ctx)).To(Succeed())
	GinkgoWriter.Printf("DSET-PASSIVE-EVIDENCE %s=%s written to %s and the controller restarted at %s\n",
		key, value, passiveManagerConfigMap, time.Now().UTC().Format(time.RFC3339))
	return original
}

// releaseSettingsFile is the settings ConfigMap the release files ship.
var releaseSettingsFile = filepath.Join("..", "..", "config", "release", "settings-configmap.yaml")

// applyReleaseSettings writes the live keys of the release settings ConfigMap
// (passive among them, at the release default), overridden by set, into the
// controller's settings ConfigMap, the way the guide has an operator edit it.
// The restart keys are left as the suite installed them: they take effect
// only at start, and the shipped values (logVerbosity 2, scaleToZero false)
// would change the controller under the rest of the suite. It returns the
// restore.
func applyReleaseSettings(set map[string]string) func() {
	GinkgoHelper()
	raw, err := os.ReadFile(releaseSettingsFile)
	Expect(err).NotTo(HaveOccurred())
	var shipped corev1.ConfigMap
	Expect(yaml.Unmarshal(raw, &shipped)).To(Succeed())
	Expect(shipped.Data).To(HaveKeyWithValue(config.SettingPassive, "true"), "the release files install passive")
	data := map[string]string{}
	for _, k := range config.LiveSettingKeys {
		if v, ok := shipped.Data[k]; ok {
			data[k] = v
		}
	}
	for k, v := range set {
		data[k] = v
	}
	name := config.DefaultSettingsConfigMapName
	cms := k8sClient.CoreV1().ConfigMaps(cfg.LLMSMNamespace)
	existing, err := cms.Get(ctx, name, metav1.GetOptions{})
	existed := err == nil
	if err != nil && !errors.IsNotFound(err) {
		Expect(err).NotTo(HaveOccurred())
	}
	var original map[string]string
	if existed {
		original = existing.Data
	}
	patchSettings(data)
	GinkgoWriter.Printf("DSET-PASSIVE-EVIDENCE settings %v at %s\n", data, time.Now().UTC().Format(time.RFC3339))
	return func() {
		c, err := cms.Get(context.Background(), name, metav1.GetOptions{})
		if err != nil {
			return
		}
		if !existed {
			_ = cms.Delete(context.Background(), name, metav1.DeleteOptions{})
			return
		}
		c.Data = original
		_, _ = cms.Update(context.Background(), c, metav1.UpdateOptions{})
	}
}

// patchSettings merges data into the controller's settings ConfigMap,
// creating it when absent.
func patchSettings(data map[string]string) {
	GinkgoHelper()
	name := config.DefaultSettingsConfigMapName
	cms := k8sClient.CoreV1().ConfigMaps(cfg.LLMSMNamespace)
	Eventually(func(g Gomega) {
		c, err := cms.Get(ctx, name, metav1.GetOptions{})
		if errors.IsNotFound(err) {
			_, err = cms.Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: cfg.LLMSMNamespace}, Data: data},
				metav1.CreateOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			return
		}
		g.Expect(err).NotTo(HaveOccurred())
		if c.Data == nil {
			c.Data = map[string]string{}
		}
		for k, v := range data {
			c.Data[k] = v
		}
		_, err = cms.Update(ctx, c, metav1.UpdateOptions{})
		g.Expect(err).NotTo(HaveOccurred())
	}, 30*time.Second, time.Second).Should(Succeed())
}

// promSeries is every series of query, one line each, for the evidence log.
func promSeries(pc *utils.PrometheusClient, query string) string {
	qctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	v, _, _, err := pc.API().Query(qctx, query, time.Now())
	if err != nil {
		return "query failed: " + err.Error()
	}
	vec, ok := v.(model.Vector)
	if !ok {
		return "not a vector: " + v.String()
	}
	var lines []string
	for _, r := range vec {
		lines = append(lines, fmt.Sprintf("  %v = %v", r.Metric, r.Value))
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// fleetSample is one read of everything that could move.
type fleetSample struct {
	at    time.Duration
	state string
	err   string
}

// fleetRecorder reads the fleet every two seconds until stopped.
type fleetRecorder struct {
	mu      sync.Mutex
	samples []fleetSample
	stop    chan struct{}
	done    chan struct{}
	start   time.Time
}

// recordFleet samples, every two seconds: each set's spec.roles[].spec.replicas,
// each role scaler's spec.replicas, every child LeaderWorkerSet's
// spec.replicas, the Deployment's spec.replicas, and the names of the model
// pods (the load Job's excluded).
func recordFleet(ns string, sets, roleScalers []string, deployment string) *fleetRecorder {
	r := &fleetRecorder{stop: make(chan struct{}), done: make(chan struct{}), start: time.Now()}
	read := func() (string, error) {
		var parts []string
		for _, set := range sets {
			s, err := fixtures.ReadDisaggregatedSet(ctx, crClient, ns, set)
			if err != nil {
				return "", err
			}
			parts = append(parts, fmt.Sprintf("%s.roles=%v", set, s.Spec))
		}
		for _, rs := range roleScalers {
			spec, _, err := fixtures.RoleScalerReplicas(ctx, crClient, ns, rs)
			if err != nil {
				return "", err
			}
			parts = append(parts, fmt.Sprintf("%s.spec=%d", rs, spec))
		}
		var children lwsv1.LeaderWorkerSetList
		if err := crClient.List(ctx, &children, client.InNamespace(ns), client.HasLabels{constants.DisaggregatedSetNameLabelKey}); err != nil {
			return "", err
		}
		for _, l := range children.Items {
			parts = append(parts, fmt.Sprintf("lws/%s=%d", l.Name, ptr.Deref(l.Spec.Replicas, 1)))
		}
		dep, err := k8sClient.AppsV1().Deployments(ns).Get(ctx, deployment, metav1.GetOptions{})
		if err != nil {
			return "", err
		}
		parts = append(parts, fmt.Sprintf("%s=%d", deployment, ptr.Deref(dep.Spec.Replicas, 1)))
		pods, err := k8sClient.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: "!job-name"})
		if err != nil {
			return "", err
		}
		names := make([]string, 0, len(pods.Items))
		for _, p := range pods.Items {
			names = append(names, p.Name)
		}
		slices.Sort(names)
		sort.Strings(parts)
		return strings.Join(parts, " ") + " pods=" + strings.Join(names, ","), nil
	}
	go func() {
		defer GinkgoRecover()
		defer close(r.done)
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for {
			s := fleetSample{at: time.Since(r.start).Round(time.Second)}
			state, err := read()
			if err != nil {
				s.err = err.Error()
			} else {
				s.state = state
			}
			r.mu.Lock()
			r.samples = append(r.samples, s)
			r.mu.Unlock()
			select {
			case <-r.stop:
				return
			case <-t.C:
			}
		}
	}()
	return r
}

// Stop ends the recording, prints every change and the read errors, and
// returns the samples.
func (r *fleetRecorder) Stop() []fleetSample {
	close(r.stop)
	<-r.done
	r.mu.Lock()
	defer r.mu.Unlock()
	GinkgoWriter.Printf("DSET-PASSIVE-TIMELINE from %s, %d samples\n", r.start.UTC().Format(time.RFC3339), len(r.samples))
	prev := ""
	for i, s := range r.samples {
		if s.err != "" {
			GinkgoWriter.Printf("DSET-PASSIVE-TIMELINE +%-5s read failed: %s\n", s.at, s.err)
			continue
		}
		if s.state != prev || i == len(r.samples)-1 {
			GinkgoWriter.Printf("DSET-PASSIVE-TIMELINE +%-5s %s\n", s.at, s.state)
		}
		prev = s.state
	}
	return slices.Clone(r.samples)
}

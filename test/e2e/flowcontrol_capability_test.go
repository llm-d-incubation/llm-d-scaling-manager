package e2e

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/client/config"

	"github.com/llm-d-incubation/llm-d-scaling-manager/test/utils"
)

// Scale-from-zero has exactly one input: the EPP flow-control queue. If EPP is
// not queueing, the engine cannot wake anything and the suite is testing an
// absent precondition rather than the scaling manager.
//
// The gate used to be `if !cfg.ScaleToZeroEnabled { Skip }` -- it asserted that a
// FLAG was set, not that the capability existed. Those are different claims, and
// the gap between them hid a real problem for weeks: with the flag off every
// spec Skip()ped, and a runtime Skip reports as SUCCESS, so the suite was green
// while more than half of it never ran. Turning the flag on then produced
// failures that read as the scaling manager bugs but were nothing of the kind -- measured on
// kind, across 152 samples of a full spec run, llm_d_epp_flow_control_queue_size
// never registered at all while ServiceUnavailable climbed by 40. Flow control
// was not engaging; the scaling manager never had a signal to act on.
//
// So check the capability, and when it is missing say so precisely. A skip that
// names what is absent is worth more than a failure that points at the wrong
// component.

const (
	flowControlQueueMetric = "llm_d_epp_flow_control_queue_size"
	// The GIE name for the same series, still exported as a deprecated alias.
	flowControlQueueMetricAlias = "inference_extension_flow_control_queue_size"
	// The token scaling manager itself mounts to scrape EPP. EPP /metrics is authenticated:
	// an unauthenticated GET returns a one-line "Unauthorized" body, which is
	// easy to mistake for "the metric is absent".
	eppMetricsTokenSecret = "scaling-manager-epp-metrics-token"
)

// eppFlowControlAvailable reports whether the flow-control queue metric exists,
// and why not when it does not.
//
// Prometheus is asked first, and is the authority. It scrapes EPP from inside
// the cluster, which the test host cannot do: pod IPs are not routable from a
// kind host, so a direct scrape reports "could not scrape" and hides the real
// answer behind a plumbing failure. Asking Prometheus checks the capability from
// somewhere that can actually observe it.
//
// The direct scrape is kept only as a fallback for environments with no
// Prometheus reachable from the test host.
func eppFlowControlAvailable(ctx context.Context, c client.Client, scalingManagerNamespace, poolNamespace string) (bool, string) {
	// Ask the EPP process what it actually loaded, via its logs. This is the most
	// reliable of the three checks and the only one that works everywhere: the
	// API server proxies logs, whereas pod IPs are not routable from a kind host
	// and Prometheus needs a port-forward the suite does not set up.
	//
	// It also catches the failure that motivated this gate, which neither of the
	// others can: EPP reads --config-file ONCE at startup, so a ConfigMap that
	// gains featureGates: [flowControl] does not reach a pod already running.
	// Observed on kind -- the ConfigMap said flowControl, the process had parsed
	// no featureGates at all, and three separate investigations blamed the scaling manager for
	// the missing wake signal. Comparing the ConfigMap to the pod proves nothing;
	// only the process's own account of its config does.
	//
	// Its verdict is honoured in BOTH directions. Gating only on `why != ""` made a
	// positive finding fall through to the checks below, so the most reliable check
	// could only ever cause a skip and never prevent one — and the Prometheus check
	// then skipped a healthy cluster, because llm_d_epp_flow_control_queue_size is
	// created per flow, on demand, and so does not exist until something has
	// actually queued. Running one scale-from-zero suite on its own was enough to
	// hit that: 0 specs ran, and the run reported SUCCESS.
	if ok, why := eppFlowControlFromLogs(ctx, c, poolNamespace); ok || why != "" {
		return ok, why
	}
	if pc := promClientForCheck(); pc != nil {
		for _, name := range []string{flowControlQueueMetric, flowControlQueueMetricAlias} {
			// absent() yields 1 when the series does not exist and nothing when it
			// does. That is the distinction the whole check turns on -- a
			// value-based query cannot tell "no such metric" from "metric present,
			// value 0", and conflating those is exactly how this stayed hidden.
			v, err := pc.QueryWithRetry(ctx, fmt.Sprintf("absent(%s)", name))
			if err != nil {
				continue
			}
			if v == 0 {
				return true, ""
			}
		}
		// An absent series is NOT evidence that flow control is off, and treating it
		// as such used to skip healthy clusters: llm_d_epp_flow_control_queue_size is
		// created per flow, on demand, so before anything has queued it does not
		// exist on an EPP that is queueing perfectly well. That is the ordinary state
		// at the start of a run, and it gated every scale-from-zero suite. Reaching
		// here means the logs could not tell either, so fall through to the scrape
		// rather than skip on a failure to look.
	}
	return eppFlowControlAvailableByScrape(ctx, c, scalingManagerNamespace, poolNamespace)
}

// eppFlowControlAvailableByScrape is the fallback: read the metric off an EPP pod
// directly. Only usable where the test host can reach pod IPs.
func eppFlowControlAvailableByScrape(ctx context.Context, c client.Client, scalingManagerNamespace, poolNamespace string) (bool, string) {
	var secret corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{
		Name: eppMetricsTokenSecret, Namespace: scalingManagerNamespace,
	}, &secret); err != nil {
		return false, fmt.Sprintf("cannot read %s/%s: %v", scalingManagerNamespace, eppMetricsTokenSecret, err)
	}
	token := strings.TrimSpace(string(secret.Data["token"]))
	if token == "" {
		return false, eppMetricsTokenSecret + " carries no token"
	}

	var pods corev1.PodList
	if err := c.List(ctx, &pods, client.InNamespace(poolNamespace)); err != nil {
		return false, fmt.Sprintf("listing pods in %s: %v", poolNamespace, err)
	}

	var tried int
	for i := range pods.Items {
		p := &pods.Items[i]
		if !strings.Contains(p.Name, "epp") || p.Status.PodIP == "" {
			continue
		}
		tried++
		body, err := scrapeEPPMetrics(ctx, p.Status.PodIP, token)
		if err != nil {
			continue
		}
		if strings.Contains(body, flowControlQueueMetric) ||
			strings.Contains(body, flowControlQueueMetricAlias) {
			return true, ""
		}
		// Reached and readable, but the family is not there. Report what the
		// endpoint DID contain, so the reader can tell an unauthenticated
		// response apart from a genuinely absent metric.
		return false, fmt.Sprintf(
			"EPP %s exports no flow-control queue metric (%d bytes scraped). "+
				"Flow control is not engaging, so no wake signal can exist -- "+
				"this is an EPP/environment gap, not a scaling manager failure",
			p.Name, len(body))
	}
	// Could not determine it. Fail OPEN: run the specs.
	//
	// Skipping on ignorance is the wrong default and was actively harmful here --
	// none of the three checks can reach EPP from a kind test host (pod IPs are
	// not routable, Prometheus needs a port-forward the suite does not create,
	// and the startup log line may have rotated), so a working cluster reported
	// "could not scrape" and skipped all 49 specs while flow control was in fact
	// running. A spec that runs and fails leaves diagnostics; a spec that skips
	// leaves nothing, and reports SUCCESS while testing nothing.
	//
	// Skip only on positive evidence of absence -- which the log check above
	// provides when it can see the parsed config.
	_ = tried
	return true, ""
}

// eppFlowControlFromLogs reads the EPP's own account of the config it parsed.
//
// Returns ("", "") when it cannot tell -- no EPP pod, logs unavailable, or the
// startup line has already rotated out -- so the caller falls through to the
// other checks rather than skipping on a failure to look.
func eppFlowControlFromLogs(ctx context.Context, c client.Client, poolNamespace string) (bool, string) {
	cs, err := kubernetes.NewForConfig(ctrlconfig.GetConfigOrDie())
	if err != nil {
		return false, ""
	}
	var pods corev1.PodList
	if err := c.List(ctx, &pods, client.InNamespace(poolNamespace)); err != nil {
		return false, ""
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if !strings.Contains(p.Name, "epp") || p.Status.Phase != corev1.PodRunning {
			continue
		}
		req := cs.CoreV1().Pods(poolNamespace).GetLogs(p.Name, &corev1.PodLogOptions{
			Container: "epp",
			// Read from the START, capped -- the config line is the first thing
			// EPP logs. TailLines would be wrong: EPP is extremely chatty (121k
			// lines in a few seconds under load), so the startup line scrolls out
			// of any tail window almost immediately, and the check then reports
			// "cannot tell" on a perfectly healthy EPP.
			LimitBytes: ptr.To(int64(512 * 1024)),
		})
		stream, err := req.Stream(ctx)
		if err != nil {
			continue
		}
		body, readErr := io.ReadAll(stream)
		_ = stream.Close()
		if readErr != nil {
			continue
		}
		text := string(body)
		switch {
		case strings.Contains(text, `"flowControl"`), strings.Contains(text, "flow-control"):
			return true, ""
		case strings.Contains(text, "Raw config after phase one"):
			// It logged its config and flowControl was not in it. That is
			// conclusive, and almost always a pod older than the ConfigMap.
			return false, fmt.Sprintf(
				"EPP %s parsed no flowControl feature gate, so it is not queueing. "+
					"If the ConfigMap does enable it, the pod predates that edit -- EPP "+
					"reads --config-file once at startup. Restart the EPP deployment",
				p.Name)
		}
	}
	return false, ""
}

// promClientForCheck builds a Prometheus client the same way the other e2e
// helpers do. Returns nil when none can be built, so the caller falls back to a
// direct scrape rather than failing the suite over a missing convenience.
func promClientForCheck() *utils.PrometheusClient {
	url := os.Getenv("PROMETHEUS_URL")
	if url == "" {
		url = utils.DefaultPrometheusURL
	}
	insecure := true
	if v := os.Getenv("PROMETHEUS_SKIP_TLS_VERIFY"); v != "" {
		insecure = strings.EqualFold(v, "true")
	}
	pc, err := utils.NewPrometheusClient(url, insecure)
	if err != nil {
		return nil
	}
	return pc
}

func scrapeEPPMetrics(ctx context.Context, podIP, token string) (string, error) {
	url := fmt.Sprintf("http://%s:9090/metrics", podIP)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

package constants

// The KEDA HPA repair's metrics (internal/hparepair, KEDA_HPA_REPAIR). Absent
// while the repair is off.
const (
	// LLMSMKEDAUnwiredHPAs is a gauge: how many HPAs KEDA built for this
	// scaler's ScaledObjects carry no External metric for the scaler, as of
	// the last read of the namespace. Such an HPA scales on the CPU default,
	// so the scaler's decisions reach nothing. An HPA younger than the
	// repair's grace is not counted yet.
	// Labels: namespace (the HPA's; controller_instance when set)
	LLMSMKEDAUnwiredHPAs = "llmsm_keda_unwired_hpas"

	// LLMSMKEDAHPARewiresTotal is a counter of the repair's annotations on an
	// unwired HPA, by outcome: requested (the HPA was annotated, so KEDA
	// derives it again), forbidden (the patch was denied) or failed (any other
	// error).
	// Labels: namespace, outcome (controller_instance when set)
	LLMSMKEDAHPARewiresTotal = "llmsm_keda_hpa_rewires_total"

	// LLMSMKEDAHPAWiringUnread is a gauge: 1 when the last pass could not read
	// an HPA list or a ScaledObject it needed, so some HPAs are neither known
	// to be wired nor counted in llmsm_keda_unwired_hpas; 0 when it read all
	// of them. A denied read is unknown, not absent.
	// Labels: none (controller_instance when set)
	LLMSMKEDAHPAWiringUnread = "llmsm_keda_hpa_wiring_unread"
)

// Values of LabelOutcome on LLMSMKEDAHPARewiresTotal.
const (
	// HPARewireRequested is an annotation the API server accepted.
	HPARewireRequested = "requested"
	// HPARewireForbidden is a patch the controller's role does not allow.
	HPARewireForbidden = "forbidden"
	// HPARewireFailed is a patch that failed for any other reason.
	HPARewireFailed = "failed"
)

package constants

// Decode packing metrics: what a TPOT objective (slo.tpot) made of a decode
// variant's k2, the batch the saturation analyzer sizes one decode replica
// at. Diagnostic only: the bound is already in the per-replica capacity the
// optimizer reads.
const (
	// LLMSMDecodePackingBound is a per-variant gauge, always 1, whose labels
	// say whether the TPOT target bounded how far the decode variant is
	// packed this cycle (bound itl or none) and why. Published only for a
	// decode variant whose policy sets slo.tpot and lists the slo analyzer;
	// one series per variant.
	// Labels: namespace, model_name, variant_name, bound, reason.
	LLMSMDecodePackingBound = "llmsm_decode_packing_bound"
	// LLMSMDecodePackingK2Tokens is a per-variant gauge: the decode variant's
	// k2 in KV tokens before the TPOT bound (k2="unbounded") and after it
	// (k2="bounded"). Same absence as LLMSMDecodePackingBound.
	// Labels: namespace, model_name, variant_name, k2.
	LLMSMDecodePackingK2Tokens = "llmsm_decode_packing_k2_tokens"

	// LabelBound is the bound label of LLMSMDecodePackingBound.
	LabelBound = "bound"
	// LabelK2 is the k2 label of LLMSMDecodePackingK2Tokens.
	LabelK2 = "k2"
	// K2Unbounded and K2Bounded are the values of LabelK2.
	K2Unbounded = "unbounded"
	K2Bounded   = "bounded"
)

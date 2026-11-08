package config

// ExternalAnalyzerBody is one inference engine's query and per-replica target in
// a catalog definition. The field names mirror KEDA's Prometheus scaler
// (threshold is a string, e.g. "0.5").
//
// query must resolve to the model's TOTAL demand (every series it returns is
// summed) and threshold is what ONE scale-target replica — a pod, or an LWS
// group — can serve, not one engine instance. A sum-shaped query is safe at any
// granularity; count() and avg() are not, since they measure in DP ranks. See
// external.Body for the full note.
type ExternalAnalyzerBody struct {
	Query     string `yaml:"query"`
	Threshold string `yaml:"threshold"`
}

// ExternalAnalyzerDef is a catalog entry for one external analyzer. It carries a
// per-engine body keyed by engine ("vllm", "sglang"), or an engine-agnostic body
// given by the top-level Query/Threshold when Engines is empty.
type ExternalAnalyzerDef struct {
	Engines   map[string]ExternalAnalyzerBody `yaml:"engines,omitempty"`
	Query     string                          `yaml:"query,omitempty"`
	Threshold string                          `yaml:"threshold,omitempty"`
}

// ExternalAnalyzerCatalog maps an analyzer label to its definition.
type ExternalAnalyzerCatalog map[string]ExternalAnalyzerDef

// ExternalAnalyzerCatalog returns a copy of the external analyzers declared on the
// cluster "default" scaling entry.
//
// It used to come from a ConfigMap of its own (scaling-manager-analyzers). Folding it onto the
// scaling-policy surface leaves one place to look for "how is scaling configured",
// and puts definitions in the same object as the tiers that select them — related
// settings that were previously two ConfigMaps apart, with nothing tying a
// policy's analyzer name to the definition it resolves against.
//
// Cluster-scoped, like limiters: read from the SYSTEM namespace's ConfigMap only.
// A workload namespace declaring analyzers is reported and ignored — see
// parseScalingPolicyConfig. Note this still lets a per-namespace scaling manager install define
// its own, since that install's own namespace IS its system namespace.
func (c *Config) ExternalAnalyzerCatalog() ExternalAnalyzerCatalog {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return copyAnalyzerCatalog(c.saturation.global[GlobalDefaultsKey].AnalyzerDefinitions)
}

// copyAnalyzerCatalog deep-copies a catalog (including each def's Engines map) so
// stored and returned catalogs cannot be mutated through a shared reference.
func copyAnalyzerCatalog(src ExternalAnalyzerCatalog) ExternalAnalyzerCatalog {
	out := make(ExternalAnalyzerCatalog, len(src))
	for label, def := range src {
		cp := ExternalAnalyzerDef{Query: def.Query, Threshold: def.Threshold}
		if def.Engines != nil {
			cp.Engines = make(map[string]ExternalAnalyzerBody, len(def.Engines))
			for engine, body := range def.Engines {
				cp.Engines[engine] = body
			}
		}
		out[label] = cp
	}
	return out
}

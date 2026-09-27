package memory

// ProvenanceLabel describes formation authority without implying verification or
// presenting a model confidence estimate as a calibrated probability.
func ProvenanceLabel(provenance string) string {
	switch provenance {
	case "user_statement":
		return "stated"
	case "model_inference":
		return "inferred"
	default:
		return "unknown"
	}
}

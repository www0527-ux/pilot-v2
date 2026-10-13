package intent

// Source identifies how a classification was produced.
const (
	SourceLLM      = "llm"
	SourceRule     = "rule"
	SourceFallback = "fallback"
)

// Valid intents mirrored from multiagent RulePlanner.
const (
	IntentGeneralAnalysis    = "general_analysis"
	IntentKnowledgeQuery     = "knowledge_query"
	IntentObservabilityQuery = "observability_query"
	IntentIncidentTriage     = "incident_triage"
)

// Classification is the structured output of an intent classifier.
type Classification struct {
	Intent        string   `json:"intent"`
	Service       string   `json:"service"`
	SelectedRoles []string `json:"selected_roles"`
	Confidence    float64  `json:"confidence"`
	Reason        string   `json:"reason"`
	Source        string   `json:"source"`
}

// ValidIntent reports whether intent is one of the supported labels.
func ValidIntent(intent string) bool {
	switch intent {
	case IntentGeneralAnalysis, IntentKnowledgeQuery, IntentObservabilityQuery, IntentIncidentTriage:
		return true
	default:
		return false
	}
}

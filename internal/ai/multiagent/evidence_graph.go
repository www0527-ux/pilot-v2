package multiagent

import (
	"math"
	"sort"
	"strconv"
	"strings"
)

const (
	// VerificationModeEvidenceGraph enables deterministic epistemic scoring.
	// The score measures evidence coverage and independence; it is deliberately
	// not presented as a learned probability of a root cause being correct.
	VerificationModeEvidenceGraph = "evidence_graph"
	VerificationModeOff           = "off"
)

// VerificationPolicy controls evidence quorum and adaptive re-planning.
type VerificationPolicy struct {
	Mode                   string
	MinConfidence          float64
	MinIndependentSources  int
	ReplanWindowMultiplier int
}

func (p VerificationPolicy) enabled() bool {
	return strings.EqualFold(strings.TrimSpace(p.Mode), VerificationModeEvidenceGraph)
}

func (p VerificationPolicy) normalized() VerificationPolicy {
	p.Mode = strings.ToLower(strings.TrimSpace(p.Mode))
	if p.MinConfidence <= 0 || p.MinConfidence > 1 {
		p.MinConfidence = 0.65
	}
	if p.MinIndependentSources <= 0 {
		p.MinIndependentSources = 2
	}
	if p.ReplanWindowMultiplier <= 0 {
		p.ReplanWindowMultiplier = 2
	}
	if p.ReplanWindowMultiplier > 8 {
		p.ReplanWindowMultiplier = 8
	}
	return p
}

// EvidenceNode is a content-free provenance node. Raw evidence remains in
// Finding; the graph intentionally avoids duplicating potentially sensitive
// log or knowledge text.
type EvidenceNode struct {
	ID          string  `json:"id"`
	Source      string  `json:"source"`
	Kind        string  `json:"kind"` // evidence | hypothesis | context
	Role        Role    `json:"role"`
	Stance      string  `json:"stance"` // support | counter | neutral
	ClaimFamily string  `json:"claim_family,omitempty"`
	Reliability float64 `json:"reliability"`
	Derived     bool    `json:"derived"`
	Runtime     bool    `json:"runtime"`
	Claimable   bool    `json:"claimable"`
}

// EvidenceEdge captures explainable provenance and conflicts.
type EvidenceEdge struct {
	From     string  `json:"from"`
	To       string  `json:"to"`
	Relation string  `json:"relation"` // supported_by | contradicted_by | same_trace | duplicates
	Weight   float64 `json:"weight"`
}

// EvidenceGraph is a deterministic, inspectable evidence ledger.
type EvidenceGraph struct {
	Nodes []EvidenceNode `json:"nodes"`
	Edges []EvidenceEdge `json:"edges,omitempty"`
}

// EvidenceAssessment is a selective-prediction gate over the graph.
// Score is a bounded evidence sufficiency score, not causal probability.
type EvidenceAssessment struct {
	Score                  float64  `json:"score"`
	Band                   string   `json:"band"` // high | medium | low | abstain
	Method                 string   `json:"method"`
	EvidenceCount          int      `json:"evidence_count"`
	RuntimeEvidenceCount   int      `json:"runtime_evidence_count"`
	HypothesisCount        int      `json:"hypothesis_count"`
	IndependentSources     []string `json:"independent_sources"`
	RuntimeSources         []string `json:"runtime_sources"`
	ContradictionCount     int      `json:"contradiction_count"`
	MinIndependentSources  int      `json:"min_independent_sources"`
	NeedsMoreEvidence      bool     `json:"needs_more_evidence"`
	StopReason             string   `json:"stop_reason"`
	ReplanWindowMultiplier int      `json:"replan_window_multiplier"`
}

// CitationAudit verifies that model output cites only IDs present in the
// evidence ledger. It does not attempt to expose or score chain-of-thought.
type CitationAudit struct {
	Passed           bool     `json:"passed"`
	ValidIDs         []string `json:"valid_ids,omitempty"`
	InvalidIDs       []string `json:"invalid_ids,omitempty"`
	ClaimableCount   int      `json:"claimable_count"`
	CitationCount    int      `json:"citation_count"`
	EvidenceCoverage float64  `json:"evidence_coverage"`
}

// VerificationReport combines pre-synthesis evidence sufficiency and
// post-synthesis citation integrity.
type VerificationReport struct {
	Decision   string             `json:"decision"` // accept | degraded | abstain | rejected
	Assessment EvidenceAssessment `json:"assessment"`
	Citations  CitationAudit      `json:"citations"`
}

type graphEvidence struct {
	item        EvidenceItem
	node        EvidenceNode
	traceID     string
	fingerprint string
	supportIDs  []string
	counterIDs  []string
	confidence  float64
}

// buildEvidenceGraph constructs provenance edges and scores whether the
// current evidence is sufficient to stop or should trigger another bounded
// acquisition round.
func buildEvidenceGraph(plan Plan, findings []Finding, policy VerificationPolicy) (*EvidenceGraph, EvidenceAssessment) {
	policy = policy.normalized()
	items := make([]graphEvidence, 0)
	nodeByID := make(map[string]EvidenceNode)
	independent := map[string]bool{}
	runtimeSources := map[string]bool{}
	runtimeCounts := map[string]int{}
	allCounts := map[string]int{}
	countedObservations := map[string]bool{}

	for _, finding := range findings {
		findingMemory := metadataBool(finding.Metadata, "memory") || metadataBool(finding.Metadata, "not_runtime_evidence")
		for _, item := range finding.Evidence {
			id := strings.TrimSpace(item.ID)
			if id == "" {
				continue
			}
			source := canonicalEvidenceSource(item.Source)
			memory := findingMemory || metadataBool(item.Metadata, "memory") || metadataBool(item.Metadata, "not_runtime_evidence")
			kind := strings.ToLower(strings.TrimSpace(metadataString(item.Metadata, "kind")))
			if kind == "" {
				switch {
				case memory:
					kind = "context"
				case source == "correlation" || strings.HasPrefix(strings.ToLower(id), "corr-hyp:"):
					kind = "hypothesis"
				default:
					kind = "evidence"
				}
			}
			stance := strings.ToLower(strings.TrimSpace(metadataString(item.Metadata, "stance")))
			if stance != "counter" && stance != "neutral" {
				stance = "support"
			}
			derived := memory || kind == "hypothesis" || source == "correlation"
			runtime := isRuntimeEvidenceSource(source) && !memory && kind != "hypothesis"
			claimable := strings.TrimSpace(item.Content) != "" && !memory && !metadataStatusError(item.Metadata)
			reliability := sourceReliability(source)
			if override, ok := metadataFloat(item.Metadata, "reliability"); ok && override >= 0 && override <= 1 {
				reliability = override
			}
			confidence := reliability
			if value, ok := metadataFloat(item.Metadata, "confidence"); ok {
				confidence = clamp01(value)
			} else if item.Score != nil {
				confidence = normalizeEvidenceScore(*item.Score, reliability)
			}
			node := EvidenceNode{
				ID:          id,
				Source:      source,
				Kind:        kind,
				Role:        finding.Role,
				Stance:      stance,
				ClaimFamily: strings.TrimSpace(metadataString(item.Metadata, "claim_family")),
				Reliability: round3(reliability),
				Derived:     derived,
				Runtime:     runtime,
				Claimable:   claimable,
			}
			fingerprint := evidenceFingerprint(item.Content)

			// Stable IDs are the de-duplication boundary. A repeated acquisition
			// round cannot inflate confidence by returning the same item again.
			if existing, ok := nodeByID[id]; ok {
				if node.Reliability > existing.Reliability {
					nodeByID[id] = node
				}
				continue
			}
			nodeByID[id] = node
			items = append(items, graphEvidence{
				item:        item,
				node:        node,
				traceID:     strings.TrimSpace(metadataString(item.Metadata, "trace_id")),
				fingerprint: fingerprint,
				supportIDs:  metadataStrings(item.Metadata, "support_ids"),
				counterIDs:  metadataStrings(item.Metadata, "counter_ids"),
				confidence:  confidence,
			})
			if !claimable || derived {
				continue
			}
			independent[source] = true
			observationKey := fingerprint
			if observationKey == "" {
				observationKey = id
			}
			observationKey = source + "\x00" + observationKey
			if countedObservations[observationKey] {
				continue
			}
			countedObservations[observationKey] = true
			allCounts[source]++
			if runtime {
				runtimeSources[source] = true
				runtimeCounts[source]++
			}
		}
	}

	// A derived hypothesis is claimable only when at least one of its declared
	// supports resolves to a claimable, non-hypothesis node. This prevents a
	// model- or adapter-produced hypothesis from becoming self-authenticating.
	for index := range items {
		entry := &items[index]
		if entry.node.Kind != "hypothesis" {
			continue
		}
		validSupport := false
		for _, supportID := range entry.supportIDs {
			support, ok := nodeByID[supportID]
			if ok && support.Claimable && support.Kind != "hypothesis" {
				validSupport = true
				break
			}
		}
		if validSupport {
			continue
		}
		entry.node.Claimable = false
		entry.confidence = 0
		nodeByID[entry.node.ID] = entry.node
	}

	nodes := make([]EvidenceNode, 0, len(nodeByID))
	for _, node := range nodeByID {
		nodes = append(nodes, node)
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })

	edges := make([]EvidenceEdge, 0)
	edgeSeen := map[string]bool{}
	addEdge := func(from, to, relation string, weight float64) {
		if from == "" || to == "" || from == to {
			return
		}
		if _, ok := nodeByID[from]; !ok {
			return
		}
		if _, ok := nodeByID[to]; !ok {
			return
		}
		key := from + "\x00" + to + "\x00" + relation
		if edgeSeen[key] {
			return
		}
		edgeSeen[key] = true
		edges = append(edges, EvidenceEdge{From: from, To: to, Relation: relation, Weight: round3(clamp01(weight))})
	}

	hypothesisCount := 0
	contradictionCount := 0
	hypothesisQualityTotal := 0.0
	for _, entry := range items {
		if entry.node.Kind != "hypothesis" || !entry.node.Claimable {
			continue
		}
		hypothesisCount++
		hypothesisQualityTotal += entry.confidence
		for _, supportID := range entry.supportIDs {
			support, ok := nodeByID[supportID]
			if !ok || !support.Claimable || support.Kind == "hypothesis" {
				continue
			}
			addEdge(entry.node.ID, supportID, "supported_by", entry.confidence)
		}
		for _, counterID := range entry.counterIDs {
			counter, ok := nodeByID[counterID]
			if !ok || !counter.Claimable || counter.Kind == "hypothesis" {
				continue
			}
			contradictionCount++
			addEdge(entry.node.ID, counterID, "contradicted_by", 1-entry.confidence)
		}
	}

	// Same-trace and duplicate edges make source dependence explicit without
	// counting it as additional independent corroboration.
	linkEvidenceGroups(items, func(entry graphEvidence) string { return entry.traceID }, "same_trace", 0.9, addEdge)
	linkEvidenceGroups(items, func(entry graphEvidence) string { return entry.fingerprint }, "duplicates", 1, addEdge)
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].From != edges[j].From {
			return edges[i].From < edges[j].From
		}
		if edges[i].To != edges[j].To {
			return edges[i].To < edges[j].To
		}
		return edges[i].Relation < edges[j].Relation
	})

	requiresRuntime := planSelectsRole(plan, RoleEvidence)
	selectedSources := independent
	selectedCounts := allCounts
	minSources := 1
	if requiresRuntime {
		selectedSources = runtimeSources
		selectedCounts = runtimeCounts
		minSources = policy.MinIndependentSources
	}

	quality := 0.0
	for source := range selectedSources {
		quality += sourceReliability(source)
	}
	if len(selectedSources) > 0 {
		quality /= float64(len(selectedSources))
	}
	itemCount := 0
	for _, count := range selectedCounts {
		itemCount += count
	}
	diversity := math.Min(1, float64(len(selectedSources))/float64(maxInt(minSources, 1)))
	density := 1 - math.Exp(-float64(itemCount)/3)
	hypothesisQuality := 0.0
	if hypothesisCount > 0 {
		hypothesisQuality = hypothesisQualityTotal / float64(hypothesisCount)
	}
	conflictPenalty := math.Min(0.45, float64(contradictionCount)*0.12)
	score := clamp01(0.50*diversity + 0.30*quality + 0.10*density + 0.10*hypothesisQuality - conflictPenalty)

	needsMore := false
	stopReason := "evidence_not_required"
	if requiresRuntime {
		switch {
		case itemCount == 0:
			needsMore = true
			stopReason = "no_runtime_evidence"
		case len(runtimeSources) < minSources:
			needsMore = true
			stopReason = "insufficient_source_diversity"
		case contradictionCount > 0:
			needsMore = true
			stopReason = "contradictory_evidence"
		case score < policy.MinConfidence:
			needsMore = true
			stopReason = "confidence_below_threshold"
		default:
			stopReason = "evidence_quorum_met"
		}
	} else if itemCount > 0 {
		stopReason = "context_evidence_available"
	}

	assessment := EvidenceAssessment{
		Score:                  round3(score),
		Band:                   confidenceBand(score, policy.MinConfidence),
		Method:                 "diversity_weighted_evidence_graph_v1",
		EvidenceCount:          countClaimableNodes(nodes),
		RuntimeEvidenceCount:   countRuntimeNodes(nodes),
		HypothesisCount:        hypothesisCount,
		IndependentSources:     sortedSet(independent),
		RuntimeSources:         sortedSet(runtimeSources),
		ContradictionCount:     contradictionCount,
		MinIndependentSources:  minSources,
		NeedsMoreEvidence:      needsMore,
		StopReason:             stopReason,
		ReplanWindowMultiplier: policy.ReplanWindowMultiplier,
	}
	return &EvidenceGraph{Nodes: nodes, Edges: edges}, assessment
}

func linkEvidenceGroups(items []graphEvidence, keyFn func(graphEvidence) string, relation string, weight float64, add func(string, string, string, float64)) {
	groups := map[string][]string{}
	for _, item := range items {
		key := strings.TrimSpace(keyFn(item))
		if key == "" || !item.node.Claimable {
			continue
		}
		groups[key] = append(groups[key], item.node.ID)
	}
	for _, ids := range groups {
		sort.Strings(ids)
		for index := 1; index < len(ids); index++ {
			add(ids[index-1], ids[index], relation, weight)
		}
	}
}

// appendContextEvidenceNodes keeps recalled episodic context visible in the
// returned provenance ledger without allowing it to raise runtime confidence
// or become a valid citation for a current-incident claim.
func appendContextEvidenceNodes(graph *EvidenceGraph, findings []Finding) {
	if graph == nil {
		return
	}
	seen := make(map[string]bool, len(graph.Nodes))
	for _, node := range graph.Nodes {
		seen[node.ID] = true
	}
	for _, finding := range findings {
		memory := metadataBool(finding.Metadata, "memory") || metadataBool(finding.Metadata, "not_runtime_evidence")
		if !memory {
			continue
		}
		for _, item := range finding.Evidence {
			id := strings.TrimSpace(item.ID)
			if id == "" || seen[id] {
				continue
			}
			source := canonicalEvidenceSource(item.Source)
			graph.Nodes = append(graph.Nodes, EvidenceNode{
				ID:          id,
				Source:      source,
				Kind:        "context",
				Role:        finding.Role,
				Stance:      "neutral",
				ClaimFamily: strings.TrimSpace(metadataString(item.Metadata, "claim_family")),
				Reliability: round3(sourceReliability(source)),
				Derived:     true,
				Runtime:     false,
				Claimable:   false,
			})
			seen[id] = true
		}
	}
	sort.Slice(graph.Nodes, func(i, j int) bool { return graph.Nodes[i].ID < graph.Nodes[j].ID })
}

func auditCitations(answer string, findings []Finding) CitationAudit {
	known := claimableEvidenceIDs(findings)

	validSet := map[string]bool{}
	invalidSet := map[string]bool{}
	for _, candidate := range bracketedCitations(answer) {
		if known[candidate] {
			validSet[candidate] = true
			continue
		}
		if looksLikeEvidenceID(candidate) {
			invalidSet[candidate] = true
		}
	}
	valid := sortedSet(validSet)
	invalid := sortedSet(invalidSet)
	coverage := 1.0
	if len(known) > 0 {
		coverage = float64(len(valid)) / float64(len(known))
	}
	return CitationAudit{
		Passed:           (len(known) == 0 || len(valid) > 0) && len(invalid) == 0,
		ValidIDs:         valid,
		InvalidIDs:       invalid,
		ClaimableCount:   len(known),
		CitationCount:    len(valid),
		EvidenceCoverage: round3(coverage),
	}
}

func claimableEvidenceIDs(findings []Finding) map[string]bool {
	known := map[string]bool{}
	type pendingHypothesis struct {
		id       string
		supports []string
	}
	pending := make([]pendingHypothesis, 0)
	for _, finding := range findings {
		memory := metadataBool(finding.Metadata, "memory") || metadataBool(finding.Metadata, "not_runtime_evidence")
		for _, item := range finding.Evidence {
			if memory || !claimableEvidenceItem(item) {
				continue
			}
			id := strings.TrimSpace(item.ID)
			if isHypothesisEvidence(item) {
				pending = append(pending, pendingHypothesis{id: id, supports: metadataStrings(item.Metadata, "support_ids")})
				continue
			}
			known[id] = true
		}
	}
	for _, hypothesis := range pending {
		for _, supportID := range hypothesis.supports {
			if known[supportID] {
				known[hypothesis.id] = true
				break
			}
		}
	}
	return known
}

func isHypothesisEvidence(item EvidenceItem) bool {
	kind := strings.ToLower(strings.TrimSpace(metadataString(item.Metadata, "kind")))
	return kind == "hypothesis" || canonicalEvidenceSource(item.Source) == "correlation" || strings.HasPrefix(strings.ToLower(strings.TrimSpace(item.ID)), "corr-hyp:")
}

func planWithAssessment(plan Plan, assessment EvidenceAssessment) Plan {
	plan.Metadata = cloneMetadata(plan.Metadata)
	plan.Metadata["evidence_confidence"] = assessment.Score
	plan.Metadata["evidence_confidence_band"] = assessment.Band
	plan.Metadata["evidence_stop_reason"] = assessment.StopReason
	plan.Metadata["evidence_needs_more"] = assessment.NeedsMoreEvidence
	plan.Metadata["evidence_runtime_sources"] = assessment.RuntimeSources
	plan.Metadata["evidence_contradictions"] = assessment.ContradictionCount
	return plan
}

func bracketedCitations(answer string) []string {
	out := make([]string, 0)
	for start := 0; start < len(answer); {
		open := strings.IndexByte(answer[start:], '[')
		if open < 0 {
			break
		}
		open += start
		closeOffset := strings.IndexByte(answer[open+1:], ']')
		if closeOffset < 0 {
			break
		}
		closeIndex := open + 1 + closeOffset
		candidate := strings.TrimSpace(answer[open+1 : closeIndex])
		if candidate != "" && len(candidate) <= 240 && !strings.ContainsAny(candidate, "\r\n[]") {
			out = append(out, candidate)
		}
		start = closeIndex + 1
	}
	return out
}

func looksLikeEvidenceID(value string) bool {
	value = strings.TrimSpace(strings.ToLower(value))
	if strings.ContainsAny(value, " \t") {
		return false
	}
	prefixes := []string{"alert:", "log:", "metric:", "trace:", "knowledge:", "tool:", "corr-", "episodic:"}
	for _, prefix := range prefixes {
		if strings.HasPrefix(value, prefix) {
			return true
		}
	}
	return strings.Contains(value, ":")
}

func claimableEvidenceItem(item EvidenceItem) bool {
	return strings.TrimSpace(item.ID) != "" && strings.TrimSpace(item.Content) != "" &&
		!metadataBool(item.Metadata, "memory") && !metadataBool(item.Metadata, "not_runtime_evidence") &&
		!metadataStatusError(item.Metadata)
}

func metadataStatusError(metadata map[string]any) bool {
	return strings.EqualFold(strings.TrimSpace(metadataString(metadata, "status")), "error")
}

func canonicalEvidenceSource(source string) string {
	normalized := strings.ToLower(strings.TrimSpace(source))
	switch {
	case normalized == "alert", normalized == "alerts", strings.Contains(normalized, "query_alerts"):
		return "alerts"
	case normalized == "log", normalized == "logs", strings.Contains(normalized, "query_logs"):
		return "logs"
	case normalized == "metric", normalized == "metrics", strings.Contains(normalized, "query_metrics"):
		return "metrics"
	case normalized == "trace", normalized == "traces", strings.Contains(normalized, "query_traces"):
		return "traces"
	case normalized == "correlation":
		return "correlation"
	case normalized == "knowledge", strings.Contains(normalized, "search_knowledge"):
		return "knowledge"
	case normalized == "episodic_memory", normalized == "memory":
		return "memory"
	case normalized == "topology":
		return "topology"
	case normalized == "":
		return "unknown"
	default:
		return normalized
	}
}

func isRuntimeEvidenceSource(source string) bool {
	switch canonicalEvidenceSource(source) {
	case "alerts", "logs", "metrics", "traces", "topology":
		return true
	default:
		return false
	}
}

func sourceReliability(source string) float64 {
	switch canonicalEvidenceSource(source) {
	case "traces":
		return 0.92
	case "alerts":
		return 0.85
	case "metrics":
		return 0.80
	case "logs":
		return 0.70
	case "topology":
		return 0.55
	case "knowledge":
		return 0.50
	case "correlation":
		return 0.45
	case "memory":
		return 0.20
	default:
		return 0.35
	}
}

func normalizeEvidenceScore(score, fallback float64) float64 {
	if math.IsNaN(score) || math.IsInf(score, 0) || score < 0 {
		return fallback
	}
	if score <= 1 {
		return clamp01(0.5*fallback + 0.5*score)
	}
	// Scores from BM25/correlation are unbounded; saturate without pretending
	// they are probabilities.
	return clamp01(1 - math.Exp(-score/8))
}

func planSelectsRole(plan Plan, role Role) bool {
	for _, selected := range plan.SelectedRoles {
		if selected == role {
			return true
		}
	}
	return false
}

func confidenceBand(score, threshold float64) string {
	switch {
	case score >= 0.80:
		return "high"
	case score >= threshold:
		return "medium"
	case score > 0:
		return "low"
	default:
		return "abstain"
	}
}

func metadataString(metadata map[string]any, key string) string {
	if metadata == nil {
		return ""
	}
	switch value := metadata[key].(type) {
	case string:
		return value
	case []byte:
		return string(value)
	default:
		return ""
	}
}

func metadataBool(metadata map[string]any, key string) bool {
	if metadata == nil {
		return false
	}
	switch value := metadata[key].(type) {
	case bool:
		return value
	case string:
		parsed, _ := strconv.ParseBool(value)
		return parsed
	default:
		return false
	}
}

func metadataFloat(metadata map[string]any, key string) (float64, bool) {
	if metadata == nil {
		return 0, false
	}
	switch value := metadata[key].(type) {
	case float64:
		return value, true
	case float32:
		return float64(value), true
	case int:
		return float64(value), true
	case int64:
		return float64(value), true
	case string:
		parsed, err := strconv.ParseFloat(value, 64)
		return parsed, err == nil
	default:
		return 0, false
	}
}

func metadataStrings(metadata map[string]any, key string) []string {
	if metadata == nil {
		return nil
	}
	var out []string
	switch value := metadata[key].(type) {
	case []string:
		out = append(out, value...)
	case []any:
		for _, item := range value {
			if text, ok := item.(string); ok {
				out = append(out, text)
			}
		}
	case string:
		if strings.TrimSpace(value) != "" {
			out = strings.Split(value, ",")
		}
	}
	seen := map[string]bool{}
	clean := make([]string, 0, len(out))
	for _, value := range out {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		clean = append(clean, value)
	}
	sort.Strings(clean)
	return clean
}

func evidenceFingerprint(content string) string {
	content = strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(content)), " "))
	if len(content) < 16 {
		return ""
	}
	if len(content) > 256 {
		content = content[:256]
	}
	return content
}

func sortedSet(values map[string]bool) []string {
	out := make([]string, 0, len(values))
	for value, include := range values {
		if include {
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out
}

func countClaimableNodes(nodes []EvidenceNode) int {
	count := 0
	for _, node := range nodes {
		if node.Claimable {
			count++
		}
	}
	return count
}

func countRuntimeNodes(nodes []EvidenceNode) int {
	count := 0
	for _, node := range nodes {
		if node.Claimable && node.Runtime {
			count++
		}
	}
	return count
}

func clamp01(value float64) float64 {
	if value < 0 {
		return 0
	}
	if value > 1 {
		return 1
	}
	return value
}

func round3(value float64) float64 { return math.Round(value*1000) / 1000 }

func maxInt(left, right int) int {
	if left > right {
		return left
	}
	return right
}

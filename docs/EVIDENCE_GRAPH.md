# OnCall Epistemic Evidence Graph

## Why this exists

An OnCall Agent fails dangerously when it converts “one plausible log line” into a definitive root cause. Pilot therefore separates four concepts that ordinary RAG/Agent pipelines often collapse:

1. raw runtime observation;
2. retrieved operational context;
3. derived hypothesis;
4. counter-evidence.

The graph and score are deterministic. The LLM writes the final explanation but does not decide whether its own evidence is sufficient, and it cannot create a valid citation by inventing an ID.

## Runtime contract

`POST /api/v1/chat/multi-agent` adds two backward-compatible fields:

~~~json
{
  "evidence_graph": {
    "nodes": [
      {
        "id": "corr-hyp:0:checkout",
        "source": "correlation",
        "kind": "hypothesis",
        "stance": "support",
        "reliability": 0.45,
        "derived": true,
        "runtime": false,
        "claimable": true
      }
    ],
    "edges": [
      {
        "from": "corr-hyp:0:checkout",
        "to": "corr-log:0:...",
        "relation": "supported_by",
        "weight": 0.82
      }
    ]
  },
  "verification": {
    "decision": "accept",
    "assessment": {
      "score": 0.841,
      "band": "high",
      "method": "diversity_weighted_evidence_graph_v1",
      "runtime_sources": ["alerts", "logs", "metrics"],
      "contradiction_count": 0,
      "needs_more_evidence": false,
      "stop_reason": "evidence_quorum_met"
    },
    "citations": {
      "passed": true,
      "valid_ids": ["corr-hyp:0:checkout"],
      "invalid_ids": []
    }
  }
}
~~~

The graph does not duplicate `EvidenceItem.Content`. This keeps log lines and knowledge text out of the provenance topology and out of high-cardinality telemetry attributes. Recalled episodic incidents remain visible as non-claimable `context` nodes, but never raise the current incident's runtime score or satisfy its citation gate.

## Acquisition and temporal correlation

Alerts, error logs, and metric ranges are read concurrently. A source failure is retained in `data_gaps`; the remaining sources may still produce a partial result.

Metric handling is deliberately different from “value > 0 means anomaly”:

- `up` has known semantics: values below `0.5` support an availability incident; healthy `up=1` is counter-evidence only for an availability claim, not for unrelated latency/error/saturation hypotheses.
- Other series require at least four samples or an explicit `anomaly=true` label.
- The detector uses median and Median Absolute Deviation (MAD), with a relative-scale fallback when MAD is zero.
- At most the three strongest anomalous points per series enter the graph, preventing sampling frequency from inflating confidence.

Signals are aligned into time buckets and classified into narrow claim families (`availability`, `latency`, `errors`, `saturation`, and generic `incident`/`performance`). Each service hypothesis records source diversity, claim families, first/lead signal, lead-to-last lag, support IDs, matching counter IDs, and whether a static topology edge contributed a weak prior. This prevents a healthy availability probe from incorrectly “disproving” a latency incident. A topology prior is applied only when the dependency itself has an observed signal. Pilot never creates a dependency hypothesis by copying the parent service score onto an unobserved node.

This is a robust temporal evidence heuristic, not causal discovery. The API and summaries use the word “hypothesis” and never label the score as causal probability.

## Evidence sufficiency score

For an incident plan, only independent runtime sources count toward the quorum. Correlation hypotheses are derived evidence; knowledge and episodic memory are context; same-trace and duplicate observations are explicit dependency edges.

The bounded score is:

~~~text
score = clamp01(0.50*D + 0.30*Q + 0.10*N + 0.10*H - P)
~~~

- `D`: independent-source diversity divided by the configured quorum.
- `Q`: mean source reliability for the independent sources.
- `N`: saturating unique-observation density, `1-exp(-n/3)`.
- `H`: mean confidence of derived temporal hypotheses.
- `P`: counter-evidence penalty, capped at `0.45`.

Default source reliability priors are traces `0.92`, alerts `0.85`, metrics `0.80`, logs `0.70`, topology `0.55`, knowledge `0.50`, correlation `0.45`, and episodic memory `0.20`. They are engineering priors for deterministic ranking, not empirically calibrated likelihoods.

Default policy:

~~~yaml
agent:
  multi_agent:
    max_replan_rounds: 1
    verification:
      mode: evidence_graph
      min_confidence: 0.65
      min_independent_sources: 2
      replan_window_multiplier: 2
~~~

## Bounded acquire-assess-replan

The first acquisition stops only when the runtime-source quorum and score threshold are met and no counter-evidence relation remains unresolved. Otherwise the single configured replan round receives:

~~~text
replan_round=1
evidence_window_multiplier=2
~~~

The deterministic collector expands both its time range and bounded log limit. The correlation branch expands the same window. Stable evidence IDs are de-duplicated when rounds merge, so fetching an identical observation twice cannot raise the score.

After the budget is exhausted, `stop_reason` is prefixed with `replan_budget_exhausted:` and limitations contain `EVIDENCE_QUORUM_NOT_MET` / `EVIDENCE_CONFIDENCE_LOW`. The synthesis prompt is told to abstain from a definitive root-cause claim.

## Citation integrity

The model must cite exact IDs in square brackets. The audit:

- excludes failed tool outputs and episodic memory from claimable runtime evidence;
- skips model synthesis entirely and returns an abstention when no claimable evidence exists;
- requires at least one valid citation when claimable evidence exists;
- rejects evidence-looking IDs that are absent from the graph;
- returns a deterministic evidence summary on missing or fabricated citations.

This validates provenance integrity, not semantic entailment of every sentence. Claim-level natural-language-inference verification remains future work and must be evaluated on labelled incidents before it can become a production gate.

## Runtime parity and observability

Both legacy goroutine orchestration and Eino ADK `ParallelAgent` use the same graph builder, score, replan policy, and citation audit. Session summaries are passed through request context rather than a mutable shared field, preventing concurrent user sessions from overwriting each other.

OpenTelemetry spans follow the current GenAI vocabulary:

- `invoke_agent pilot.oncall`;
- `plan pilot.oncall`;
- `invoke_agent pilot.evidence|pilot.knowledge`;
- `chat <model>`;
- `execute_tool <tool>`;
- `embeddings <model>`.

Prompts, user input, tool arguments/results, log bodies, and embedding input remain excluded from span attributes.

## Verification

Local deterministic checks:

~~~bash
go test ./internal/ai/multiagent ./internal/observability/correlate ./internal/ai/observe ./internal/config
go run ./cmd/agenteval -mode offline
make ci
~~~

Live canaries can additionally require `verification`, set `min_confidence`, and cap `max_contradictions` in `testdata/eval/canaries.json`. The report records confidence band, decision, stop reason, contradiction count, citation status, and graph node count.

## Standards and framework references

- [CloudWeGo Eino ADK cookbook](https://github.com/cloudwego/eino-examples/blob/main/COOKBOOK.md) — supervisor, plan-execute-replan, deep-agent, and HITL patterns considered for this design.
- [OpenTelemetry GenAI agent spans](https://github.com/open-telemetry/semantic-conventions-genai/blob/main/docs/gen-ai/gen-ai-agent-spans.md) — `invoke_agent`, `plan`, and `execute_tool` span conventions.
- [OpenTelemetry GenAI attribute registry](https://opentelemetry.io/docs/specs/semconv/registry/attributes/gen-ai/) — operation, model, tool, token, and content-sensitivity semantics.

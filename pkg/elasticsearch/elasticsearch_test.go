package elasticsearch

import "testing"

func TestExtractSearchHitsPreservesIDAndScore(t *testing.T) {
	result := map[string]interface{}{
		"hits": map[string]interface{}{
			"hits": []interface{}{
				map[string]interface{}{
					"_id":    "knowledge-1",
					"_score": 4.2,
					"_source": map[string]interface{}{
						"title": "runbook",
					},
				},
			},
		},
	}

	hits, err := extractSearchHits(result)
	if err != nil {
		t.Fatalf("extractSearchHits() error = %v", err)
	}
	if len(hits) != 1 || hits[0].ID != "knowledge-1" || hits[0].Score != 4.2 {
		t.Fatalf("unexpected hits: %#v", hits)
	}
}

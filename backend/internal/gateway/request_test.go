package gateway

import "testing"

func TestApplyContinuationStateDoesNotBackfillPreviousResponseID(t *testing.T) {
	reqBody := map[string]any{
		"input": []any{
			map[string]any{
				"type":    "function_call_output",
				"call_id": "call_1",
				"output":  "ok",
			},
		},
	}

	session := openAISessionResolution{PreviousRespID: "resp_prev"}
	reqBody = applyContinuationState(reqBody, session)
	if got, _ := reqBody["previous_response_id"].(string); got != "" {
		t.Fatalf("expected previous_response_id to NOT be backfilled, got %q", got)
	}
}

func TestDropPreviousResponseIDFromJSON(t *testing.T) {
	next, changed := dropPreviousResponseIDFromJSON([]byte(`{"model":"gpt-5.4","previous_response_id":"resp_old","input":[]}`))
	if !changed {
		t.Fatalf("expected previous_response_id to be removed")
	}
	if string(next) == `{"model":"gpt-5.4","previous_response_id":"resp_old","input":[]}` {
		t.Fatalf("expected updated payload")
	}
}

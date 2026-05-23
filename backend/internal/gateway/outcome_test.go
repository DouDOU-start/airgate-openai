package gateway

import (
	"errors"
	"testing"

	sdk "github.com/DouDOU-start/airgate-sdk/sdkgo"
)

func TestForwardErrForOutcomeSuppressesClientError(t *testing.T) {
	err := errors.New("boom")
	got := forwardErrForOutcome(sdk.ForwardOutcome{Kind: sdk.OutcomeClientError}, err)
	if got != nil {
		t.Fatalf("expected nil err for client outcome, got %v", got)
	}
}

func TestForwardErrForOutcomeKeepsAccountError(t *testing.T) {
	err := errors.New("boom")
	got := forwardErrForOutcome(sdk.ForwardOutcome{Kind: sdk.OutcomeAccountDead}, err)
	if got != err {
		t.Fatalf("expected original err for account outcome, got %v", got)
	}
}

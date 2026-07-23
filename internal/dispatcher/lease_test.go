package dispatcher

import (
	"context"
	"errors"
	"testing"
)

func TestSupervisorLeaseRenewalIgnoresShutdownCancellation(t *testing.T) {
	cause := errors.New("database unavailable")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := supervisorLeaseRenewalError(ctx, cause); err != nil {
		t.Fatalf("shutdown cancellation became a lease failure: %v", err)
	}
	if err := supervisorLeaseRenewalError(context.Background(), cause); !errors.Is(err, cause) {
		t.Fatalf("live renewal failure was lost: %v", err)
	}
}

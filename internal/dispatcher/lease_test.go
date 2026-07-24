package dispatcher

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nn1a/autogora/internal/store"
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

func TestServiceLeaseDeadlineUsesReturnedExpiryAndLocalMonotonicStart(t *testing.T) {
	started := time.Now()
	expires := started.Add(17 * time.Second)
	deadline, err := serviceLeaseMonotonicDeadline(started, store.ServiceLease{
		ExpiresAt: expires.UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		t.Fatal(err)
	}
	if delta := deadline.Sub(started); delta != 17*time.Second {
		t.Fatalf("deadline delta = %s, want 17s", delta)
	}
	if _, err := serviceLeaseMonotonicDeadline(started, store.ServiceLease{
		ExpiresAt: started.UTC().Format(time.RFC3339Nano),
	}); err == nil {
		t.Fatal("non-advancing service lease expiry was accepted")
	}
}

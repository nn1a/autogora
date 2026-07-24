package processguard

import (
	"errors"
	"fmt"
	"strings"
)

// ErrAutomaticMutationContainmentUnavailable identifies an automatic host or
// worker mutation that this platform cannot safely contain. Callers should
// persist a capability diagnostic instead of starting the mutation.
var ErrAutomaticMutationContainmentUnavailable = errors.New(
	"automatic mutation containment is unavailable",
)

const automaticMutationContainmentUnsupportedReason = "this platform cannot attest that every descendant process stopped; automatic host and durable workspace mutations require the Linux process guard"

// AutomaticMutationContainmentAvailable reports whether automatic writable
// work can use the same descendant teardown proof exposed by
// TeardownProofAvailable. Keeping the two capabilities identical prevents a
// caller from treating an unprovable direct fallback as a safe mutation host.
func AutomaticMutationContainmentAvailable() bool {
	return TeardownProofAvailable()
}

// AutomaticMutationContainmentUnsupportedReason returns a stable,
// human-readable explanation on unsupported platforms and an empty string
// when automatic mutations are available.
func AutomaticMutationContainmentUnsupportedReason() string {
	if AutomaticMutationContainmentAvailable() {
		return ""
	}
	return automaticMutationContainmentUnsupportedReason
}

// AutomaticMutationContainmentError retains a typed capability boundary while
// adding the specific operation to logs and durable diagnostics.
type AutomaticMutationContainmentError struct {
	Operation string
}

func (e *AutomaticMutationContainmentError) Error() string {
	operation := strings.TrimSpace(e.Operation)
	if operation == "" {
		return ErrAutomaticMutationContainmentUnavailable.Error() + ": " +
			AutomaticMutationContainmentUnsupportedReason()
	}
	return fmt.Sprintf(
		"%s for %s: %s",
		ErrAutomaticMutationContainmentUnavailable,
		operation,
		AutomaticMutationContainmentUnsupportedReason(),
	)
}

func (e *AutomaticMutationContainmentError) Unwrap() error {
	return ErrAutomaticMutationContainmentUnavailable
}

// RequireAutomaticMutationContainment fails closed before an automatic
// mutation is started on a platform without verifiable descendant teardown.
func RequireAutomaticMutationContainment(operation string) error {
	if AutomaticMutationContainmentAvailable() {
		return nil
	}
	return &AutomaticMutationContainmentError{Operation: operation}
}

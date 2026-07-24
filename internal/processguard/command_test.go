package processguard

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestNestedTeardownFailureReportersPreserveOuterFirstOrder(t *testing.T) {
	var calls []string
	ctx := WithTeardownFailureReporter(
		context.Background(),
		func(err error) {
			if !errors.Is(err, ErrTeardownUnconfirmed) {
				t.Fatalf("outer reporter error = %v", err)
			}
			calls = append(calls, "outer")
		},
	)
	ctx = WithTeardownFailureReporter(ctx, func(err error) {
		if !errors.Is(err, ErrTeardownUnconfirmed) {
			t.Fatalf("inner reporter error = %v", err)
		}
		calls = append(calls, "inner")
	})

	ReportTeardownFailure(ctx, errors.Join(
		errors.New("fixture cleanup failed"),
		ErrTeardownUnconfirmed,
	))

	if want := []string{"outer", "inner"}; !reflect.DeepEqual(calls, want) {
		t.Fatalf("reporter order = %v, want %v", calls, want)
	}
}

func TestNilNestedTeardownFailureReporterKeepsParent(t *testing.T) {
	called := false
	ctx := WithTeardownFailureReporter(
		context.Background(),
		func(error) { called = true },
	)
	ctx = WithTeardownFailureReporter(ctx, nil)

	ReportTeardownFailure(ctx, ErrTeardownUnconfirmed)
	if !called {
		t.Fatal("nil nested reporter removed the parent reporter")
	}
}

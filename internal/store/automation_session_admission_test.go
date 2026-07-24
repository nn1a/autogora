package store

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"
)

func mustRegisterAdmissionSession(
	t *testing.T,
	opened *Store,
	board string,
	sessionID string,
) AutomationDispatcherSessionLease {
	t.Helper()
	lease, acquired, err := opened.RegisterAutomationDispatcherSession(
		context.Background(),
		board,
		sessionID,
		time.Minute,
	)
	if err != nil || !acquired || lease.leaseToken == "" {
		t.Fatalf(
			"register %q on %q = %s, acquired=%v, err=%v",
			sessionID,
			board,
			lease,
			acquired,
			err,
		)
	}
	return lease
}

func TestAutomationDispatcherAdmissionRejectsOverlappingLiveSessions(t *testing.T) {
	ctx := context.Background()

	t.Run("wildcard blocks every scope", func(t *testing.T) {
		opened := openAutomationTestStore(t)
		owner := mustRegisterAdmissionSession(t, opened, "*", "wildcard-owner")
		duplicate, acquired, err := opened.RegisterAutomationDispatcherSession(
			ctx,
			"*",
			owner.SessionID,
			time.Minute,
		)
		if err != nil || acquired || duplicate.SessionID != owner.SessionID ||
			duplicate.leaseToken != "" {
			t.Fatalf(
				"duplicate registration = %s, acquired=%v, err=%v",
				duplicate,
				acquired,
				err,
			)
		}

		for _, board := range []string{"*", "default", "other"} {
			lease, acquired, err := opened.RegisterAutomationDispatcherSession(
				ctx,
				board,
				"blocked-"+board,
				time.Minute,
			)
			if !errors.Is(err, ErrAutomationHostNotIdle) || acquired ||
				lease.leaseToken != "" {
				t.Fatalf(
					"overlapping registration on %q = %s, acquired=%v, err=%v",
					board,
					lease,
					acquired,
					err,
				)
			}
		}
	})

	t.Run("board scope blocks itself and wildcard only", func(t *testing.T) {
		opened := openAutomationTestStore(t)
		mustRegisterAdmissionSession(t, opened, "alpha", "alpha-owner")

		for _, candidate := range []struct {
			board string
			id    string
		}{
			{board: "alpha", id: "second-alpha"},
			{board: "*", id: "global-owner"},
		} {
			lease, acquired, err := opened.RegisterAutomationDispatcherSession(
				ctx,
				candidate.board,
				candidate.id,
				time.Minute,
			)
			if !errors.Is(err, ErrAutomationHostNotIdle) || acquired ||
				lease.leaseToken != "" {
				t.Fatalf(
					"overlapping registration on %q = %s, acquired=%v, err=%v",
					candidate.board,
					lease,
					acquired,
					err,
				)
			}
		}

		beta := mustRegisterAdmissionSession(t, opened, "beta", "beta-owner")
		if beta.Board != "beta" {
			t.Fatalf("distinct board lease = %s", beta)
		}
	})
}

func TestAutomationDispatcherAdmissionQuarantinesExpiredOwnerAtomically(t *testing.T) {
	ctx := context.Background()
	opened := openAutomationTestStore(t)
	expired := mustRegisterAdmissionSession(
		t,
		opened,
		"default",
		"expired-owner",
	)
	expiredToken := expired.leaseToken
	expiredAt := time.Now().Add(-time.Second).UTC().Format(
		automationTimestampLayout,
	)
	if _, err := opened.db.ExecContext(ctx, `UPDATE automation_dispatcher_sessions
		SET expires_at = ? WHERE session_id = ?`,
		expiredAt,
		expired.SessionID,
	); err != nil {
		t.Fatal(err)
	}

	replacement, acquired, err := opened.RegisterAutomationDispatcherSession(
		ctx,
		"default",
		"replacement-before-confirmation",
		time.Minute,
	)
	if !errors.Is(err, ErrAutomationQuarantined) || acquired ||
		replacement.leaseToken != "" {
		t.Fatalf(
			"replacement = %s, acquired=%v, err=%v",
			replacement,
			acquired,
			err,
		)
	}
	gate, err := opened.GetAutomationQuarantine(ctx)
	if err != nil || !gate.Active || gate.Generation != 1 ||
		gate.ActiveSourceCount != 1 {
		t.Fatalf("expired owner gate = %+v, err=%v", gate, err)
	}
	sources, err := opened.ListAutomationQuarantineSources(
		ctx,
		AutomationQuarantineSourceFilter{
			Board:    "default",
			Kind:     automationExpiredSessionSourceKind,
			SourceID: expired.SessionID,
		},
	)
	if err != nil || len(sources) != 1 {
		t.Fatalf("expired owner sources = %+v, err=%v", sources, err)
	}
	source := sources[0]
	if source.Generation != gate.Generation ||
		source.ObservedUpdatedAt != expiredAt ||
		source.DiagnosticCode != automationExpiredSessionDiagnostic ||
		source.Disposition != "active" {
		t.Fatalf("expired owner source = %+v", source)
	}

	var releasedAt *string
	if err := opened.db.QueryRowContext(ctx, `SELECT released_at
		FROM automation_dispatcher_sessions WHERE session_id = ?`,
		expired.SessionID,
	).Scan(&releasedAt); err != nil || releasedAt == nil {
		t.Fatalf("expired owner release = %v, err=%v", releasedAt, err)
	}
	var replacementRows int
	if err := opened.db.QueryRowContext(ctx, `SELECT COUNT(*)
		FROM automation_dispatcher_sessions WHERE session_id = ?`,
		"replacement-before-confirmation",
	).Scan(&replacementRows); err != nil || replacementRows != 0 {
		t.Fatalf("premature replacement rows = %d, err=%v", replacementRows, err)
	}

	confirmation := AutomationQuarantineConfirmation{
		Generation:            gate.Generation,
		Actor:                 "operator",
		Reason:                "verified expired dispatcher process is stopped",
		HelpersStopped:        true,
		ExternalWritesStopped: true,
		Sources: []AutomationQuarantineSourceResolution{{
			SourceKey:         source.SourceKey,
			ObservedUpdatedAt: source.ObservedUpdatedAt,
			Disposition:       AutomationSourceAbandoned,
		}},
	}
	cleared, changed, err := opened.ConfirmAutomationQuarantine(ctx, confirmation)
	if err != nil || !changed || cleared.Active {
		t.Fatalf("confirm expired owner = %+v, changed=%v, err=%v", cleared, changed, err)
	}

	reused, acquired, err := opened.RegisterAutomationDispatcherSession(
		ctx,
		"default",
		expired.SessionID,
		time.Minute,
	)
	if err != nil || acquired || reused.SessionID != expired.SessionID ||
		reused.leaseToken != "" {
		t.Fatalf("session ID reuse = %s, acquired=%v, err=%v", reused, acquired, err)
	}
	if rendered := fmt.Sprintf("%+v %#v", reused, reused); rendered == "" ||
		strings.Contains(rendered, expiredToken) {
		t.Fatalf("redacted reused lease = %q", rendered)
	}

	next := mustRegisterAdmissionSession(
		t,
		opened,
		"default",
		"replacement-after-confirmation",
	)
	if next.SessionID == expired.SessionID {
		t.Fatalf("new session reused old identity: %s", next)
	}
}

func TestAutomationDispatcherAdmissionSerializesConcurrentRegistration(t *testing.T) {
	ctx := context.Background()
	opened := openAutomationTestStore(t)

	const attempts = 12
	type result struct {
		lease    AutomationDispatcherSessionLease
		acquired bool
		err      error
	}
	start := make(chan struct{})
	results := make(chan result, attempts)
	var workers sync.WaitGroup
	workers.Add(attempts)
	for index := 0; index < attempts; index++ {
		index := index
		go func() {
			defer workers.Done()
			<-start
			lease, acquired, err := opened.RegisterAutomationDispatcherSession(
				ctx,
				"shared-board",
				fmt.Sprintf("concurrent-owner-%02d", index),
				time.Minute,
			)
			results <- result{lease: lease, acquired: acquired, err: err}
		}()
	}
	close(start)
	workers.Wait()
	close(results)

	acquiredCount := 0
	conflictCount := 0
	for result := range results {
		switch {
		case result.err == nil && result.acquired &&
			result.lease.leaseToken != "":
			acquiredCount++
		case errors.Is(result.err, ErrAutomationHostNotIdle) &&
			!result.acquired && result.lease.leaseToken == "":
			conflictCount++
		default:
			t.Fatalf(
				"unexpected concurrent result = %s, acquired=%v, err=%v",
				result.lease,
				result.acquired,
				result.err,
			)
		}
	}
	if acquiredCount != 1 || conflictCount != attempts-1 {
		t.Fatalf(
			"concurrent registration acquired=%d, conflicts=%d",
			acquiredCount,
			conflictCount,
		)
	}
	var rows int
	if err := opened.db.QueryRowContext(ctx, `SELECT COUNT(*)
		FROM automation_dispatcher_sessions
		WHERE board = ? AND released_at IS NULL`,
		"shared-board",
	).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("live serialized rows = %d, err=%v", rows, err)
	}
}

func TestAutomationDispatcherAdmissionRollsBackAtGenerationExhaustion(t *testing.T) {
	ctx := context.Background()
	opened := openAutomationTestStore(t)
	expired := mustRegisterAdmissionSession(
		t,
		opened,
		"default",
		"expired-at-generation-limit",
	)
	expiredAt := time.Now().Add(-time.Second).UTC().Format(
		automationTimestampLayout,
	)
	if _, err := opened.db.ExecContext(ctx, `UPDATE automation_dispatcher_sessions
		SET expires_at = ? WHERE session_id = ?`,
		expiredAt,
		expired.SessionID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := opened.db.ExecContext(ctx, `UPDATE automation_quarantine_gate
		SET generation = ? WHERE singleton = 1`,
		int64(math.MaxInt64),
	); err != nil {
		t.Fatal(err)
	}

	lease, acquired, err := opened.RegisterAutomationDispatcherSession(
		ctx,
		"default",
		"must-not-register-at-generation-limit",
		time.Minute,
	)
	if err == nil || !strings.Contains(err.Error(), "generation is exhausted") ||
		acquired || lease.leaseToken != "" {
		t.Fatalf(
			"generation exhaustion registration = %s, acquired=%v, err=%v",
			lease,
			acquired,
			err,
		)
	}
	var released, sourceCount, replacementCount int
	if err := opened.db.QueryRowContext(ctx, `SELECT released_at IS NOT NULL
		FROM automation_dispatcher_sessions WHERE session_id = ?`,
		expired.SessionID,
	).Scan(&released); err != nil {
		t.Fatal(err)
	}
	if err := opened.db.QueryRowContext(ctx, `SELECT COUNT(*)
		FROM automation_quarantine_sources WHERE source_id = ?`,
		expired.SessionID,
	).Scan(&sourceCount); err != nil {
		t.Fatal(err)
	}
	if err := opened.db.QueryRowContext(ctx, `SELECT COUNT(*)
		FROM automation_dispatcher_sessions WHERE session_id = ?`,
		"must-not-register-at-generation-limit",
	).Scan(&replacementCount); err != nil {
		t.Fatal(err)
	}
	if released != 0 || sourceCount != 0 || replacementCount != 0 {
		t.Fatalf(
			"generation exhaustion partially committed: released=%d, sources=%d, replacement=%d",
			released,
			sourceCount,
			replacementCount,
		)
	}
}

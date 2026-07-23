package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestServiceLeaseLifecycle(t *testing.T) {
	ctx := context.Background()
	store, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	zone := time.FixedZone("KST", 9*60*60)
	current := time.Date(2030, 1, 2, 12, 4, 5, 123456789, zone)
	initial, acquired, err := store.AcquireServiceLease(ctx, " dispatcher ", " node-a ", 30*time.Second, current)
	if err != nil {
		t.Fatal(err)
	}
	if !acquired {
		t.Fatal("new service lease was not acquired")
	}
	if initial.Name != "dispatcher" || initial.Owner != "node-a" {
		t.Fatalf("service lease identifiers were not normalized: %+v", initial)
	}
	if initial.AcquiredAt != "2030-01-02T03:04:05.123456789Z" ||
		initial.RenewedAt != initial.AcquiredAt ||
		initial.ExpiresAt != "2030-01-02T03:04:35.123456789Z" {
		t.Fatalf("service lease timestamps were not normalized: %+v", initial)
	}
	loaded, err := store.GetServiceLease(ctx, " dispatcher ")
	if err != nil || loaded != initial {
		t.Fatalf("get service lease = %+v, %v", loaded, err)
	}

	reacquired, acquired, err := store.AcquireServiceLease(ctx, "dispatcher", "node-a", 30*time.Second, current.Add(5*time.Second))
	if err != nil || !acquired {
		t.Fatalf("same owner did not reacquire its active lease: %+v, %v", reacquired, err)
	}
	if reacquired.AcquiredAt != initial.AcquiredAt || reacquired.RenewedAt != "2030-01-02T03:04:10.123456789Z" {
		t.Fatalf("same-owner acquisition reset the lease lifetime: %+v", reacquired)
	}

	busy, acquired, err := store.AcquireServiceLease(ctx, "dispatcher", "node-b", time.Minute, current.Add(10*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if acquired || busy.Owner != "node-a" || busy.ExpiresAt != reacquired.ExpiresAt {
		t.Fatalf("active lease was taken from its owner: acquired=%v lease=%+v", acquired, busy)
	}
	if _, err := store.RenewServiceLease(ctx, "dispatcher", "node-b", time.Minute, current.Add(10*time.Second)); !errors.Is(err, ErrServiceLeaseNotOwner) {
		t.Fatalf("other-owner renewal error = %v, want ErrServiceLeaseNotOwner", err)
	}
	if err := store.ReleaseServiceLease(ctx, "dispatcher", "node-b"); !errors.Is(err, ErrServiceLeaseNotOwner) {
		t.Fatalf("other-owner release error = %v, want ErrServiceLeaseNotOwner", err)
	}

	renewed, err := store.RenewServiceLease(ctx, "dispatcher", "node-a", 40*time.Second, current.Add(15*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if renewed.AcquiredAt != initial.AcquiredAt || renewed.RenewedAt != "2030-01-02T03:04:20.123456789Z" ||
		renewed.ExpiresAt != "2030-01-02T03:05:00.123456789Z" {
		t.Fatalf("unexpected renewed lease: %+v", renewed)
	}

	expiration := current.Add(55 * time.Second)
	if _, err := store.RenewServiceLease(ctx, "dispatcher", "node-a", time.Minute, expiration); !errors.Is(err, ErrServiceLeaseExpired) {
		t.Fatalf("expired renewal error = %v, want ErrServiceLeaseExpired", err)
	}
	taken, acquired, err := store.AcquireServiceLease(ctx, "dispatcher", "node-b", time.Minute, expiration)
	if err != nil || !acquired {
		t.Fatalf("expired lease was not taken over: %+v, acquired=%v, err=%v", taken, acquired, err)
	}
	if taken.Owner != "node-b" || taken.AcquiredAt != "2030-01-02T03:05:00.123456789Z" || taken.RenewedAt != taken.AcquiredAt {
		t.Fatalf("takeover did not reset lease ownership: %+v", taken)
	}
	if err := store.ReleaseServiceLease(ctx, "dispatcher", "node-a"); !errors.Is(err, ErrServiceLeaseNotOwner) {
		t.Fatalf("old owner release error = %v, want ErrServiceLeaseNotOwner", err)
	}
	if err := store.ReleaseServiceLease(ctx, " dispatcher ", " node-b "); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RenewServiceLease(ctx, "dispatcher", "node-b", time.Minute, expiration); !errors.Is(err, ErrServiceLeaseNotFound) {
		t.Fatalf("released lease renewal error = %v, want ErrServiceLeaseNotFound", err)
	}
}

func TestServiceLeaseValidation(t *testing.T) {
	ctx := context.Background()
	store, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	current := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, test := range []struct {
		name  string
		owner string
		ttl   time.Duration
	}{
		{name: " ", owner: "node", ttl: time.Second},
		{name: "dispatcher", owner: " ", ttl: time.Second},
		{name: "dispatcher", owner: "node", ttl: 0},
		{name: "dispatcher", owner: "node", ttl: -time.Second},
	} {
		if _, _, err := store.AcquireServiceLease(ctx, test.name, test.owner, test.ttl, current); err == nil {
			t.Fatalf("invalid acquisition was accepted: %+v", test)
		}
		if _, err := store.RenewServiceLease(ctx, test.name, test.owner, test.ttl, current); err == nil {
			t.Fatalf("invalid renewal was accepted: %+v", test)
		}
	}
	if err := store.ReleaseServiceLease(ctx, " ", "node"); err == nil {
		t.Fatal("blank lease name was accepted for release")
	}
	if err := store.ReleaseServiceLease(ctx, "dispatcher", " "); err == nil {
		t.Fatal("blank lease owner was accepted for release")
	}
}

func TestServiceLeaseExpiredTakeoverIsAtomic(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "autogora.db")
	stores := make([]*Store, 2)
	for index := range stores {
		var err error
		stores[index], err = Open(dbPath, "default", "")
		if err != nil {
			t.Fatal(err)
		}
		defer stores[index].Close()
	}

	current := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	if _, acquired, err := stores[0].AcquireServiceLease(ctx, "dispatcher", "old-node", time.Second, current); err != nil || !acquired {
		t.Fatalf("seed service lease: acquired=%v, err=%v", acquired, err)
	}

	type result struct {
		lease    ServiceLease
		acquired bool
		err      error
	}
	results := make([]result, len(stores))
	start := make(chan struct{})
	var wait sync.WaitGroup
	for index, store := range stores {
		wait.Add(1)
		go func(index int, store *Store) {
			defer wait.Done()
			<-start
			owner := "node-a"
			if index == 1 {
				owner = "node-b"
			}
			results[index].lease, results[index].acquired, results[index].err =
				store.AcquireServiceLease(ctx, "dispatcher", owner, time.Minute, current.Add(time.Second))
		}(index, store)
	}
	close(start)
	wait.Wait()

	winner := ""
	for index, result := range results {
		if result.err != nil {
			t.Fatalf("contender %d: %v", index, result.err)
		}
		if result.acquired {
			if winner != "" {
				t.Fatalf("multiple contenders acquired the lease: %+v", results)
			}
			winner = result.lease.Owner
		}
	}
	if winner == "" {
		t.Fatalf("no contender acquired the expired lease: %+v", results)
	}
	for _, result := range results {
		if result.lease.Owner != winner {
			t.Fatalf("contenders observed different leaders: winner=%s results=%+v", winner, results)
		}
	}
}

func TestSchema14ReopenAddsServiceLeases(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "autogora.db")
	initial, err := Open(dbPath, "default", "")
	if err != nil {
		t.Fatal(err)
	}
	task, err := initial.CreateTask(ctx, CreateTaskInput{Title: "preserved task"})
	if err != nil {
		t.Fatal(err)
	}
	if err := initial.Close(); err != nil {
		t.Fatal(err)
	}

	schema14, err := sql.Open("sqlite", dataSourceName(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := schema14.ExecContext(ctx, "DROP TABLE service_leases"); err != nil {
		schema14.Close()
		t.Fatal(err)
	}
	if _, err := schema14.ExecContext(ctx, "PRAGMA user_version = 14"); err != nil {
		schema14.Close()
		t.Fatal(err)
	}
	if err := schema14.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(dbPath, "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	preserved, err := reopened.GetTask(ctx, task.Task.ID)
	if err != nil || preserved.Task.Title != "preserved task" {
		t.Fatalf("schema upgrade lost existing data: %+v, %v", preserved, err)
	}
	var version int
	if err := reopened.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if schemaVersion != 20 {
		t.Fatalf("test requires schema version 20, got %d", schemaVersion)
	}
	if version != schemaVersion {
		t.Fatalf("schema version = %d, want %d", version, schemaVersion)
	}
	if _, acquired, err := reopened.AcquireServiceLease(ctx, "dispatcher", "node-a", time.Minute, time.Now()); err != nil || !acquired {
		t.Fatalf("additive service lease table is unusable: acquired=%v, err=%v", acquired, err)
	}
}

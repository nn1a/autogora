package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrServiceLeaseNotFound = errors.New("service lease not found")
	ErrServiceLeaseNotOwner = errors.New("service lease is owned by another owner")
	ErrServiceLeaseExpired  = errors.New("service lease has expired")
)

const (
	serviceLeaseColumns         = "name, owner, acquired_at, renewed_at, expires_at"
	serviceLeaseTimestampLayout = "2006-01-02T15:04:05.000000000Z"
)

// ServiceLease elects one process to supervise a named service until the
// lease expires. The current owner must renew it before ExpiresAt.
type ServiceLease struct {
	Name       string `json:"name"`
	Owner      string `json:"owner"`
	AcquiredAt string `json:"acquiredAt"`
	RenewedAt  string `json:"renewedAt"`
	ExpiresAt  string `json:"expiresAt"`
}

func scanServiceLease(row scanner) (ServiceLease, error) {
	var lease ServiceLease
	err := row.Scan(&lease.Name, &lease.Owner, &lease.AcquiredAt, &lease.RenewedAt, &lease.ExpiresAt)
	return lease, err
}

func (s *Store) GetServiceLease(ctx context.Context, name string) (ServiceLease, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return ServiceLease{}, errors.New("service lease requires a name")
	}
	lease, err := scanServiceLease(s.db.QueryRowContext(ctx,
		"SELECT "+serviceLeaseColumns+" FROM service_leases WHERE name = ?", name))
	if errors.Is(err, sql.ErrNoRows) {
		return ServiceLease{}, fmt.Errorf("%w: %q", ErrServiceLeaseNotFound, name)
	}
	return lease, err
}

func normalizeServiceLeaseArgs(name, owner string, ttl time.Duration, current time.Time) (string, string, string, string, error) {
	name = strings.TrimSpace(name)
	owner = strings.TrimSpace(owner)
	switch {
	case name == "":
		return "", "", "", "", errors.New("service lease requires a name")
	case owner == "":
		return "", "", "", "", errors.New("service lease requires an owner")
	case ttl <= 0:
		return "", "", "", "", errors.New("service lease TTL must be positive")
	}

	current = current.UTC()
	expires := current.Add(ttl)
	if current.Year() < 0 || current.Year() > 9999 || expires.Year() < 0 || expires.Year() > 9999 {
		return "", "", "", "", errors.New("service lease time must fit RFC3339")
	}
	return name, owner, current.Format(serviceLeaseTimestampLayout), expires.Format(serviceLeaseTimestampLayout), nil
}

func serviceLeaseExpired(lease ServiceLease, current string) (bool, error) {
	expires, err := time.Parse(time.RFC3339Nano, lease.ExpiresAt)
	if err != nil {
		return false, fmt.Errorf("parse service lease %q expiry: %w", lease.Name, err)
	}
	now, err := time.Parse(time.RFC3339Nano, current)
	if err != nil {
		return false, fmt.Errorf("parse current service lease time: %w", err)
	}
	return !expires.After(now), nil
}

func serviceLeaseOwnerError(lease ServiceLease, owner string) error {
	return fmt.Errorf("%w: %q is owned by %q, not %q", ErrServiceLeaseNotOwner, lease.Name, lease.Owner, owner)
}

// AcquireServiceLease creates a lease, renews one already held by owner, or
// atomically takes over an expired lease. A live lease held by another owner
// is returned with acquired set to false.
func (s *Store) AcquireServiceLease(ctx context.Context, name, owner string, ttl time.Duration, current time.Time) (lease ServiceLease, acquired bool, err error) {
	name, owner, timestamp, expiresAt, err := normalizeServiceLeaseArgs(name, owner, ttl, current)
	if err != nil {
		return ServiceLease{}, false, err
	}

	err = s.withWrite(ctx, func(tx *sql.Tx) error {
		lease, err = scanServiceLease(tx.QueryRowContext(ctx,
			"SELECT "+serviceLeaseColumns+" FROM service_leases WHERE name = ?", name))
		if errors.Is(err, sql.ErrNoRows) {
			_, err = tx.ExecContext(ctx, `INSERT INTO service_leases(
				name, owner, acquired_at, renewed_at, expires_at
			) VALUES (?, ?, ?, ?, ?)`, name, owner, timestamp, timestamp, expiresAt)
			if err != nil {
				return fmt.Errorf("acquire service lease %q: %w", name, err)
			}
			lease = ServiceLease{
				Name: name, Owner: owner, AcquiredAt: timestamp,
				RenewedAt: timestamp, ExpiresAt: expiresAt,
			}
			acquired = true
			return nil
		}
		if err != nil {
			return fmt.Errorf("read service lease %q: %w", name, err)
		}

		expired, err := serviceLeaseExpired(lease, timestamp)
		if err != nil {
			return err
		}
		if !expired && lease.Owner != owner {
			acquired = false
			return nil
		}

		acquiredAt := lease.AcquiredAt
		if expired {
			acquiredAt = timestamp
		}
		if _, err := tx.ExecContext(ctx, `UPDATE service_leases
			SET owner = ?, acquired_at = ?, renewed_at = ?, expires_at = ?
			WHERE name = ?`, owner, acquiredAt, timestamp, expiresAt, name); err != nil {
			return fmt.Errorf("acquire service lease %q: %w", name, err)
		}
		lease = ServiceLease{
			Name: name, Owner: owner, AcquiredAt: acquiredAt,
			RenewedAt: timestamp, ExpiresAt: expiresAt,
		}
		acquired = true
		return nil
	})
	return lease, acquired, err
}

// RenewServiceLease extends an active lease held by owner. An expired lease
// must be acquired again so a stale owner cannot overwrite a new election.
func (s *Store) RenewServiceLease(ctx context.Context, name, owner string, ttl time.Duration, current time.Time) (lease ServiceLease, err error) {
	name, owner, timestamp, expiresAt, err := normalizeServiceLeaseArgs(name, owner, ttl, current)
	if err != nil {
		return ServiceLease{}, err
	}

	err = s.withWrite(ctx, func(tx *sql.Tx) error {
		lease, err = scanServiceLease(tx.QueryRowContext(ctx,
			"SELECT "+serviceLeaseColumns+" FROM service_leases WHERE name = ?", name))
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: %q", ErrServiceLeaseNotFound, name)
		}
		if err != nil {
			return fmt.Errorf("read service lease %q: %w", name, err)
		}
		if lease.Owner != owner {
			return serviceLeaseOwnerError(lease, owner)
		}
		expired, err := serviceLeaseExpired(lease, timestamp)
		if err != nil {
			return err
		}
		if expired {
			return fmt.Errorf("%w: %q", ErrServiceLeaseExpired, name)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE service_leases
			SET renewed_at = ?, expires_at = ? WHERE name = ? AND owner = ?`,
			timestamp, expiresAt, name, owner); err != nil {
			return fmt.Errorf("renew service lease %q: %w", name, err)
		}
		lease.RenewedAt = timestamp
		lease.ExpiresAt = expiresAt
		return nil
	})
	return lease, err
}

// ReleaseServiceLease removes a lease only when owner still holds it.
func (s *Store) ReleaseServiceLease(ctx context.Context, name, owner string) error {
	name = strings.TrimSpace(name)
	owner = strings.TrimSpace(owner)
	if name == "" {
		return errors.New("service lease requires a name")
	}
	if owner == "" {
		return errors.New("service lease requires an owner")
	}

	return s.withWrite(ctx, func(tx *sql.Tx) error {
		lease, err := scanServiceLease(tx.QueryRowContext(ctx,
			"SELECT "+serviceLeaseColumns+" FROM service_leases WHERE name = ?", name))
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: %q", ErrServiceLeaseNotFound, name)
		}
		if err != nil {
			return fmt.Errorf("read service lease %q: %w", name, err)
		}
		if lease.Owner != owner {
			return serviceLeaseOwnerError(lease, owner)
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM service_leases WHERE name = ? AND owner = ?", name, owner); err != nil {
			return fmt.Errorf("release service lease %q: %w", name, err)
		}
		return nil
	})
}

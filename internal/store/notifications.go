package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/nn1a/kanban/internal/model"
)

var TerminalNotificationKinds = []string{"completed", "blocked", "gave_up", "crashed", "timed_out"}

type NotificationSubscription struct {
	ID          string   `json:"id"`
	TaskID      string   `json:"taskId"`
	Platform    string   `json:"platform"`
	ChatID      string   `json:"chatId"`
	ThreadID    *string  `json:"threadId"`
	UserID      *string  `json:"userId"`
	EventKinds  []string `json:"eventKinds"`
	HasSecret   bool     `json:"hasSecret"`
	LastEventID int64    `json:"lastEventId"`
	CreatedAt   string   `json:"createdAt"`
	UpdatedAt   string   `json:"updatedAt"`
}

type SubscriptionInput struct {
	TaskID     string
	Platform   string
	ChatID     string
	ThreadID   *string
	UserID     *string
	EventKinds []string
	Secret     OptionalString
}

type ClaimedNotificationDelivery struct {
	ID           string                   `json:"id"`
	LeaseToken   string                   `json:"leaseToken"`
	Subscription NotificationSubscription `json:"subscription"`
	Secret       *string                  `json:"secret"`
	Event        model.TaskEvent          `json:"event"`
	Task         model.Task               `json:"task"`
	Attempts     int                      `json:"attempts"`
}

type notificationRow struct {
	NotificationSubscription
	secret *string
	thread string
}

type deliveryRow struct {
	id             string
	subscriptionID string
	eventID        int64
	status         string
	attempts       int
	leaseToken     *string
	leaseExpiresAt *string
	nextAttemptAt  string
	lastError      *string
	deliveredAt    *string
	createdAt      string
}

func scanSubscription(row scanner) (notificationRow, error) {
	var value notificationRow
	var userID, secret sql.NullString
	var kinds string
	if err := row.Scan(&value.ID, &value.TaskID, &value.Platform, &value.ChatID, &value.thread, &userID,
		&kinds, &secret, &value.LastEventID, &value.CreatedAt, &value.UpdatedAt); err != nil {
		return notificationRow{}, err
	}
	value.UserID, value.secret, value.HasSecret = stringPointer(userID), stringPointer(secret), secret.Valid
	if value.thread != "" {
		thread := value.thread
		value.ThreadID = &thread
	}
	if err := json.Unmarshal([]byte(kinds), &value.EventKinds); err != nil {
		return notificationRow{}, err
	}
	return value, nil
}

func scanDelivery(row scanner) (deliveryRow, error) {
	var value deliveryRow
	var leaseToken, leaseExpiresAt, lastError, deliveredAt sql.NullString
	if err := row.Scan(&value.id, &value.subscriptionID, &value.eventID, &value.status, &value.attempts,
		&leaseToken, &leaseExpiresAt, &value.nextAttemptAt, &lastError, &deliveredAt, &value.createdAt); err != nil {
		return deliveryRow{}, err
	}
	value.leaseToken, value.leaseExpiresAt, value.lastError, value.deliveredAt = stringPointer(leaseToken), stringPointer(leaseExpiresAt), stringPointer(lastError), stringPointer(deliveredAt)
	return value, nil
}

const subscriptionColumns = "id, task_id, platform, chat_id, thread_id, user_id, event_kinds_json, secret, last_event_id, created_at, updated_at"
const deliveryColumns = "id, subscription_id, event_id, status, attempts, lease_token, lease_expires_at, next_attempt_at, last_error, delivered_at, created_at"

func (s *Store) SubscribeTask(ctx context.Context, input SubscriptionInput) (NotificationSubscription, error) {
	platform, chatID := strings.ToLower(strings.TrimSpace(input.Platform)), strings.TrimSpace(input.ChatID)
	thread := ""
	if input.ThreadID != nil {
		thread = strings.TrimSpace(*input.ThreadID)
	}
	userID := normalizedPointer(input.UserID)
	kinds := normalizeSkills(input.EventKinds)
	if len(kinds) == 0 {
		kinds = append([]string{}, TerminalNotificationKinds...)
	}
	if platform == "" {
		return NotificationSubscription{}, errors.New("notification platform cannot be empty")
	}
	if chatID == "" {
		return NotificationSubscription{}, errors.New("notification chat id cannot be empty")
	}
	encodedKinds, _ := json.Marshal(kinds)
	subscriptionID := ""
	err := s.withWrite(ctx, func(tx *sql.Tx) error {
		task, err := requireTask(ctx, tx, input.TaskID)
		if err != nil {
			return err
		}
		if task.Status == model.TaskStatusDone || task.Status == model.TaskStatusArchived {
			return fmt.Errorf("cannot subscribe to a %s task", task.Status)
		}
		existing, err := scanSubscription(tx.QueryRowContext(ctx, "SELECT "+subscriptionColumns+" FROM notification_subscriptions WHERE task_id = ? AND platform = ? AND chat_id = ? AND thread_id = ?", input.TaskID, platform, chatID, thread))
		timestamp := now()
		if err == nil {
			subscriptionID = existing.ID
			secret := nullableString(existing.secret)
			if input.Secret.Set {
				secret = nullableString(input.Secret.Value)
			}
			_, err = tx.ExecContext(ctx, "UPDATE notification_subscriptions SET user_id = ?, event_kinds_json = ?, secret = ?, updated_at = ? WHERE id = ?", nullableString(userID), string(encodedKinds), secret, timestamp, existing.ID)
			return err
		}
		if err != sql.ErrNoRows {
			return err
		}
		subscriptionID = newID("nsub")
		var latest int64
		if err := tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(id), 0) FROM task_events WHERE task_id = ?", input.TaskID).Scan(&latest); err != nil {
			return err
		}
		var secret any
		if input.Secret.Set {
			secret = nullableString(input.Secret.Value)
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO notification_subscriptions(id, task_id, platform, chat_id, thread_id,
			user_id, event_kinds_json, secret, last_event_id, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			subscriptionID, input.TaskID, platform, chatID, thread, nullableString(userID), string(encodedKinds), secret, latest, timestamp, timestamp)
		return err
	})
	if err != nil {
		return NotificationSubscription{}, err
	}
	row, err := scanSubscription(s.db.QueryRowContext(ctx, "SELECT "+subscriptionColumns+" FROM notification_subscriptions WHERE id = ?", subscriptionID))
	return row.NotificationSubscription, err
}

func (s *Store) ListNotificationSubscriptions(ctx context.Context, taskID string) ([]NotificationSubscription, error) {
	if taskID != "" {
		if _, err := requireTask(ctx, s.db, taskID); err != nil {
			return nil, err
		}
	}
	statement, args := "SELECT s."+strings.ReplaceAll(subscriptionColumns, ", ", ", s.")+" FROM notification_subscriptions s JOIN tasks t ON t.id = s.task_id WHERE t.board = ? ORDER BY s.created_at", []any{s.board}
	if taskID != "" {
		statement, args = "SELECT "+subscriptionColumns+" FROM notification_subscriptions WHERE task_id = ? ORDER BY created_at", []any{taskID}
	}
	rows, err := s.db.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []NotificationSubscription{}
	for rows.Next() {
		value, err := scanSubscription(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, value.NotificationSubscription)
	}
	return result, rows.Err()
}

func (s *Store) UnsubscribeTask(ctx context.Context, taskID, platform, chatID string, threadID *string) (bool, error) {
	thread := ""
	if threadID != nil {
		thread = strings.TrimSpace(*threadID)
	}
	removed := false
	err := s.withWrite(ctx, func(tx *sql.Tx) error {
		if _, err := requireTask(ctx, tx, taskID); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, "DELETE FROM notification_subscriptions WHERE task_id = ? AND platform = ? AND chat_id = ? AND thread_id = ?", taskID, strings.ToLower(strings.TrimSpace(platform)), strings.TrimSpace(chatID), thread)
		if err != nil {
			return err
		}
		changes, _ := result.RowsAffected()
		removed = changes > 0
		return nil
	})
	return removed, err
}

func (s *Store) ClaimNotificationDeliveries(ctx context.Context, limit, leaseSeconds int) ([]ClaimedNotificationDelivery, error) {
	if limit <= 0 {
		limit = 25
	}
	limit = min(limit, 500)
	if leaseSeconds <= 0 {
		leaseSeconds = 30
	}
	claimed := []ClaimedNotificationDelivery{}
	err := s.withWrite(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "DELETE FROM notification_subscriptions WHERE task_id IN (SELECT id FROM tasks WHERE board = ? AND status = 'archived')", s.board); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, "SELECT s."+strings.ReplaceAll(subscriptionColumns, ", ", ", s.")+" FROM notification_subscriptions s JOIN tasks t ON t.id = s.task_id WHERE t.board = ? ORDER BY s.created_at", s.board)
		if err != nil {
			return err
		}
		subscriptions := []notificationRow{}
		for rows.Next() {
			value, err := scanSubscription(rows)
			if err != nil {
				rows.Close()
				return err
			}
			subscriptions = append(subscriptions, value)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, subscription := range subscriptions {
			if len(subscription.EventKinds) == 0 {
				continue
			}
			placeholders := make([]string, len(subscription.EventKinds))
			args := []any{subscription.TaskID, subscription.LastEventID}
			for index, kind := range subscription.EventKinds {
				placeholders[index] = "?"
				args = append(args, kind)
			}
			args = append(args, subscription.ID)
			var eventID int64
			err := tx.QueryRowContext(ctx, `SELECT e.id FROM task_events e WHERE e.task_id = ? AND e.id > ? AND e.kind IN (`+strings.Join(placeholders, ",")+`)
				AND NOT EXISTS (SELECT 1 FROM notification_deliveries d WHERE d.subscription_id = ? AND d.event_id = e.id) ORDER BY e.id ASC LIMIT 1`, args...).Scan(&eventID)
			if err == nil {
				timestamp := now()
				if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO notification_deliveries(id, subscription_id, event_id, status, attempts, next_attempt_at, created_at)
				VALUES (?, ?, ?, 'pending', 0, ?, ?)`, newID("ndel"), subscription.ID, eventID, timestamp, timestamp); err != nil {
					return err
				}
			} else if err != sql.ErrNoRows {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM notification_subscriptions WHERE task_id IN (SELECT id FROM tasks WHERE board = ? AND status = 'done')
			AND NOT EXISTS (SELECT 1 FROM notification_deliveries d WHERE d.subscription_id = notification_subscriptions.id AND d.status <> 'delivered')`, s.board); err != nil {
			return err
		}
		timestamp := now()
		rows, err = tx.QueryContext(ctx, "SELECT d."+strings.ReplaceAll(deliveryColumns, ", ", ", d.")+` FROM notification_deliveries d
			JOIN notification_subscriptions s ON s.id = d.subscription_id JOIN tasks t ON t.id = s.task_id
			WHERE t.board = ? AND ((d.status = 'pending' AND d.next_attempt_at <= ?) OR (d.status = 'delivering' AND d.lease_expires_at <= ?)) ORDER BY d.created_at, d.event_id`, s.board, timestamp, timestamp)
		if err != nil {
			return err
		}
		due := []deliveryRow{}
		for rows.Next() {
			value, err := scanDelivery(rows)
			if err != nil {
				rows.Close()
				return err
			}
			due = append(due, value)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		seen := map[string]bool{}
		for _, delivery := range due {
			if len(claimed) >= limit {
				break
			}
			if seen[delivery.subscriptionID] {
				continue
			}
			token, err := claimToken()
			if err != nil {
				return err
			}
			expires := futureISO(leaseSeconds)
			result, err := tx.ExecContext(ctx, `UPDATE notification_deliveries SET status = 'delivering', attempts = attempts + 1, lease_token = ?, lease_expires_at = ? WHERE id = ?
			AND ((status = 'pending' AND next_attempt_at <= ?) OR (status = 'delivering' AND lease_expires_at <= ?))`, token, expires, delivery.id, timestamp, timestamp)
			if err != nil {
				return err
			}
			changes, _ := result.RowsAffected()
			if changes != 1 {
				continue
			}
			subscription, err := scanSubscription(tx.QueryRowContext(ctx, "SELECT "+subscriptionColumns+" FROM notification_subscriptions WHERE id = ?", delivery.subscriptionID))
			if err != nil {
				return err
			}
			event, err := scanEvent(tx.QueryRowContext(ctx, "SELECT id, task_id, run_id, kind, payload_json, created_at FROM task_events WHERE id = ?", delivery.eventID))
			if err != nil {
				return err
			}
			task, err := requireTask(ctx, tx, subscription.TaskID)
			if err != nil {
				return err
			}
			claimed = append(claimed, ClaimedNotificationDelivery{ID: delivery.id, LeaseToken: token, Subscription: subscription.NotificationSubscription, Secret: subscription.secret, Event: event, Task: task, Attempts: delivery.attempts + 1})
			seen[delivery.subscriptionID] = true
		}
		return nil
	})
	return claimed, err
}

func (s *Store) ResolveNotificationDelivery(ctx context.Context, deliveryID, leaseToken string, deliveryError *string) error {
	return s.withWrite(ctx, func(tx *sql.Tx) error {
		delivery, err := scanDelivery(tx.QueryRowContext(ctx, "SELECT "+deliveryColumns+" FROM notification_deliveries WHERE id = ?", deliveryID))
		if err == sql.ErrNoRows {
			return nil
		}
		if err != nil {
			return err
		}
		if delivery.status != "delivering" || delivery.leaseToken == nil || *delivery.leaseToken != leaseToken {
			return fmt.Errorf("notification delivery lease is no longer active: %s", deliveryID)
		}
		timestamp := now()
		if deliveryError == nil {
			event, err := scanEvent(tx.QueryRowContext(ctx, "SELECT id, task_id, run_id, kind, payload_json, created_at FROM task_events WHERE id = ?", delivery.eventID))
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, "UPDATE notification_deliveries SET status = 'delivered', delivered_at = ?, lease_token = NULL, lease_expires_at = NULL, last_error = NULL WHERE id = ?", timestamp, deliveryID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, "UPDATE notification_subscriptions SET last_event_id = MAX(last_event_id, ?), updated_at = ? WHERE id = ?", delivery.eventID, timestamp, delivery.subscriptionID); err != nil {
				return err
			}
			if event.Kind == "completed" || event.Kind == "archived" {
				_, err = tx.ExecContext(ctx, "DELETE FROM notification_subscriptions WHERE id = ?", delivery.subscriptionID)
			}
			return err
		}
		delay := int(math.Min(300, math.Pow(2, float64(min(delivery.attempts, 8)))))
		message := *deliveryError
		if len(message) > 2000 {
			message = message[:2000]
		}
		_, err = tx.ExecContext(ctx, "UPDATE notification_deliveries SET status = 'pending', lease_token = NULL, lease_expires_at = NULL, next_attempt_at = ?, last_error = ? WHERE id = ?", futureISO(delay), message, deliveryID)
		return err
	})
}

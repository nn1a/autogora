package notifications

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/nn1a/kanban/internal/model"
	"github.com/nn1a/kanban/internal/store"
)

type PayloadTask struct {
	ID          string           `json:"id"`
	Title       string           `json:"title"`
	Status      model.TaskStatus `json:"status"`
	Assignee    *string          `json:"assignee"`
	Result      *string          `json:"result"`
	BlockReason *string          `json:"blockReason"`
}

type Payload struct {
	DeliveryID string          `json:"deliveryId"`
	Board      string          `json:"board"`
	Platform   string          `json:"platform"`
	ChatID     string          `json:"chatId"`
	ThreadID   *string         `json:"threadId"`
	UserID     *string         `json:"userId"`
	Message    string          `json:"message"`
	Task       PayloadTask     `json:"task"`
	Event      model.TaskEvent `json:"event"`
}

type Result struct {
	DeliveryID     string `json:"deliveryId"`
	SubscriptionID string `json:"subscriptionId"`
	TaskID         string `json:"taskId"`
	EventID        int64  `json:"eventId"`
	EventKind      string `json:"eventKind"`
	Delivered      bool   `json:"delivered"`
	Error          string `json:"error,omitempty"`
}

type Adapter func(context.Context, Payload, store.ClaimedNotificationDelivery, time.Duration) error

type Options struct {
	Limit        int
	LeaseSeconds int
	Timeout      time.Duration
	Adapters     map[string]Adapter
	HTTPClient   *http.Client
}

func firstLine(value string) string {
	value = strings.TrimSpace(strings.Split(strings.ReplaceAll(value, "\r\n", "\n"), "\n")[0])
	runes := []rune(value)
	if len(runes) > 400 {
		value = string(runes[:400])
	}
	return value
}

func eventSummary(event model.TaskEvent) string {
	if len(event.Payload) == 0 {
		return ""
	}
	var payload map[string]any
	if json.Unmarshal(event.Payload, &payload) != nil {
		return ""
	}
	value, _ := payload["summary"].(string)
	return firstLine(value)
}

func messageFor(delivery store.ClaimedNotificationDelivery) string {
	task, event := delivery.Task, delivery.Event
	actor := ""
	if task.Assignee != nil {
		actor = " by " + *task.Assignee
	}
	switch event.Kind {
	case "completed":
		summary := eventSummary(event)
		if summary == "" && task.Result != nil {
			summary = firstLine(*task.Result)
		}
		message := fmt.Sprintf("✓ %s completed%s", task.ID, actor)
		if summary != "" {
			message += "\n" + summary
		}
		return message
	case "blocked":
		message := fmt.Sprintf("! %s blocked%s", task.ID, actor)
		if task.BlockReason != nil && firstLine(*task.BlockReason) != "" {
			message += "\n" + firstLine(*task.BlockReason)
		}
		return message
	case "gave_up":
		return fmt.Sprintf("! %s gave up after its retry budget was exhausted", task.ID)
	case "crashed":
		return fmt.Sprintf("! %s worker crashed%s", task.ID, actor)
	case "timed_out":
		return fmt.Sprintf("! %s worker timed out%s", task.ID, actor)
	default:
		return fmt.Sprintf("%s: %s", task.ID, event.Kind)
	}
}

func payloadFor(delivery store.ClaimedNotificationDelivery) Payload {
	return Payload{DeliveryID: delivery.ID, Board: delivery.Task.Board, Platform: delivery.Subscription.Platform,
		ChatID: delivery.Subscription.ChatID, ThreadID: delivery.Subscription.ThreadID, UserID: delivery.Subscription.UserID,
		Message: messageFor(delivery), Task: PayloadTask{ID: delivery.Task.ID, Title: delivery.Task.Title,
			Status: delivery.Task.Status, Assignee: delivery.Task.Assignee, Result: delivery.Task.Result, BlockReason: delivery.Task.BlockReason}, Event: delivery.Event}
}

func WebhookAdapter(client *http.Client) Adapter {
	if client == nil {
		client = http.DefaultClient
	}
	return func(ctx context.Context, payload Payload, delivery store.ClaimedNotificationDelivery, timeout time.Duration) error {
		target, err := url.ParseRequestURI(delivery.Subscription.ChatID)
		if err != nil || target.Host == "" || (target.Scheme != "http" && target.Scheme != "https") {
			return fmt.Errorf("webhook notification targets must use HTTP(S)")
		}
		body, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		if timeout < 100*time.Millisecond {
			timeout = 100 * time.Millisecond
		}
		requestContext, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		request, err := http.NewRequestWithContext(requestContext, http.MethodPost, target.String(), bytes.NewReader(body))
		if err != nil {
			return err
		}
		request.Header.Set("content-type", "application/json")
		request.Header.Set("x-taskcircuit-delivery-id", delivery.ID)
		request.Header.Set("x-taskcircuit-event", delivery.Event.Kind)
		if delivery.Secret != nil {
			digest := hmac.New(sha256.New, []byte(*delivery.Secret))
			_, _ = digest.Write(body)
			request.Header.Set("x-taskcircuit-signature", "sha256="+hex.EncodeToString(digest.Sum(nil)))
		}
		response, err := client.Do(request)
		if err != nil {
			return err
		}
		defer response.Body.Close()
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			responseBody, _ := io.ReadAll(io.LimitReader(response.Body, 501))
			message := strings.TrimSpace(string(responseBody))
			if len(message) > 500 {
				message = message[:500]
			}
			if message != "" {
				return fmt.Errorf("webhook returned HTTP %d: %s", response.StatusCode, message)
			}
			return fmt.Errorf("webhook returned HTTP %d", response.StatusCode)
		}
		return nil
	}
}

func Deliver(ctx context.Context, opened *store.Store, options Options) ([]Result, error) {
	if options.Limit <= 0 {
		options.Limit = 25
	}
	if options.Timeout <= 0 {
		options.Timeout = 10 * time.Second
	}
	leaseSeconds := max(options.LeaseSeconds, int((options.Timeout+time.Second-1)/time.Second)+5)
	leaseSeconds = max(30, leaseSeconds)
	deliveries, err := opened.ClaimNotificationDeliveries(ctx, options.Limit, leaseSeconds)
	if err != nil {
		return nil, err
	}
	adapters := map[string]Adapter{"webhook": WebhookAdapter(options.HTTPClient)}
	for name, adapter := range options.Adapters {
		adapters[name] = adapter
	}
	results := make([]Result, len(deliveries))
	var wait sync.WaitGroup
	for index, delivery := range deliveries {
		wait.Add(1)
		go func(index int, delivery store.ClaimedNotificationDelivery) {
			defer wait.Done()
			result := Result{DeliveryID: delivery.ID, SubscriptionID: delivery.Subscription.ID, TaskID: delivery.Task.ID,
				EventID: delivery.Event.ID, EventKind: delivery.Event.Kind}
			adapter := adapters[delivery.Subscription.Platform]
			var deliveryErr error
			if adapter == nil {
				deliveryErr = fmt.Errorf("no notification adapter is registered for platform: %s", delivery.Subscription.Platform)
			} else {
				deliveryErr = adapter(ctx, payloadFor(delivery), delivery, options.Timeout)
			}
			if deliveryErr == nil {
				deliveryErr = opened.ResolveNotificationDelivery(ctx, delivery.ID, delivery.LeaseToken, nil)
			} else {
				message := deliveryErr.Error()
				if resolutionErr := opened.ResolveNotificationDelivery(ctx, delivery.ID, delivery.LeaseToken, &message); resolutionErr != nil {
					deliveryErr = fmt.Errorf("%v; delivery state update failed: %w", deliveryErr, resolutionErr)
				}
			}
			result.Delivered = deliveryErr == nil
			if deliveryErr != nil {
				result.Error = deliveryErr.Error()
			}
			results[index] = result
		}(index, delivery)
	}
	wait.Wait()
	return results, nil
}

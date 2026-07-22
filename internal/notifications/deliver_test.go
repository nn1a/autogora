package notifications

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nn1a/kanban/internal/model"
	"github.com/nn1a/kanban/internal/store"
)

func completedSubscribedTask(t *testing.T, opened *store.Store, platform, chatID, secret string) string {
	t.Helper()
	ctx := context.Background()
	assignee := "worker"
	task, err := opened.CreateTask(ctx, store.CreateTaskInput{Title: "Notify", Assignee: &assignee, Runtime: model.RuntimeCodex})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := opened.SubscribeTask(ctx, store.SubscriptionInput{TaskID: task.Task.ID, Platform: platform, ChatID: chatID, Secret: store.OptionalString{Set: true, Value: &secret}}); err != nil {
		t.Fatal(err)
	}
	claim, err := opened.ClaimTask(ctx, store.ClaimOptions{TaskID: task.Task.ID})
	if err != nil || claim == nil {
		t.Fatalf("claim: %v %v", claim, err)
	}
	if _, err := opened.CompleteRun(ctx, store.RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken}, store.CompletionInput{Summary: "verified result"}); err != nil {
		t.Fatal(err)
	}
	return task.Task.ID
}

func TestWebhookDeliverySignsPayloadAndRemovesTerminalSubscription(t *testing.T) {
	ctx := context.Background()
	secret := "webhook-secret"
	var receivedBody []byte
	var receivedSignature, deliveryID, eventKind string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var err error
		receivedBody, err = io.ReadAll(request.Body)
		if err != nil {
			t.Error(err)
		}
		receivedSignature = request.Header.Get("x-kanban-signature")
		deliveryID = request.Header.Get("x-kanban-delivery-id")
		eventKind = request.Header.Get("x-kanban-event")
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	opened, err := store.Open(filepath.Join(t.TempDir(), "kanban.db"), "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	taskID := completedSubscribedTask(t, opened, "webhook", server.URL, secret)
	results, err := Deliver(ctx, opened, Options{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || !results[0].Delivered || results[0].TaskID != taskID || deliveryID == "" || eventKind != "completed" {
		t.Fatalf("unexpected delivery: %+v", results)
	}
	digest := hmac.New(sha256.New, []byte(secret))
	_, _ = digest.Write(receivedBody)
	expectedSignature := "sha256=" + hex.EncodeToString(digest.Sum(nil))
	if receivedSignature != expectedSignature {
		t.Fatalf("signature = %q, want %q", receivedSignature, expectedSignature)
	}
	var payload Payload
	if err := json.Unmarshal(receivedBody, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.DeliveryID != deliveryID || !strings.Contains(payload.Message, "verified result") {
		t.Fatalf("payload mismatch: %+v", payload)
	}
	subscriptions, err := opened.ListNotificationSubscriptions(ctx, taskID)
	if err != nil || len(subscriptions) != 0 {
		t.Fatalf("terminal subscription remains: %+v %v", subscriptions, err)
	}
}

func TestDeliveryFailureReturnsToPendingWithoutExposingSecret(t *testing.T) {
	ctx := context.Background()
	secret := "must-not-leak"
	opened, err := store.Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	taskID := completedSubscribedTask(t, opened, "unsupported", "destination", secret)
	results, err := Deliver(ctx, opened, Options{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Delivered || !strings.Contains(results[0].Error, "no notification adapter") {
		t.Fatalf("unexpected failure: %+v", results)
	}
	encoded, _ := json.Marshal(results)
	if strings.Contains(string(encoded), secret) {
		t.Fatalf("delivery result exposed secret: %s", encoded)
	}
	subscriptions, err := opened.ListNotificationSubscriptions(ctx, taskID)
	if err != nil || len(subscriptions) != 1 || !subscriptions[0].HasSecret {
		t.Fatalf("failed delivery did not remain subscribed: %+v %v", subscriptions, err)
	}
}

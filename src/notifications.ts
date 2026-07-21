import { createHmac } from "node:crypto";

import {
  KanbanStore,
  type ClaimedNotificationDelivery,
} from "./store.js";

export interface NotificationPayload {
  deliveryId: string;
  board: string;
  platform: string;
  chatId: string;
  threadId: string | null;
  userId: string | null;
  message: string;
  task: {
    id: string;
    title: string;
    status: string;
    assignee: string | null;
    result: string | null;
    blockReason: string | null;
  };
  event: ClaimedNotificationDelivery["event"];
}

export type NotificationAdapter = (
  payload: NotificationPayload,
  delivery: ClaimedNotificationDelivery,
  timeoutMs: number,
) => Promise<void>;

export interface NotificationDeliveryResult {
  deliveryId: string;
  subscriptionId: string;
  taskId: string;
  eventId: number;
  eventKind: string;
  delivered: boolean;
  error?: string | undefined;
}

function firstLine(value: string | null | undefined): string | null {
  const line = value?.split(/\r?\n/, 1)[0]?.trim();
  return line ? line.slice(0, 400) : null;
}

function messageFor(delivery: ClaimedNotificationDelivery): string {
  const { task, event } = delivery;
  const actor = task.assignee ? ` by ${task.assignee}` : "";
  if (event.kind === "completed") {
    const summary = typeof event.payload?.summary === "string"
      ? firstLine(event.payload.summary)
      : firstLine(task.result);
    return `✓ ${task.id} completed${actor}${summary ? `\n${summary}` : ""}`;
  }
  if (event.kind === "blocked") {
    return `! ${task.id} blocked${actor}${task.blockReason ? `\n${firstLine(task.blockReason)}` : ""}`;
  }
  if (event.kind === "gave_up") return `! ${task.id} gave up after its retry budget was exhausted`;
  if (event.kind === "crashed") return `! ${task.id} worker crashed${actor}`;
  if (event.kind === "timed_out") return `! ${task.id} worker timed out${actor}`;
  return `${task.id}: ${event.kind}`;
}

function payloadFor(delivery: ClaimedNotificationDelivery): NotificationPayload {
  return {
    deliveryId: delivery.id,
    board: delivery.task.board,
    platform: delivery.subscription.platform,
    chatId: delivery.subscription.chatId,
    threadId: delivery.subscription.threadId,
    userId: delivery.subscription.userId,
    message: messageFor(delivery),
    task: {
      id: delivery.task.id,
      title: delivery.task.title,
      status: delivery.task.status,
      assignee: delivery.task.assignee,
      result: delivery.task.result,
      blockReason: delivery.task.blockReason,
    },
    event: delivery.event,
  };
}

export const webhookNotificationAdapter: NotificationAdapter = async (payload, delivery, timeoutMs) => {
  const target = new URL(delivery.subscription.chatId);
  if (!["http:", "https:"].includes(target.protocol)) {
    throw new Error("Webhook notification targets must use HTTP(S)");
  }
  const body = JSON.stringify(payload);
  const headers: Record<string, string> = {
    "content-type": "application/json",
    "x-kanban-delivery-id": delivery.id,
    "x-kanban-event": delivery.event.kind,
  };
  if (delivery.secret !== null) {
    headers["x-kanban-signature"] = `sha256=${createHmac("sha256", delivery.secret).update(body).digest("hex")}`;
  }
  const response = await fetch(target, {
    method: "POST",
    headers,
    body,
    signal: AbortSignal.timeout(Math.max(100, timeoutMs)),
  });
  if (!response.ok) {
    const responseBody = (await response.text()).slice(0, 500).trim();
    throw new Error(`Webhook returned HTTP ${response.status}${responseBody ? `: ${responseBody}` : ""}`);
  }
};

export async function deliverNotifications(
  store: KanbanStore,
  options: {
    limit?: number | undefined;
    leaseSeconds?: number | undefined;
    timeoutMs?: number | undefined;
    adapters?: Record<string, NotificationAdapter> | undefined;
  } = {},
): Promise<NotificationDeliveryResult[]> {
  const timeoutMs = options.timeoutMs ?? 10_000;
  const leaseSeconds = Math.max(options.leaseSeconds ?? 30, Math.ceil(timeoutMs / 1_000) + 5);
  const deliveries = store.claimNotificationDeliveries(options.limit ?? 25, leaseSeconds);
  const adapters: Record<string, NotificationAdapter> = {
    webhook: webhookNotificationAdapter,
    ...(options.adapters ?? {}),
  };
  return Promise.all(deliveries.map(async (delivery) => {
    let error: string | undefined;
    try {
      const adapter = adapters[delivery.subscription.platform];
      if (!adapter) throw new Error(`No notification adapter is registered for platform: ${delivery.subscription.platform}`);
      await adapter(payloadFor(delivery), delivery, timeoutMs);
    } catch (cause) {
      error = cause instanceof Error ? cause.message : String(cause);
    }
    if (error === undefined) {
      try {
        store.resolveNotificationDelivery(delivery.id, delivery.leaseToken, {});
      } catch (cause) {
        error = cause instanceof Error ? cause.message : String(cause);
      }
    } else {
      try {
        store.resolveNotificationDelivery(delivery.id, delivery.leaseToken, { error });
      } catch (cause) {
        const resolutionError = cause instanceof Error ? cause.message : String(cause);
        error = `${error}; delivery state update failed: ${resolutionError}`;
      }
    }
    return {
      deliveryId: delivery.id,
      subscriptionId: delivery.subscription.id,
      taskId: delivery.task.id,
      eventId: delivery.event.id,
      eventKind: delivery.event.kind,
      delivered: error === undefined,
      error,
    };
  }));
}

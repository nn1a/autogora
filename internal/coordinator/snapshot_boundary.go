package coordinator

import (
	"encoding/json"
	"path"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/nn1a/autogora/internal/model"
)

const (
	coordinatorChangedPathLimit = 256
	coordinatorChangedFileLimit = 12
)

var coordinatorPublicIdentifier = regexp.MustCompile(`^[A-Za-z0-9_.:@+-]{1,256}$`)

// SanitizeIncidentSnapshot is the single trust boundary for data handed to an
// external Coordinator. Store records intentionally retain operator-facing
// diagnostics, while this copy contains only stable machine state and bounded
// repository-relative changed paths. In particular, raw errors, block
// reasons, run summaries, workspace diagnostics, filesystem locations, Git
// refs, and lease credentials never cross this boundary.
func SanitizeIncidentSnapshot(snapshot IncidentSnapshot) IncidentSnapshot {
	safe := snapshot
	safe.IncidentID = publicIdentifier(snapshot.IncidentID)
	safe.Trigger = publicIncidentCode(snapshot.Trigger)
	safe.Severity = publicSeverity(snapshot.Severity)
	safe.FocusTaskID = publicIdentifier(snapshot.FocusTaskID)
	safe.Summary = publicIncidentSummary(snapshot.Trigger)
	safe.Details = publicIncidentDetails(snapshot.Trigger, snapshot.Details)

	safe.Nodes = make([]NodeSnapshot, 0, len(snapshot.Nodes))
	for _, node := range snapshot.Nodes {
		if !safePublicIdentifier(node.ID) ||
			!publicTaskStatus(string(node.Status)) ||
			!publicWorkflowRole(string(node.WorkflowRole)) ||
			!publicRuntime(string(node.Runtime)) {
			continue
		}
		copy := node
		copy.ID = publicIdentifier(node.ID)
		copy.Title = publicFreeText(
			node.Title,
			"Task title withheld at the Coordinator boundary",
		)
		copy.UpdatedAt = publicTimestamp(node.UpdatedAt)
		copy.Assignee = publicIdentifierPointer(node.Assignee)
		copy.CurrentRunID = publicIdentifierPointer(node.CurrentRunID)
		copy.BlockKind = publicBlockKindPointer(node.BlockKind)
		copy.BlockReason = publicBlockReason(copy.BlockKind, node.BlockReason)
		copy.BlockedBy = publicIdentifierList(node.BlockedBy)
		copy.Unlocks = publicIdentifierList(node.Unlocks)
		safe.Nodes = append(safe.Nodes, copy)
	}

	safe.Dependencies = make([]DependencySnapshot, 0, len(snapshot.Dependencies))
	for _, dependency := range snapshot.Dependencies {
		if !safePublicIdentifier(dependency.PrerequisiteID) ||
			!safePublicIdentifier(dependency.DependentID) {
			continue
		}
		safe.Dependencies = append(safe.Dependencies, dependency)
	}

	safe.RecoveryCheckpoints = make(
		[]RecoveryCheckpointSnapshot,
		0,
		len(snapshot.RecoveryCheckpoints),
	)
	for _, checkpoint := range snapshot.RecoveryCheckpoints {
		if !safePublicIdentifier(checkpoint.ID) ||
			!safePublicIdentifier(checkpoint.SourceRunID) ||
			!publicCheckpointState(string(checkpoint.State)) {
			continue
		}
		copy := checkpoint
		copy.ID = publicIdentifier(checkpoint.ID)
		copy.SourceRunID = publicIdentifier(checkpoint.SourceRunID)
		copy.BoundedChangedFiles = make([]string, 0, min(
			len(checkpoint.BoundedChangedFiles),
			coordinatorChangedFileLimit,
		))
		for _, changedPath := range checkpoint.BoundedChangedFiles {
			if len(copy.BoundedChangedFiles) >= coordinatorChangedFileLimit {
				break
			}
			if safePath, ok := SafeRelativeChangedPath(changedPath); ok {
				copy.BoundedChangedFiles = append(copy.BoundedChangedFiles, safePath)
			}
		}
		copy.CreatedAt = publicTimestamp(checkpoint.CreatedAt)
		copy.AdoptedAt = publicTimestampPointer(checkpoint.AdoptedAt)
		if checkpoint.SupersedeReason != nil {
			reason := "recovery_checkpoint_superseded"
			copy.SupersedeReason = &reason
		}
		safe.RecoveryCheckpoints = append(safe.RecoveryCheckpoints, copy)
	}

	safe.Diagnostics = make([]IssueSnapshot, 0, len(snapshot.Diagnostics))
	for _, issue := range snapshot.Diagnostics {
		kind := publicDiagnosticKind(issue.Kind)
		safe.Diagnostics = append(safe.Diagnostics, IssueSnapshot{
			Kind:   kind,
			TaskID: publicIdentifier(issue.TaskID),
			Detail: publicDiagnosticSummary(kind),
		})
	}

	safe.AvailableAgents = make([]AgentSnapshot, 0, len(snapshot.AvailableAgents))
	for _, agent := range snapshot.AvailableAgents {
		if !safePublicIdentifier(agent.ID) ||
			!publicRuntime(string(agent.Runtime)) ||
			!publicAgentHealth(agent.Health) {
			continue
		}
		copy := agent
		copy.ID = publicIdentifier(agent.ID)
		copy.Model = publicOpaqueLabel(agent.Model)
		copy.Provider = publicOpaqueLabel(agent.Provider)
		copy.Roles = publicRoles(agent.Roles)
		copy.CooldownUntil = publicTimestampPointer(agent.CooldownUntil)
		copy.Fallbacks = publicIdentifierList(agent.Fallbacks)
		safe.AvailableAgents = append(safe.AvailableAgents, copy)
	}
	return safe
}

// SafeRelativeChangedPath accepts only bounded, canonical repository-relative
// paths. It deliberately rejects path traversal, platform absolute paths,
// control characters, internal Autogora refs, and ambiguous normalization.
func SafeRelativeChangedPath(value string) (string, bool) {
	if value == "" || strings.TrimSpace(value) != value || !utf8.ValidString(value) ||
		utf8.RuneCountInString(value) > coordinatorChangedPathLimit ||
		strings.Contains(value, `\`) {
		return "", false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return "", false
		}
	}
	normalized := strings.ReplaceAll(value, `\`, "/")
	lower := strings.ToLower(normalized)
	if strings.HasPrefix(normalized, "/") ||
		(len(normalized) >= 3 &&
			((normalized[0] >= 'a' && normalized[0] <= 'z') ||
				(normalized[0] >= 'A' && normalized[0] <= 'Z')) &&
			normalized[1] == ':' && normalized[2] == '/') ||
		path.Clean(normalized) != normalized ||
		normalized == "." ||
		strings.HasPrefix(lower, "refs/autogora/") ||
		strings.HasPrefix(lower, ".git/") {
		return "", false
	}
	for _, component := range strings.Split(normalized, "/") {
		if component == ".." || component == "." || component == "" {
			return "", false
		}
	}
	return normalized, true
}

func publicIncidentSummary(trigger string) string {
	switch model.CoordinationTrigger(strings.TrimSpace(trigger)) {
	case model.CoordinationTriggerRepeatedBlock:
		return "A task repeatedly entered the same blocked state."
	case model.CoordinationTriggerRetryExhausted:
		return "A task exhausted its configured retry limit."
	case model.CoordinationTriggerGraphStalled:
		return "The workflow graph has unfinished work but no runnable task."
	case model.CoordinationTriggerIntegrationConflict:
		return "Task integration requires bounded conflict recovery."
	case model.CoordinationTriggerAgentExhausted:
		return "No enabled healthy worker agent matches the task route."
	case model.CoordinationTriggerRunInvariant:
		return "Task and run ownership require deterministic recovery."
	default:
		return "An exceptional workflow incident requires coordination."
	}
}

func publicIncidentDetails(trigger string, details map[string]any) map[string]any {
	result := map[string]any{
		"diagnosticCode": publicIncidentCode(trigger),
	}
	switch model.CoordinationTrigger(strings.TrimSpace(trigger)) {
	case model.CoordinationTriggerRepeatedBlock:
		copyPublicTaskState(result, details)
		copyPublicNumberFields(
			result,
			details,
			"blockRecurrences",
			"failureCount",
			"maxRetries",
		)
	case model.CoordinationTriggerRetryExhausted:
		copyPublicTaskState(result, details)
		copyPublicNumberFields(result, details, "failureCount", "maxRetries")
		if lastRun, ok := details["lastRun"].(map[string]any); ok {
			publicRun := map[string]any{}
			copyPublicIdentifierField(publicRun, lastRun, "id")
			copyPublicEnumField(publicRun, lastRun, "status", publicRunStatus)
			copyPublicNumberFields(publicRun, lastRun, "exitCode")
			copyPublicTimestampField(publicRun, lastRun, "endedAt")
			if len(publicRun) > 0 {
				result["lastRun"] = publicRun
			}
		}
	case model.CoordinationTriggerGraphStalled:
		copyPublicNumberFields(
			result,
			details,
			"unfinishedTasks",
			"actionableTasks",
			"intentionallyWaiting",
			"idleSeconds",
			"activeRuns",
		)
		copyPublicTimestampField(result, details, "lastTaskActivityAt")
		copyPublicTimestampField(result, details, "taskUpdatedAt")
		if values, ok := details["byStatus"].(map[string]any); ok {
			statuses := map[string]any{}
			for key, value := range values {
				if publicTaskStatus(key) {
					if number, valid := publicNumber(value); valid {
						statuses[key] = number
					}
				}
			}
			if len(statuses) > 0 {
				result["byStatus"] = statuses
			}
		}
	case model.CoordinationTriggerIntegrationConflict:
		copyPublicEnumField(result, details, "code", publicIntegrationCode)
		copyPublicEnumField(result, details, "blockKind", publicBlockKind)
	case model.CoordinationTriggerAgentExhausted:
		copyPublicTaskState(result, details)
		copyPublicEnumField(result, details, "runtime", publicRuntime)
		copyPublicIdentifierField(result, details, "assignee")
		if capacityIgnored, ok := details["capacityIgnored"].(bool); ok {
			result["capacityIgnored"] = capacityIgnored
		}
		if routes, ok := details["routes"].([]any); ok {
			publicRoutes := make([]any, 0, min(len(routes), 32))
			for _, route := range routes {
				value, ok := route.(map[string]any)
				if !ok || len(publicRoutes) >= 32 {
					continue
				}
				publicRoute := map[string]any{}
				copyPublicIdentifierField(publicRoute, value, "id")
				copyPublicEnumField(publicRoute, value, "runtime", publicRuntime)
				copyPublicEnumField(publicRoute, value, "health", publicAgentHealth)
				copyPublicTimestampField(publicRoute, value, "cooldownUntil")
				for _, key := range []string{"enabled", "workerRole"} {
					if flag, valid := value[key].(bool); valid {
						publicRoute[key] = flag
					}
				}
				if len(publicRoute) > 0 {
					publicRoutes = append(publicRoutes, publicRoute)
				}
			}
			if len(publicRoutes) > 0 {
				result["routes"] = publicRoutes
			}
		}
	case model.CoordinationTriggerRunInvariant:
		copyPublicTaskState(result, details)
		copyPublicIdentifierField(result, details, "currentRunId")
		copyPublicIdentifierField(result, details, "checkpointId")
		copyPublicIdentifierField(result, details, "sourceRunId")
		copyPublicEnumField(result, details, "runStatus", publicRunStatus)
		copyPublicEnumField(result, details, "checkpointState", publicCheckpointState)
		copyPublicEnumField(result, details, "reason", publicRunInvariantReason)
		copyPublicNumberFields(result, details, "boundedRechecks", "fenceGeneration")
		if reason, _ := result["reason"].(string); reason == "operator_recovery_required" {
			copyPublicEnumField(
				result,
				details,
				"diagnosticCode",
				publicOperatorRecoveryDiagnosticCode,
			)
		}
	}
	return result
}

func copyPublicTaskState(target, source map[string]any) {
	copyPublicEnumField(target, source, "taskStatus", publicTaskStatus)
	copyPublicEnumField(target, source, "blockKind", publicBlockKind)
	copyPublicTimestampField(target, source, "taskUpdatedAt")
}

func copyPublicEnumField(
	target map[string]any,
	source map[string]any,
	key string,
	allowed func(string) bool,
) {
	value, ok := source[key].(string)
	value = strings.TrimSpace(value)
	if ok && allowed(value) {
		target[key] = value
	}
}

func copyPublicIdentifierField(target, source map[string]any, key string) {
	value, ok := source[key].(string)
	if ok && safePublicIdentifier(value) {
		target[key] = value
	}
}

func copyPublicTimestampField(target, source map[string]any, key string) {
	value, ok := source[key].(string)
	if ok {
		if timestamp := publicTimestamp(value); timestamp != "" {
			target[key] = timestamp
		}
	}
}

func copyPublicNumberFields(target, source map[string]any, keys ...string) {
	for _, key := range keys {
		if value, ok := publicNumber(source[key]); ok {
			target[key] = value
		}
	}
}

func publicNumber(value any) (any, bool) {
	switch typed := value.(type) {
	case int:
		return typed, true
	case int8:
		return typed, true
	case int16:
		return typed, true
	case int32:
		return typed, true
	case int64:
		return typed, true
	case uint:
		return typed, true
	case uint8:
		return typed, true
	case uint16:
		return typed, true
	case uint32:
		return typed, true
	case uint64:
		return typed, true
	case float32:
		return typed, true
	case float64:
		return typed, true
	case json.Number:
		return typed, true
	default:
		return nil, false
	}
}

func publicIncidentCode(trigger string) string {
	if publicCoordinationTrigger(trigger) {
		return strings.TrimSpace(trigger)
	}
	return "workflow_incident"
}

func publicCoordinationTrigger(value string) bool {
	for _, trigger := range model.CoordinationTriggers {
		if string(trigger) == value {
			return true
		}
	}
	return false
}

func publicSeverity(value string) string {
	for _, severity := range model.CoordinationSeverities {
		if string(severity) == value {
			return value
		}
	}
	return string(model.CoordinationSeverityError)
}

func publicTaskStatus(value string) bool {
	for _, status := range model.TaskStatuses {
		if string(status) == value {
			return true
		}
	}
	return false
}

func publicBlockKind(value string) bool {
	switch model.BlockKind(value) {
	case model.BlockKindDependency, model.BlockKindNeedsInput,
		model.BlockKindCapability, model.BlockKindTransient:
		return true
	default:
		return false
	}
}

func publicWorkflowRole(value string) bool {
	for _, role := range model.WorkflowRoles {
		if string(role) == value {
			return true
		}
	}
	return false
}

func publicRuntime(value string) bool {
	for _, runtime := range model.Runtimes {
		if string(runtime) == value {
			return true
		}
	}
	return false
}

func publicRunStatus(value string) bool {
	switch model.RunStatus(value) {
	case model.RunStatusRunning, model.RunStatusCompleted, model.RunStatusBlocked,
		model.RunStatusFailed, model.RunStatusReclaimed, model.RunStatusCrashed,
		model.RunStatusTimedOut, model.RunStatusRateLimited,
		model.RunStatusSpawnFailed, model.RunStatusProtocolViolation:
		return true
	default:
		return false
	}
}

func publicAgentHealth(value string) bool {
	switch model.AgentHealthStatus(value) {
	case model.AgentHealthUnknown, model.AgentHealthReady, model.AgentHealthMissing,
		model.AgentHealthAuthRequired, model.AgentHealthRateLimited,
		model.AgentHealthUnhealthy:
		return true
	default:
		return false
	}
}

func publicCheckpointState(value string) bool {
	switch model.RecoveryCheckpointState(value) {
	case model.RecoveryCheckpointPending, model.RecoveryCheckpointReserved,
		model.RecoveryCheckpointAdopted, model.RecoveryCheckpointSuperseded:
		return true
	default:
		return false
	}
}

func publicIntegrationCode(value string) bool {
	switch value {
	case "conflict", "history_rewrite", "resolution_exhausted":
		return true
	default:
		return false
	}
}

func publicRunInvariantReason(value string) bool {
	switch value {
	case "current_run_on_non_running_task",
		"running_task_without_current_run",
		"referenced_run_missing",
		"referenced_run_belongs_to_another_task",
		"referenced_run_terminal",
		"running_owner_missing_from_active_runs",
		"operator_recovery_required",
		"recovery_checkpoint_adoption_exhausted":
		return true
	default:
		return false
	}
}

func publicOperatorRecoveryDiagnosticCode(value string) bool {
	switch value {
	case "unverifiable_process_ownership",
		"process_teardown_unconfirmed",
		"process_teardown_proof_unavailable":
		return true
	default:
		return false
	}
}

func publicBlockReason(kind *model.BlockKind, reason *string) *string {
	if kind == nil && reason == nil {
		return nil
	}
	value := "task_blocked"
	if kind != nil {
		switch *kind {
		case model.BlockKindDependency:
			value = "dependency_block"
		case model.BlockKindNeedsInput:
			value = "needs_input_block"
		case model.BlockKindCapability:
			value = "capability_block"
		case model.BlockKindTransient:
			value = "transient_block"
		}
	}
	return &value
}

func publicDiagnosticKind(value string) string {
	switch strings.TrimSpace(value) {
	case "workspace_inspection_failed",
		"snapshot_truncated",
		"run_invariant",
		"external_review_required",
		"overdue_schedule",
		"stale_heartbeat",
		"agent_unavailable",
		"stranded_in_ready",
		"promotion_lag",
		"done_with_open_dependency",
		"running_with_open_dependency",
		"stalled_prerequisite",
		"terminal_prerequisite":
		return strings.TrimSpace(value)
	default:
		return "workflow_diagnostic"
	}
}

func publicDiagnosticSummary(kind string) string {
	switch kind {
	case "workspace_inspection_failed":
		return "Workspace state could not be inspected."
	case "snapshot_truncated":
		return "The Coordinator snapshot was truncated to its configured bounds."
	case "run_invariant":
		return "Task and run ownership are inconsistent."
	case "external_review_required":
		return "An imported task is waiting for explicit review."
	case "overdue_schedule":
		return "A scheduled task has passed its start time."
	case "stale_heartbeat":
		return "An active run has not renewed its heartbeat."
	case "agent_unavailable":
		return "The assigned agent is unavailable."
	case "stranded_in_ready":
		return "A ready task has no runnable agent route."
	case "promotion_lag":
		return "A task is eligible for promotion but remains queued."
	case "done_with_open_dependency", "running_with_open_dependency":
		return "A running or completed task still has an open dependency."
	case "stalled_prerequisite", "terminal_prerequisite":
		return "An unfinished task has a prerequisite that cannot currently advance."
	default:
		return "A workflow consistency diagnostic was reported."
	}
}

func publicRoles(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		switch value {
		case "planner", "worker", "reviewer", "finalizer", "coordinator", "judge":
			result = append(result, value)
		}
	}
	return result
}

func publicIdentifierList(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if safePublicIdentifier(value) {
			result = append(result, value)
		}
	}
	return result
}

func publicIdentifierPointer(value *string) *string {
	if value == nil || !safePublicIdentifier(*value) {
		return nil
	}
	copy := *value
	return &copy
}

func publicBlockKindPointer(value *model.BlockKind) *model.BlockKind {
	if value == nil || !publicBlockKind(string(*value)) {
		return nil
	}
	copy := *value
	return &copy
}

func publicIdentifier(value string) string {
	if !safePublicIdentifier(value) {
		return ""
	}
	return value
}

func safePublicIdentifier(value string) bool {
	value = strings.TrimSpace(value)
	return coordinatorPublicIdentifier.MatchString(value) &&
		!containsSensitiveCoordinatorText(value)
}

func publicOpaqueLabel(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || utf8.RuneCountInString(value) > 256 ||
		containsSensitiveCoordinatorText(value) {
		return ""
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return ""
		}
	}
	return value
}

func publicFreeText(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if containsSensitiveCoordinatorText(value) {
		return fallback
	}
	return value
}

func containsSensitiveCoordinatorText(value string) bool {
	lower := strings.ToLower(strings.ReplaceAll(value, `\`, "/"))
	if strings.Contains(lower, "refs/autogora/") ||
		strings.Contains(lower, "claimtoken") ||
		strings.Contains(lower, "claim_token") ||
		strings.Contains(lower, "claim-token") ||
		strings.Contains(lower, "claim token") ||
		strings.Contains(lower, "reservationtoken") ||
		strings.Contains(lower, "reservation_token") ||
		strings.Contains(lower, "reservation-token") ||
		strings.Contains(lower, "reservation token") {
		return true
	}
	for index := 0; index < len(lower); index++ {
		if lower[index] != '/' {
			continue
		}
		if index == 0 {
			return true
		}
		previous := lower[index-1]
		if !((previous >= 'a' && previous <= 'z') ||
			(previous >= '0' && previous <= '9') ||
			previous == '_' || previous == '.') {
			// This catches file:///..., path:/..., drive roots after slash
			// normalization, and absolute paths following punctuation while
			// retaining ordinary relative labels such as frontend/backend.
			return true
		}
	}
	fields := strings.FieldsFunc(lower, func(character rune) bool {
		switch character {
		case ' ', '\t', '\r', '\n', '"', '\'', '`', '(', ')', '[', ']', '{', '}',
			',', ';', '=':
			return true
		default:
			return false
		}
	})
	for _, field := range fields {
		field = strings.Trim(field, ".:")
		if strings.HasPrefix(field, "/") && len(field) > 1 {
			return true
		}
	}
	return false
}

func publicTimestamp(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if _, err := time.Parse(time.RFC3339Nano, value); err != nil {
		return ""
	}
	return value
}

func publicTimestampPointer(value *string) *string {
	if value == nil {
		return nil
	}
	timestamp := publicTimestamp(*value)
	if timestamp == "" {
		return nil
	}
	return &timestamp
}

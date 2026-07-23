package dispatcher

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nn1a/autogora/internal/agentconfig"
	"github.com/nn1a/autogora/internal/boards"
	"github.com/nn1a/autogora/internal/coordinator"
	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/orchestration"
	"github.com/nn1a/autogora/internal/store"
	"github.com/nn1a/autogora/internal/workspace"
)

const (
	coordinatorSnapshotNodeLimit       = 200
	coordinatorSnapshotDependencyLimit = 800
	coordinatorSnapshotAgentLimit      = 100
	coordinatorSnapshotDiagnosticLimit = 100
	coordinatorIncidentDetailsLimit    = 16 * 1024
	coordinatorDiagnosticDetailLimit   = 1024
	coordinatorTaskTitleLimit          = 512
	coordinatorObservationTaskLimit    = 5000
	coordinatorConditionFingerprintKey = "conditionFingerprint"
)

type coordinatorCondition struct {
	trigger       model.CoordinationTrigger
	severity      model.CoordinationSeverity
	taskID        string
	rootTaskID    string
	graphRevision int64
	summary       string
	details       map[string]any
}

func (c coordinatorCondition) key() string {
	return coordinatorIncidentKey(c.trigger, c.rootTaskID, c.taskID)
}

func coordinatorIncidentKey(trigger model.CoordinationTrigger, rootTaskID, taskID string) string {
	return string(trigger) + "\x00" + strings.TrimSpace(rootTaskID) + "\x00" + strings.TrimSpace(taskID)
}

func activeCoordinatorIncident(status model.CoordinationIncidentStatus) bool {
	switch status {
	case model.CoordinationIncidentOpen, model.CoordinationIncidentCoordinating,
		model.CoordinationIncidentAwaitingApproval, model.CoordinationIncidentApplying:
		return true
	default:
		return false
	}
}

func coordinatorManagedTrigger(trigger model.CoordinationTrigger) bool {
	switch trigger {
	case model.CoordinationTriggerRepeatedBlock, model.CoordinationTriggerRetryExhausted,
		model.CoordinationTriggerGraphStalled, model.CoordinationTriggerAgentExhausted:
		return true
	default:
		return false
	}
}

func stableCoordinatorConditionDetails(details map[string]any) map[string]any {
	stable := make(map[string]any, len(details))
	for key, value := range details {
		switch key {
		case coordinatorConditionFingerprintKey, "observedAt":
			// observedAt records when the same condition was sampled. Including
			// it would make every graph-stall observation look like new work.
			continue
		default:
			stable[key] = value
		}
	}
	return stable
}

func coordinatorConditionFingerprint(condition coordinatorCondition) (string, error) {
	return coordinatorIncidentFingerprint(
		condition.trigger,
		condition.rootTaskID,
		condition.taskID,
		condition.graphRevision,
		condition.details,
	)
}

func coordinatorIncidentFingerprint(
	trigger model.CoordinationTrigger,
	rootTaskID string,
	taskID string,
	graphRevision int64,
	details map[string]any,
) (string, error) {
	payload := struct {
		Trigger       model.CoordinationTrigger `json:"trigger"`
		RootTaskID    string                    `json:"rootTaskId,omitempty"`
		TaskID        string                    `json:"taskId,omitempty"`
		GraphRevision int64                     `json:"graphRevision"`
		Details       map[string]any            `json:"details"`
	}{
		Trigger: trigger, RootTaskID: strings.TrimSpace(rootTaskID),
		TaskID: strings.TrimSpace(taskID), GraphRevision: graphRevision,
		Details: stableCoordinatorConditionDetails(details),
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	fingerprint := sha256.Sum256(encoded)
	return fmt.Sprintf("%x", fingerprint), nil
}

func storedCoordinatorConditionFingerprint(incident model.CoordinationIncident) (string, error) {
	details, err := decodeBoundedCoordinatorDetails(incident.Details)
	if err != nil {
		return "", err
	}
	if fingerprint, ok := details[coordinatorConditionFingerprintKey].(string); ok &&
		strings.TrimSpace(fingerprint) != "" {
		return strings.TrimSpace(fingerprint), nil
	}
	return coordinatorIncidentFingerprint(
		incident.Trigger,
		optionalString(incident.RootTaskID),
		optionalString(incident.TaskID),
		incident.GraphRevision,
		details,
	)
}

type coordinatorIntegrationIncidentDetails struct {
	Code      string          `json:"code"`
	BlockKind model.BlockKind `json:"blockKind"`
	Reason    string          `json:"reason"`
}

func coordinatorIntegrationReasonMatches(actual, expected string) bool {
	actual, expected = strings.TrimSpace(actual), strings.TrimSpace(expected)
	if actual == expected {
		return actual != ""
	}
	const truncationSuffix = "…"
	return strings.HasSuffix(expected, truncationSuffix) &&
		strings.HasPrefix(actual, strings.TrimSuffix(expected, truncationSuffix))
}

// integrationCoordinatorIncidentState separates a still-observed integration
// failure from one that is safe to send to Coordinator. A conflict is retained
// while its worker is still finalizing, but only the matching blocked/triage
// state is actionable.
func integrationCoordinatorIncidentState(
	ctx context.Context,
	opened *store.Store,
	incident model.CoordinationIncident,
) (keepOpen bool, actionable bool, err error) {
	taskID := optionalString(incident.TaskID)
	if taskID == "" {
		return false, false, nil
	}
	detail, err := opened.GetTask(ctx, taskID)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "task not found") {
			// Incidents intentionally outlive deleted tasks for audit. With no
			// focus task left, the integration condition cannot remain active.
			return false, false, nil
		}
		return false, false, err
	}
	switch detail.Task.Status {
	case model.TaskStatusRunning:
		return true, false, nil
	case model.TaskStatusDone, model.TaskStatusArchived:
		return false, false, nil
	case model.TaskStatusBlocked, model.TaskStatusTriage:
		var expected coordinatorIntegrationIncidentDetails
		if err := json.Unmarshal(incident.Details, &expected); err != nil {
			return false, false, fmt.Errorf(
				"decode integration coordination incident %s: %w", incident.ID, err,
			)
		}
		relevantCode := expected.Code == workspace.IntegrationFailureConflict ||
			expected.Code == workspace.IntegrationFailureHistoryRewrite
		matchingKind := detail.Task.BlockKind != nil &&
			*detail.Task.BlockKind == expected.BlockKind
		matchingReason := detail.Task.BlockReason != nil &&
			coordinatorIntegrationReasonMatches(*detail.Task.BlockReason, expected.Reason)
		matching := relevantCode && matchingKind && matchingReason
		return matching, matching, nil
	default:
		return false, false, nil
	}
}

func reconcileOpenIntegrationIncidents(
	ctx context.Context,
	opened *store.Store,
	incidents []model.CoordinationIncident,
) ([]model.CoordinationIncident, error) {
	result := make([]model.CoordinationIncident, 0, len(incidents))
	for _, incident := range incidents {
		if incident.Status != model.CoordinationIncidentOpen ||
			incident.Trigger != model.CoordinationTriggerIntegrationConflict {
			result = append(result, incident)
			continue
		}
		keepOpen, _, err := integrationCoordinatorIncidentState(ctx, opened, incident)
		if err != nil {
			return nil, err
		}
		if keepOpen {
			result = append(result, incident)
			continue
		}
		resolved, err := opened.TransitionCoordinationIncident(
			ctx,
			incident.ID,
			store.TransitionCoordinationIncidentInput{
				ExpectedStatus: model.CoordinationIncidentOpen,
				Status:         model.CoordinationIncidentResolved,
			},
		)
		if err != nil {
			return nil, err
		}
		result = append(result, resolved)
	}
	return result, nil
}

// reconcileCoordinatorIncidents runs the deterministic observation half of
// coordination. It never invokes a coding agent and does not change task or
// graph state. A future coordination queue can consume the open incidents
// independently of normal worker claims.
func reconcileCoordinatorIncidents(
	ctx context.Context,
	manager *boards.Manager,
	opened *store.Store,
	metadata boards.Metadata,
	options Options,
	current time.Time,
) ([]model.CoordinationIncident, error) {
	if manager == nil || opened == nil {
		return nil, errors.New("coordinator observer requires a board manager and store")
	}
	if current.IsZero() {
		current = time.Now()
	}
	current = current.UTC()
	configured, err := configuredProfiles(manager, metadata.Slug, options)
	if err != nil {
		return nil, err
	}
	active, err := opened.ListCoordinationIncidents(ctx, store.CoordinationIncidentFilter{
		Board: metadata.Slug, Limit: 500,
	})
	if err != nil {
		return nil, err
	}
	active, err = reconcileOpenIntegrationIncidents(ctx, opened, active)
	if err != nil {
		return nil, err
	}
	activeByKey := make(map[string]model.CoordinationIncident, len(active))
	dismissedByKey := make(map[string]model.CoordinationIncident)
	latestByKey := make(map[string]bool)
	for _, incident := range active {
		key := coordinatorIncidentKey(
			incident.Trigger, optionalString(incident.RootTaskID), optionalString(incident.TaskID),
		)
		if !latestByKey[key] {
			latestByKey[key] = true
			if incident.Status == model.CoordinationIncidentDismissed {
				// ListCoordinationIncidents is newest first. An older dismissal
				// must not suppress a condition that changed after that decision.
				dismissedByKey[key] = incident
			}
		}
		if !activeCoordinatorIncident(incident.Status) {
			continue
		}
		activeByKey[key] = incident
	}

	conditions, err := detectCoordinatorConditions(ctx, opened, metadata, configured, options, active, current)
	if err != nil {
		return nil, err
	}
	currentKeys := make(map[string]bool, len(conditions))
	observed := make([]model.CoordinationIncident, 0, len(conditions))
	for _, condition := range conditions {
		currentKeys[condition.key()] = true
		if existing, found := activeByKey[condition.key()]; found &&
			existing.Status != model.CoordinationIncidentOpen {
			observed = append(observed, existing)
			continue
		}
		fingerprint, err := coordinatorConditionFingerprint(condition)
		if err != nil {
			return nil, err
		}
		if dismissed, found := dismissedByKey[condition.key()]; found {
			dismissedFingerprint, err := storedCoordinatorConditionFingerprint(dismissed)
			if err != nil {
				return nil, err
			}
			if dismissedFingerprint == fingerprint {
				continue
			}
		}
		detailsWithFingerprint := make(map[string]any, len(condition.details)+1)
		for key, value := range condition.details {
			detailsWithFingerprint[key] = value
		}
		detailsWithFingerprint[coordinatorConditionFingerprintKey] = fingerprint
		details, err := boundedCoordinatorDetails(detailsWithFingerprint)
		if err != nil {
			return nil, err
		}
		rootTaskID, taskID := optionalPointer(condition.rootTaskID), optionalPointer(condition.taskID)
		revision := condition.graphRevision
		incident, _, err := opened.CreateCoordinationIncident(ctx, store.CreateCoordinationIncidentInput{
			Board: metadata.Slug, RootTaskID: rootTaskID, TaskID: taskID,
			Trigger: condition.trigger, Severity: condition.severity,
			ExpectedGraphRevision: &revision, Summary: condition.summary, Details: details,
		})
		if err != nil {
			return nil, err
		}
		observed = append(observed, incident)
	}

	// Only an untouched open incident is safe to auto-resolve. A claimed,
	// approval-pending, or applying incident belongs to its current actor.
	for _, incident := range active {
		if incident.Status != model.CoordinationIncidentOpen ||
			!coordinatorManagedTrigger(incident.Trigger) {
			continue
		}
		key := coordinatorIncidentKey(
			incident.Trigger, optionalString(incident.RootTaskID), optionalString(incident.TaskID),
		)
		if currentKeys[key] {
			continue
		}
		if _, err := opened.TransitionCoordinationIncident(ctx, incident.ID, store.TransitionCoordinationIncidentInput{
			ExpectedStatus: model.CoordinationIncidentOpen,
			Status:         model.CoordinationIncidentResolved,
		}); err != nil {
			return nil, err
		}
	}
	return observed, nil
}

func detectCoordinatorConditions(
	ctx context.Context,
	opened *store.Store,
	metadata boards.Metadata,
	configured configuredProfileSet,
	options Options,
	active []model.CoordinationIncident,
	current time.Time,
) ([]coordinatorCondition, error) {
	tasks, err := listCoordinatorObservationTasks(ctx, opened, metadata.Slug)
	if err != nil {
		return nil, err
	}
	activeRuns, err := opened.ListActiveRuns(ctx, metadata.Slug)
	if err != nil {
		return nil, err
	}
	graphs := map[string]model.RelationshipGraph{}
	graphFor := func(taskID string) (model.RelationshipGraph, error) {
		if graph, found := graphs[taskID]; found {
			return graph, nil
		}
		graph, err := opened.RelationshipGraph(ctx, taskID)
		if err == nil {
			graphs[taskID] = graph
		}
		return graph, err
	}
	conditionForTask := func(
		task model.Task,
		trigger model.CoordinationTrigger,
		severity model.CoordinationSeverity,
		summary string,
		details map[string]any,
	) (coordinatorCondition, error) {
		graph, err := graphFor(task.ID)
		if err != nil {
			return coordinatorCondition{}, err
		}
		return coordinatorCondition{
			trigger: trigger, severity: severity, taskID: task.ID,
			rootTaskID: graph.RootTaskID, graphRevision: graph.GraphRevision,
			summary: summary, details: details,
		}, nil
	}

	conditions := make([]coordinatorCondition, 0)
	var actionable []model.Task
	unfinishedCount, intentionallyWaiting := 0, 0
	latestActivity := time.Time{}
	for _, task := range tasks {
		if task.Status == model.TaskStatusDone || task.Status == model.TaskStatusArchived {
			continue
		}
		unfinishedCount++
		if coordinatorTaskIntentionallyWaiting(task, current) {
			intentionallyWaiting++
		} else {
			actionable = append(actionable, task)
			if updated := parseTimestamp(task.UpdatedAt); updated.After(latestActivity) {
				latestActivity = updated
			}
		}

		if (task.Status == model.TaskStatusBlocked || task.Status == model.TaskStatusTriage) &&
			task.BlockRecurrences >= 2 {
			condition, err := conditionForTask(
				task, model.CoordinationTriggerRepeatedBlock, model.CoordinationSeverityWarning,
				"Task entered the same blocked state repeatedly",
				map[string]any{
					"taskStatus":       task.Status,
					"blockKind":        task.BlockKind,
					"blockReason":      boundedCoordinatorText(optionalString(task.BlockReason), 2048),
					"blockRecurrences": task.BlockRecurrences,
					"failureCount":     task.FailureCount,
					"maxRetries":       task.MaxRetries,
					"taskUpdatedAt":    task.UpdatedAt,
				},
			)
			if err != nil {
				return nil, err
			}
			conditions = append(conditions, condition)
		}

		if task.Status == model.TaskStatusBlocked && task.MaxRetries > 0 &&
			task.FailureCount >= task.MaxRetries {
			detail, err := opened.GetTask(ctx, task.ID)
			if err != nil {
				return nil, err
			}
			retryDetails := map[string]any{
				"taskStatus":    task.Status,
				"failureCount":  task.FailureCount,
				"maxRetries":    task.MaxRetries,
				"blockKind":     task.BlockKind,
				"blockReason":   boundedCoordinatorText(optionalString(task.BlockReason), 2048),
				"taskUpdatedAt": task.UpdatedAt,
			}
			if len(detail.Runs) > 0 {
				last := detail.Runs[len(detail.Runs)-1]
				retryDetails["lastRun"] = map[string]any{
					"id": last.ID, "status": last.Status, "exitCode": last.ExitCode,
					"error":   boundedCoordinatorText(optionalString(last.Error), 2048),
					"summary": boundedCoordinatorText(optionalString(last.Summary), 2048),
					"endedAt": last.EndedAt,
				}
			}
			condition, err := conditionForTask(
				task, model.CoordinationTriggerRetryExhausted, model.CoordinationSeverityError,
				"Task exhausted its configured retry limit", retryDetails,
			)
			if err != nil {
				return nil, err
			}
			conditions = append(conditions, condition)
		}

		if coordinatorTaskNeedsAgent(task) {
			availability, err := coordinatorWorkerAvailability(ctx, opened, metadata, configured, options, task, current)
			if err != nil {
				return nil, err
			}
			if !availability.healthy {
				condition, err := conditionForTask(
					task, model.CoordinationTriggerAgentExhausted, model.CoordinationSeverityError,
					"No enabled healthy worker agent is available for the task route",
					map[string]any{
						"taskStatus": task.Status, "assignee": task.Assignee,
						"runtime": task.Runtime, "routes": availability.routes,
						"capacityIgnored": true,
					},
				)
				if err != nil {
					return nil, err
				}
				conditions = append(conditions, condition)
			}
		}
	}

	specific := len(conditions) > 0
	if !specific {
		for _, incident := range active {
			if !activeCoordinatorIncident(incident.Status) ||
				incident.Trigger == model.CoordinationTriggerGraphStalled {
				continue
			}
			// Integration conflicts are emitted by the integration path. Other
			// non-open incidents remain owned by their current coordination
			// attempt and suppress a competing board-level diagnosis.
			if incident.Trigger == model.CoordinationTriggerIntegrationConflict ||
				incident.Status != model.CoordinationIncidentOpen {
				specific = true
				break
			}
		}
	}
	if len(actionable) == 0 || len(activeRuns) > 0 || specific {
		return conditions, nil
	}
	idle := time.Duration(metadata.Orchestration.Autopilot.Coordination.IdleSeconds) * time.Second
	if idle <= 0 || latestActivity.IsZero() || current.Before(latestActivity.Add(idle)) {
		return conditions, nil
	}
	for _, task := range actionable {
		if task.Status != model.TaskStatusReady || !coordinatorTaskNeedsAgent(task) {
			continue
		}
		availability, err := coordinatorWorkerAvailability(ctx, opened, metadata, configured, options, task, current)
		if err != nil {
			return nil, err
		}
		// Capacity is deliberately excluded: a healthy saturated agent is
		// normal backpressure, not an exceptional coordination incident.
		if availability.healthy {
			return conditions, nil
		}
	}

	focus := actionable[0]
	for _, task := range actionable[1:] {
		if task.Priority > focus.Priority ||
			(task.Priority == focus.Priority && task.CreatedAt < focus.CreatedAt) ||
			(task.Priority == focus.Priority && task.CreatedAt == focus.CreatedAt && task.ID < focus.ID) {
			focus = task
		}
	}
	byStatus := map[string]int{}
	for _, task := range actionable {
		byStatus[string(task.Status)]++
	}
	condition, err := conditionForTask(
		focus, model.CoordinationTriggerGraphStalled, model.CoordinationSeverityWarning,
		"Workflow graph has unfinished work but no active or runnable task",
		map[string]any{
			"unfinishedTasks": unfinishedCount, "actionableTasks": len(actionable),
			"intentionallyWaiting": intentionallyWaiting, "byStatus": byStatus,
			"idleSeconds": int(idle.Seconds()), "lastTaskActivityAt": latestActivity.Format(time.RFC3339Nano),
			"observedAt": current.Format(time.RFC3339Nano), "activeRuns": len(activeRuns),
		},
	)
	if err != nil {
		return nil, err
	}
	return append(conditions, condition), nil
}

func listCoordinatorObservationTasks(ctx context.Context, opened *store.Store, board string) ([]model.Task, error) {
	tasks := make([]model.Task, 0)
	var cursor *store.TaskListCursor
	for len(tasks) < coordinatorObservationTaskLimit {
		page, err := opened.ListTasks(ctx, store.ListTaskFilter{
			Board: board, IncludeArchived: false, Sort: "priority-desc",
			Limit: 500, After: cursor,
		})
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, page...)
		if len(page) < 500 {
			break
		}
		last := page[len(page)-1]
		cursor = &store.TaskListCursor{Priority: last.Priority, CreatedAt: last.CreatedAt, ID: last.ID}
	}
	return tasks, nil
}

func coordinatorTaskNeedsAgent(task model.Task) bool {
	if task.WorkflowRole == model.WorkflowRoleControl || !validWorkerRuntime(task.Runtime) {
		return false
	}
	if task.Status == model.TaskStatusReady {
		return true
	}
	return task.Status == model.TaskStatusBlocked && task.BlockKind != nil &&
		*task.BlockKind == model.BlockKindCapability
}

func coordinatorTaskIntentionallyWaiting(task model.Task, current time.Time) bool {
	if task.Status == model.TaskStatusScheduled && task.ScheduledAt != nil {
		scheduledAt, err := time.Parse(time.RFC3339Nano, *task.ScheduledAt)
		if err == nil && scheduledAt.After(current) {
			return true
		}
	}
	if task.Status == model.TaskStatusTriage && isGitHubImportedTask(task) {
		return true
	}
	return task.BlockKind != nil && *task.BlockKind == model.BlockKindNeedsInput
}

type coordinatorRouteObservation struct {
	ID            string                  `json:"id"`
	Runtime       model.Runtime           `json:"runtime,omitempty"`
	Enabled       bool                    `json:"enabled"`
	WorkerRole    bool                    `json:"workerRole"`
	Health        model.AgentHealthStatus `json:"health"`
	CooldownUntil *string                 `json:"cooldownUntil,omitempty"`
}

type coordinatorAvailability struct {
	healthy bool
	routes  []coordinatorRouteObservation
}

func coordinatorWorkerAvailability(
	ctx context.Context,
	opened *store.Store,
	metadata boards.Metadata,
	configured configuredProfileSet,
	options Options,
	task model.Task,
	current time.Time,
) (coordinatorAvailability, error) {
	starts := coordinatorTaskRouteStarts(metadata, configured, options, task)
	byName := make(map[string]orchestration.ProfileRoute, len(configured.Profiles))
	for _, profile := range configured.Profiles {
		byName[profile.Name] = profile
	}
	queue := append([]string{}, starts...)
	seen := map[string]bool{}
	result := coordinatorAvailability{routes: make([]coordinatorRouteObservation, 0)}
	for len(queue) > 0 && len(result.routes) < 32 {
		name := strings.TrimSpace(queue[0])
		queue = queue[1:]
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		profile, configuredRoute := byName[name]
		workerRole := true
		if agent, found := configured.Config.Find(name); found {
			workerRole = hasAgentRole(agent, agentconfig.RoleWorker)
		}
		if !configuredRoute {
			if !workerRole {
				result.routes = append(result.routes, coordinatorRouteObservation{
					ID: name, Enabled: false, WorkerRole: false,
				})
				continue
			}
			profile = orchestration.ProfileRoute{Name: name, Runtime: task.Runtime}
		}
		queue = append(queue, profile.Fallbacks...)
		enabled := workerRole && orchestration.RunnableProfileRoute(profile)
		health, err := opened.GetAgentHealth(ctx, name)
		if err != nil {
			return coordinatorAvailability{}, err
		}
		result.routes = append(result.routes, coordinatorRouteObservation{
			ID: name, Runtime: profile.Runtime, Enabled: enabled, WorkerRole: workerRole,
			Health: health.Status, CooldownUntil: health.CooldownUntil,
		})
		if enabled && !store.IsAgentUnavailable(health, current) {
			result.healthy = true
		}
	}
	return result, nil
}

func coordinatorTaskRouteStarts(
	metadata boards.Metadata,
	configured configuredProfileSet,
	options Options,
	task model.Task,
) []string {
	if task.Assignee != nil && strings.TrimSpace(*task.Assignee) != "" {
		return []string{strings.TrimSpace(*task.Assignee)}
	}
	if options.DefaultProfile != nil && strings.TrimSpace(options.DefaultProfile.Name) != "" {
		return []string{strings.TrimSpace(options.DefaultProfile.Name)}
	}
	if metadata.Orchestration.DefaultProfile != nil &&
		strings.TrimSpace(*metadata.Orchestration.DefaultProfile) != "" {
		return []string{strings.TrimSpace(*metadata.Orchestration.DefaultProfile)}
	}
	if len(configured.DefaultWorkers) > 0 {
		return append([]string{}, configured.DefaultWorkers...)
	}
	return []string{string(task.Runtime) + "-worker"}
}

// buildCoordinatorIncidentSnapshot captures the bounded, read-only view given
// to a Coordinator. It reads the full administrator graph, then deliberately
// narrows it before any paid analysis can run.
func buildCoordinatorIncidentSnapshot(
	ctx context.Context,
	manager *boards.Manager,
	opened *store.Store,
	metadata boards.Metadata,
	options Options,
	incident model.CoordinationIncident,
) (coordinator.IncidentSnapshot, error) {
	if manager == nil || opened == nil {
		return coordinator.IncidentSnapshot{}, errors.New("coordinator snapshot requires a board manager and store")
	}
	focusTaskID := optionalString(incident.TaskID)
	if focusTaskID == "" {
		focusTaskID = optionalString(incident.RootTaskID)
	}
	if focusTaskID == "" {
		return coordinator.IncidentSnapshot{}, errors.New("coordinator incident has no focus or root task")
	}
	graph, err := opened.RelationshipGraph(ctx, focusTaskID)
	if err != nil {
		return coordinator.IncidentSnapshot{}, err
	}
	if graph.GraphRevision != incident.GraphRevision {
		return coordinator.IncidentSnapshot{}, &store.GraphRevisionConflictError{
			Board: metadata.Slug, Expected: incident.GraphRevision, Actual: graph.GraphRevision,
		}
	}
	nodes, dependencies, graphTruncated := boundCoordinatorGraph(graph)
	selected := make(map[string]bool, len(nodes))
	for _, node := range nodes {
		selected[node.Task.ID] = true
	}
	snapshot := coordinator.IncidentSnapshot{
		IncidentID: incident.ID, Trigger: string(incident.Trigger), Severity: string(incident.Severity),
		Summary: boundedCoordinatorText(incident.Summary, 2048), GraphRevision: graph.GraphRevision,
		FocusTaskID: focusTaskID, Nodes: make([]coordinator.NodeSnapshot, 0, len(nodes)),
		Dependencies: dependencies, Diagnostics: []coordinator.IssueSnapshot{},
	}
	snapshot.Details, err = decodeBoundedCoordinatorDetails(incident.Details)
	if err != nil {
		return coordinator.IncidentSnapshot{}, err
	}
	workspaces := workspace.New(manager)
	for _, graphNode := range nodes {
		detail, err := opened.GetTask(ctx, graphNode.Task.ID)
		if err != nil {
			return coordinator.IncidentSnapshot{}, err
		}
		preserved, dirty, issues := coordinatorWorkspaceState(ctx, workspaces, detail)
		snapshot.Diagnostics = appendBoundedCoordinatorIssues(snapshot.Diagnostics, issues...)
		blockedBy := selectedCoordinatorIDs(graphNode.BlockedBy, selected)
		unlocks := selectedCoordinatorIDs(graphNode.Unlocks, selected)
		snapshot.Nodes = append(snapshot.Nodes, coordinator.NodeSnapshot{
			ID: detail.Task.ID, Title: boundedCoordinatorText(detail.Task.Title, coordinatorTaskTitleLimit),
			Status: detail.Task.Status, WorkflowRole: detail.Task.WorkflowRole,
			Assignee: detail.Task.Assignee, Runtime: detail.Task.Runtime, Priority: detail.Task.Priority,
			UpdatedAt: detail.Task.UpdatedAt, CurrentRunID: detail.Task.CurrentRunID,
			PreservedWork: preserved, WorkspaceDirty: dirty,
			BlockKind:        detail.Task.BlockKind,
			BlockReason:      boundedCoordinatorStringPointer(detail.Task.BlockReason, 2048),
			BlockRecurrences: detail.Task.BlockRecurrences, FailureCount: detail.Task.FailureCount,
			BlockedBy: blockedBy, Unlocks: unlocks,
		})
	}
	if graphTruncated {
		snapshot.Diagnostics = appendBoundedCoordinatorIssues(snapshot.Diagnostics, coordinator.IssueSnapshot{
			Kind: "snapshot_truncated",
			Detail: fmt.Sprintf(
				"graph was bounded to %d nodes and %d dependencies (connected nodes=%d, sourceTruncated=%t)",
				len(nodes), len(dependencies), graph.TotalConnectedNodes, graph.Truncated,
			),
		})
	}
	diagnostics, err := opened.Diagnose(ctx, metadata.Slug)
	if err != nil {
		return coordinator.IncidentSnapshot{}, err
	}
	for _, issue := range diagnostics.Issues {
		if issue.TaskID != "" && !selected[issue.TaskID] {
			continue
		}
		snapshot.Diagnostics = appendBoundedCoordinatorIssues(snapshot.Diagnostics, coordinator.IssueSnapshot{
			Kind: issue.Kind, TaskID: issue.TaskID,
			Detail: boundedCoordinatorText(issue.Detail, coordinatorDiagnosticDetailLimit),
		})
	}
	snapshot.AvailableAgents, err = buildCoordinatorAgentSnapshots(
		ctx, manager, opened, metadata, options,
	)
	if err != nil {
		return coordinator.IncidentSnapshot{}, err
	}
	return snapshot, nil
}

func boundCoordinatorGraph(graph model.RelationshipGraph) ([]model.RelationshipNode, []coordinator.DependencySnapshot, bool) {
	selected := map[string]bool{}
	selectID := func(id string) {
		if id != "" && len(selected) < coordinatorSnapshotNodeLimit {
			selected[id] = true
		}
	}
	selectID(graph.FocusTaskID)
	selectID(graph.RootTaskID)
	for _, node := range graph.Nodes {
		selectID(node.Task.ID)
	}
	nodes := make([]model.RelationshipNode, 0, min(len(graph.Nodes), coordinatorSnapshotNodeLimit))
	for _, node := range graph.Nodes {
		if selected[node.Task.ID] {
			nodes = append(nodes, node)
		}
	}
	dependencies := make([]coordinator.DependencySnapshot, 0, min(len(graph.Dependencies), coordinatorSnapshotDependencyLimit))
	for _, edge := range graph.Dependencies {
		if len(dependencies) >= coordinatorSnapshotDependencyLimit {
			break
		}
		if !selected[edge.PrerequisiteID] || !selected[edge.DependentID] {
			continue
		}
		dependencies = append(dependencies, coordinator.DependencySnapshot{
			PrerequisiteID: edge.PrerequisiteID, DependentID: edge.DependentID,
			Satisfied: edge.SatisfiedAt != nil,
		})
	}
	truncated := graph.Truncated || len(nodes) < len(graph.Nodes) ||
		len(dependencies) < countSelectedDependencies(graph.Dependencies, selected)
	return nodes, dependencies, truncated
}

func countSelectedDependencies(edges []model.DependencyEdge, selected map[string]bool) int {
	count := 0
	for _, edge := range edges {
		if selected[edge.PrerequisiteID] && selected[edge.DependentID] {
			count++
		}
	}
	return count
}

func buildCoordinatorAgentSnapshots(
	ctx context.Context,
	manager *boards.Manager,
	opened *store.Store,
	metadata boards.Metadata,
	options Options,
) ([]coordinator.AgentSnapshot, error) {
	configured, err := configuredProfiles(manager, metadata.Slug, options)
	if err != nil {
		return nil, err
	}
	coordinationStore, err := manager.OpenCoordinationStore(ctx)
	if err != nil {
		return nil, err
	}
	defer coordinationStore.Close()
	result := make([]coordinator.AgentSnapshot, 0, min(len(configured.Profiles), coordinatorSnapshotAgentLimit))
	for _, profile := range configured.Profiles {
		if len(result) >= coordinatorSnapshotAgentLimit {
			break
		}
		health, err := opened.GetAgentHealth(ctx, profile.Name)
		if err != nil {
			return nil, err
		}
		roles := []string{"worker"}
		globalAgent, globallyConfigured := configured.Config.Find(profile.Name)
		if globallyConfigured {
			roles = make([]string, 0, len(globalAgent.Roles))
			for _, role := range globalAgent.Roles {
				roles = append(roles, string(role))
			}
		}
		activeSlots := 0
		if globallyConfigured && hasAgentRole(globalAgent, agentconfig.RoleWorker) {
			slots, err := coordinationStore.ListGlobalAgentSlots(ctx, profile.Name)
			if err != nil {
				return nil, err
			}
			activeSlots = len(slots)
		} else {
			activeSlots, err = opened.CountActiveAgentRuns(ctx, profile.Name)
			if err != nil {
				return nil, err
			}
		}
		result = append(result, coordinator.AgentSnapshot{
			ID: profile.Name, Runtime: profile.Runtime, Model: profile.Model, Provider: profile.Provider,
			Enabled: orchestration.RunnableProfileRoute(profile) && containsCoordinatorString(roles, "worker"),
			Roles:   roles, Health: string(health.Status), MaxConcurrent: profile.MaxConcurrent,
			ActiveSlots: activeSlots, CooldownUntil: health.CooldownUntil,
			Fallbacks: append([]string{}, profile.Fallbacks...),
		})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}

func coordinatorWorkspaceState(
	ctx context.Context,
	workspaces *workspace.Manager,
	detail model.TaskDetail,
) (bool, bool, []coordinator.IssueSnapshot) {
	preserved := len(detail.ChangeSets) > 0 &&
		detail.Task.Status != model.TaskStatusDone && detail.Task.Status != model.TaskStatusArchived
	dirty := false
	issues := []coordinator.IssueSnapshot{}
	for _, event := range detail.Events {
		var payload map[string]any
		if json.Unmarshal(event.Payload, &payload) == nil {
			if value, ok := payload["preservedWorkspace"].(bool); ok && value {
				preserved = true
			}
		}
	}
	reason := strings.ToLower(optionalString(detail.Task.BlockReason))
	if strings.Contains(reason, "partial changes remain") || strings.Contains(reason, "preserved workspace") {
		preserved = true
	}
	if detail.Task.Status != model.TaskStatusBlocked && detail.Task.Status != model.TaskStatusTriage &&
		detail.Task.Status != model.TaskStatusRunning {
		return preserved, dirty, issues
	}
	start := max(0, len(detail.RunWorkspaces)-3)
	for _, runWorkspace := range detail.RunWorkspaces[start:] {
		if runWorkspace.Kind == model.WorkspaceDir {
			preserved = true
			continue
		}
		inspection, err := workspaces.InspectChanges(ctx, runWorkspace)
		if err != nil {
			preserved = true
			issues = append(issues, coordinator.IssueSnapshot{
				Kind: "workspace_inspection_failed", TaskID: detail.Task.ID,
				Detail: boundedCoordinatorText(err.Error(), coordinatorDiagnosticDetailLimit),
			})
			continue
		}
		if inspection.Changed {
			dirty = true
			preserved = true
		}
	}
	return preserved, dirty, issues
}

func boundedCoordinatorDetails(details map[string]any) (json.RawMessage, error) {
	encoded, err := json.Marshal(details)
	if err != nil {
		return nil, err
	}
	if len(encoded) <= coordinatorIncidentDetailsLimit {
		return encoded, nil
	}
	fallback := map[string]any{
		"truncated": true, "originalBytes": len(encoded),
	}
	if fingerprint, ok := details[coordinatorConditionFingerprintKey].(string); ok &&
		strings.TrimSpace(fingerprint) != "" {
		fallback[coordinatorConditionFingerprintKey] = strings.TrimSpace(fingerprint)
	}
	return json.Marshal(fallback)
}

func decodeBoundedCoordinatorDetails(raw json.RawMessage) (map[string]any, error) {
	if len(raw) == 0 {
		return map[string]any{}, nil
	}
	var details map[string]any
	if err := json.Unmarshal(raw, &details); err != nil {
		return nil, fmt.Errorf("decode coordinator incident details: %w", err)
	}
	encoded, err := json.Marshal(details)
	if err != nil {
		return nil, err
	}
	if len(encoded) <= coordinatorIncidentDetailsLimit {
		return details, nil
	}
	return map[string]any{"truncated": true, "originalBytes": len(encoded)}, nil
}

func appendBoundedCoordinatorIssues(
	current []coordinator.IssueSnapshot,
	values ...coordinator.IssueSnapshot,
) []coordinator.IssueSnapshot {
	for _, value := range values {
		if len(current) >= coordinatorSnapshotDiagnosticLimit {
			break
		}
		value.Detail = boundedCoordinatorText(value.Detail, coordinatorDiagnosticDetailLimit)
		current = append(current, value)
	}
	return current
}

func selectedCoordinatorIDs(values []string, selected map[string]bool) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if selected[value] {
			result = append(result, value)
		}
	}
	return result
}

func boundedCoordinatorText(value string, maxRunes int) string {
	if maxRunes < 1 || value == "" {
		return ""
	}
	if utf8.RuneCountInString(value) <= maxRunes {
		return value
	}
	runes := []rune(value)
	return string(runes[:maxRunes]) + "…"
}

func boundedCoordinatorStringPointer(value *string, maxRunes int) *string {
	if value == nil {
		return nil
	}
	bounded := boundedCoordinatorText(*value, maxRunes)
	return &bounded
}

func optionalString(value *string) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(*value)
}

func optionalPointer(value string) *string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return &value
}

func containsCoordinatorString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

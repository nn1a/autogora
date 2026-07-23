package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/store"
)

const (
	integrationIncidentPersistenceTimeout = 5 * time.Second
	integrationIncidentDetailsLimit       = 16 * 1024
	integrationIncidentSummaryLimit       = 512
	integrationIncidentReasonLimit        = 2048
	integrationIncidentWorkspacePathLimit = 1024
	integrationIncidentIdentifierLimit    = 256
	integrationIncidentDurableRefLimit    = 1024
	integrationIncidentConflictFileLimit  = 512
	integrationIncidentConflictFilesLimit = 20
	integrationIncidentCompactReasonLimit = 512
	integrationIncidentCompactPathLimit   = 256
	integrationIncidentCompactIDLimit     = 128
	integrationIncidentCompactRefLimit    = 256
	integrationIncidentCodeLimit          = 64
	integrationIncidentBlockKindLimit     = 64
)

type integrationIncidentDetails struct {
	Code                         string          `json:"code"`
	BlockKind                    model.BlockKind `json:"blockKind"`
	Reason                       string          `json:"reason"`
	WorkspacePath                string          `json:"workspacePath,omitempty"`
	PrerequisiteID               string          `json:"prerequisiteId,omitempty"`
	ChangeSetID                  string          `json:"changeSetId,omitempty"`
	DurableRef                   string          `json:"durableRef,omitempty"`
	ConflictingFiles             []string        `json:"conflictingFiles,omitempty"`
	ConflictingFilesUniqueCount  int             `json:"conflictingFilesUniqueCount"`
	ConflictingFilesOmittedCount int             `json:"conflictingFilesOmittedCount"`
}

func exceptionalIntegrationFailure(err error) (*PrerequisiteIntegrationError, bool) {
	var integrationErr *PrerequisiteIntegrationError
	if !errors.As(err, &integrationErr) || integrationErr == nil {
		return nil, false
	}
	switch integrationErr.Code {
	case IntegrationFailureConflict, IntegrationFailureHistoryRewrite, IntegrationFailureResolutionExhausted:
		return integrationErr, true
	default:
		return nil, false
	}
}

// persistExceptionalIntegrationIncident is deliberately best effort. Workspace
// callers must continue their normal blocking or preserved-workspace lifecycle
// with the original integration error even when incident persistence fails.
func persistExceptionalIntegrationIncident(opened *store.Store, taskID string, detected error) {
	integrationErr, exceptional := exceptionalIntegrationFailure(detected)
	taskID = strings.TrimSpace(taskID)
	if !exceptional || opened == nil || taskID == "" {
		return
	}

	details, err := boundedIntegrationIncidentDetails(integrationErr)
	if err != nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), integrationIncidentPersistenceTimeout)
	defer cancel()
	task, err := opened.GetTask(ctx, taskID)
	if err != nil {
		return
	}
	taskPointer := task.Task.ID
	summary := "Prerequisite integration conflict requires coordination"
	if integrationErr.Code == IntegrationFailureHistoryRewrite {
		summary = "Worker history rewrite requires coordination"
	} else if integrationErr.Code == IntegrationFailureResolutionExhausted {
		summary = "Finalizer integration resolution exhausted"
	}
	if reason := strings.TrimSpace(integrationErr.Reason); reason != "" {
		summary += ": " + reason
	}
	summary = boundedUTF8Bytes(summary, integrationIncidentSummaryLimit)

	// RelationshipGraph supplies a root and a revision from the same stable
	// topology snapshot. Retry once when the topology changes between that read
	// and CreateCoordinationIncident's compare-and-set check.
	for range 2 {
		input := store.CreateCoordinationIncidentInput{
			Board:    task.Task.Board,
			TaskID:   &taskPointer,
			Trigger:  model.CoordinationTriggerIntegrationConflict,
			Severity: model.CoordinationSeverityError,
			Summary:  summary,
			Details:  details,
		}
		graph, graphErr := opened.RelationshipGraph(ctx, taskID)
		if graphErr == nil {
			rootTaskID, revision := graph.RootTaskID, graph.GraphRevision
			input.RootTaskID = &rootTaskID
			input.ExpectedGraphRevision = &revision
		}
		if _, _, createErr := opened.CreateCoordinationIncident(ctx, input); createErr == nil {
			return
		} else if graphErr != nil || !errors.Is(createErr, store.ErrGraphRevisionConflict) {
			return
		}
	}
}

func boundedIntegrationIncidentDetails(integrationErr *PrerequisiteIntegrationError) (json.RawMessage, error) {
	conflictingFiles, uniqueCount := boundedConflictingFiles(integrationErr.ConflictingFiles)
	details := integrationIncidentDetails{
		Code:                         boundedUTF8Bytes(strings.TrimSpace(integrationErr.Code), integrationIncidentCodeLimit),
		BlockKind:                    model.BlockKind(boundedUTF8Bytes(strings.TrimSpace(string(integrationErr.BlockKind)), integrationIncidentBlockKindLimit)),
		Reason:                       boundedUTF8Bytes(strings.TrimSpace(integrationErr.Reason), integrationIncidentReasonLimit),
		WorkspacePath:                boundedUTF8Bytes(integrationErr.WorkspacePath, integrationIncidentWorkspacePathLimit),
		PrerequisiteID:               boundedUTF8Bytes(strings.TrimSpace(integrationErr.PrerequisiteID), integrationIncidentIdentifierLimit),
		ChangeSetID:                  boundedUTF8Bytes(strings.TrimSpace(integrationErr.ChangeSetID), integrationIncidentIdentifierLimit),
		DurableRef:                   boundedUTF8Bytes(strings.TrimSpace(integrationErr.DurableRef), integrationIncidentDurableRefLimit),
		ConflictingFiles:             conflictingFiles,
		ConflictingFilesUniqueCount:  uniqueCount,
		ConflictingFilesOmittedCount: uniqueCount - len(conflictingFiles),
	}
	encoded, err := json.Marshal(details)
	if err != nil {
		return nil, err
	}
	if len(encoded) <= integrationIncidentDetailsLimit {
		return encoded, nil
	}

	// JSON escaping can expand control characters beyond their UTF-8 length.
	// Compact scalar fields before dropping file evidence, then remove paths
	// from the tail until the persisted object fits the Coordinator snapshot.
	details.Reason = boundedUTF8Bytes(details.Reason, integrationIncidentCompactReasonLimit)
	details.WorkspacePath = boundedUTF8Bytes(details.WorkspacePath, integrationIncidentCompactPathLimit)
	details.PrerequisiteID = boundedUTF8Bytes(details.PrerequisiteID, integrationIncidentCompactIDLimit)
	details.ChangeSetID = boundedUTF8Bytes(details.ChangeSetID, integrationIncidentCompactIDLimit)
	details.DurableRef = boundedUTF8Bytes(details.DurableRef, integrationIncidentCompactRefLimit)
	for {
		details.ConflictingFilesOmittedCount = uniqueCount - len(details.ConflictingFiles)
		encoded, err = json.Marshal(details)
		if err != nil {
			return nil, err
		}
		if len(encoded) <= integrationIncidentDetailsLimit {
			return encoded, nil
		}
		if len(details.ConflictingFiles) == 0 {
			break
		}
		details.ConflictingFiles = details.ConflictingFiles[:len(details.ConflictingFiles)-1]
	}

	// The compact scalar limits have a worst-case escaped size below 16 KiB.
	// Keep this defensive fallback so future fields cannot silently violate the
	// snapshot contract.
	details.Reason = ""
	details.WorkspacePath = ""
	details.PrerequisiteID = ""
	details.ChangeSetID = ""
	details.DurableRef = ""
	encoded, err = json.Marshal(details)
	if err != nil {
		return nil, err
	}
	if len(encoded) > integrationIncidentDetailsLimit {
		return nil, fmt.Errorf("bounded integration incident details exceed %d bytes", integrationIncidentDetailsLimit)
	}
	return encoded, nil
}

func boundedConflictingFiles(values []string) ([]string, int) {
	if len(values) == 0 {
		return nil, 0
	}
	unique := make([]string, 0, len(values))
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		value = strings.ToValidUTF8(value, "\uFFFD")
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		unique = append(unique, value)
	}
	sort.Strings(unique)
	result := make([]string, 0, min(len(unique), integrationIncidentConflictFilesLimit))
	boundedSeen := make(map[string]bool, len(result))
	for _, value := range unique {
		bounded := boundedUTF8Bytes(value, integrationIncidentConflictFileLimit)
		if boundedSeen[bounded] {
			continue
		}
		boundedSeen[bounded] = true
		result = append(result, bounded)
		if len(result) == integrationIncidentConflictFilesLimit {
			break
		}
	}
	sort.Strings(result)
	return result, len(unique)
}

func boundedUTF8Bytes(value string, limit int) string {
	if value == "" || limit < 1 {
		return ""
	}
	value = strings.ToValidUTF8(value, "\uFFFD")
	if len(value) <= limit {
		return value
	}
	const suffix = "…"
	if limit <= len(suffix) {
		for limit > 0 && !utf8.ValidString(value[:limit]) {
			limit--
		}
		return value[:limit]
	}
	end := limit - len(suffix)
	for end > 0 && !utf8.ValidString(value[:end]) {
		end--
	}
	return value[:end] + suffix
}

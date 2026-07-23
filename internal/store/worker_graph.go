package store

import (
	"context"

	"github.com/nn1a/autogora/internal/model"
)

// WorkerRelationshipGraph returns the minimum useful topology for one worker:
// its task, the hierarchy root, direct dependency neighbors, and immediate
// hierarchy neighbors. Coordinator, dashboard, and administrator callers keep
// using RelationshipGraph for the wider connected component.
func (s *Store) WorkerRelationshipGraph(ctx context.Context, taskID string) (model.RelationshipGraph, error) {
	full, err := s.RelationshipGraph(ctx, taskID)
	if err != nil {
		return model.RelationshipGraph{}, err
	}
	selected := map[string]bool{
		full.FocusTaskID: true,
		full.RootTaskID:  true,
	}
	for _, edge := range full.Dependencies {
		if edge.PrerequisiteID == full.FocusTaskID || edge.DependentID == full.FocusTaskID {
			selected[edge.PrerequisiteID] = true
			selected[edge.DependentID] = true
		}
	}
	for _, edge := range full.Hierarchy {
		if edge.ParentTaskID == full.FocusTaskID || edge.SubtaskID == full.FocusTaskID {
			selected[edge.ParentTaskID] = true
			selected[edge.SubtaskID] = true
		}
	}

	local := model.RelationshipGraph{
		FocusTaskID: full.FocusTaskID, RootTaskID: full.RootTaskID,
		GraphRevision: full.GraphRevision, TotalPhases: full.TotalPhases,
		TotalConnectedNodes: full.TotalConnectedNodes,
		Nodes:               []model.RelationshipNode{},
		Hierarchy:           []model.HierarchyEdge{},
		Dependencies:        []model.DependencyEdge{},
	}
	for _, node := range full.Nodes {
		if !selected[node.Task.ID] {
			continue
		}
		node.SubtaskIDs = selectedStrings(node.SubtaskIDs, selected)
		node.BlockedBy = selectedStrings(node.BlockedBy, selected)
		node.Unlocks = selectedStrings(node.Unlocks, selected)
		if node.ParentTaskID != nil && !selected[*node.ParentTaskID] {
			node.ParentTaskID = nil
			node.Position = 0
		}
		local.Nodes = append(local.Nodes, node)
	}
	for _, edge := range full.Hierarchy {
		if selected[edge.ParentTaskID] && selected[edge.SubtaskID] {
			local.Hierarchy = append(local.Hierarchy, edge)
		}
	}
	for _, edge := range full.Dependencies {
		if selected[edge.PrerequisiteID] && selected[edge.DependentID] {
			local.Dependencies = append(local.Dependencies, edge)
		}
	}
	local.OmittedNodeCount = max(0, local.TotalConnectedNodes-len(local.Nodes))
	local.Truncated = full.Truncated || local.OmittedNodeCount > 0
	return local, nil
}

func selectedStrings(values []string, selected map[string]bool) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if selected[value] {
			result = append(result, value)
		}
	}
	return result
}

package store

import (
	"context"
	"database/sql"
	"math"
	"sort"

	"github.com/nn1a/autogora/internal/model"
)

const BoardRelationshipGraphNodeLimit = 500

// BoardRelationshipGraph returns a bounded topology snapshot for the Store's
// board. The graph revision, visible task window, hierarchy, and dependencies
// are read in one SQLite transaction so callers never receive a revision from
// one topology state with edges from another.
func (s *Store) BoardRelationshipGraph(ctx context.Context, includeArchived bool) (model.BoardRelationshipGraph, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return model.BoardRelationshipGraph{}, err
	}
	defer tx.Rollback()

	graph, err := readBoardRelationshipGraph(ctx, tx, s.board, includeArchived)
	if err != nil {
		return model.BoardRelationshipGraph{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.BoardRelationshipGraph{}, err
	}
	return graph, nil
}

func readBoardRelationshipGraph(
	ctx context.Context,
	q querier,
	board string,
	includeArchived bool,
) (model.BoardRelationshipGraph, error) {
	state, err := readBoardGraphState(ctx, q, board)
	if err != nil {
		return model.BoardRelationshipGraph{}, err
	}

	visibility := ""
	if !includeArchived {
		visibility = " AND status <> 'archived'"
	}
	var total int
	if err := q.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM tasks WHERE board = ?"+visibility,
		board,
	).Scan(&total); err != nil {
		return model.BoardRelationshipGraph{}, err
	}

	// Active tasks precede archived tasks when the caller explicitly includes
	// both, so retired work cannot crowd current work out of the bounded view.
	tasks, err := queryTasks(ctx, q, `
		SELECT `+taskColumns+`
		FROM tasks
		WHERE board = ?`+visibility+`
		ORDER BY CASE WHEN status = 'archived' THEN 1 ELSE 0 END, created_at, id
		LIMIT ?
	`, board, BoardRelationshipGraphNodeLimit)
	if err != nil {
		return model.BoardRelationshipGraph{}, err
	}

	hierarchy, err := querySelectedBoardHierarchy(ctx, q, board, includeArchived)
	if err != nil {
		return model.BoardRelationshipGraph{}, err
	}
	dependencies, err := querySelectedBoardDependencies(ctx, q, board, includeArchived)
	if err != nil {
		return model.BoardRelationshipGraph{}, err
	}

	nodes, totalPhases := boardRelationshipNodes(tasks, hierarchy, dependencies)
	omitted := max(0, total-len(nodes))
	return model.BoardRelationshipGraph{
		Board:            board,
		IncludeArchived:  includeArchived,
		GraphRevision:    state.Revision,
		TotalPhases:      totalPhases,
		TotalNodes:       total,
		ReturnedNodes:    len(nodes),
		NodeLimit:        BoardRelationshipGraphNodeLimit,
		Truncated:        omitted > 0,
		OmittedNodeCount: omitted,
		Nodes:            nodes,
		Hierarchy:        boardHierarchyEdges(hierarchy),
		Dependencies:     boardDependencyEdges(dependencies),
	}, nil
}

func selectedBoardTasksCTE(includeArchived bool) string {
	visibility := ""
	if !includeArchived {
		visibility = " AND status <> 'archived'"
	}
	return `
		WITH selected_tasks AS (
			SELECT id
			FROM tasks
			WHERE board = ?` + visibility + `
			ORDER BY CASE WHEN status = 'archived' THEN 1 ELSE 0 END, created_at, id
			LIMIT ?
		)
	`
}

func querySelectedBoardHierarchy(
	ctx context.Context,
	q querier,
	board string,
	includeArchived bool,
) ([]hierarchyRow, error) {
	rows, err := q.QueryContext(ctx, selectedBoardTasksCTE(includeArchived)+`
		SELECT h.parent_id, h.child_id, h.position
		FROM task_hierarchy h
		JOIN selected_tasks parent ON parent.id = h.parent_id
		JOIN selected_tasks child ON child.id = h.child_id
		ORDER BY h.parent_id, h.position, h.child_id
	`, board, BoardRelationshipGraphNodeLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := []hierarchyRow{}
	for rows.Next() {
		var edge hierarchyRow
		if err := rows.Scan(&edge.parentID, &edge.childID, &edge.position); err != nil {
			return nil, err
		}
		result = append(result, edge)
	}
	return result, rows.Err()
}

func querySelectedBoardDependencies(
	ctx context.Context,
	q querier,
	board string,
	includeArchived bool,
) ([]dependencyRow, error) {
	rows, err := q.QueryContext(ctx, selectedBoardTasksCTE(includeArchived)+`
		SELECT l.parent_id, l.child_id, l.satisfied_at, l.satisfied_run_id
		FROM task_links l
		JOIN selected_tasks prerequisite ON prerequisite.id = l.parent_id
		JOIN selected_tasks dependent ON dependent.id = l.child_id
		ORDER BY l.parent_id, l.child_id
	`, board, BoardRelationshipGraphNodeLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := []dependencyRow{}
	for rows.Next() {
		var edge dependencyRow
		var satisfied, satisfiedRunID sql.NullString
		if err := rows.Scan(&edge.parentID, &edge.childID, &satisfied, &satisfiedRunID); err != nil {
			return nil, err
		}
		edge.satisfiedAt = stringPointer(satisfied)
		edge.satisfiedRunID = stringPointer(satisfiedRunID)
		result = append(result, edge)
	}
	return result, rows.Err()
}

func boardRelationshipNodes(
	tasks []model.Task,
	hierarchy []hierarchyRow,
	dependencies []dependencyRow,
) ([]model.RelationshipNode, int) {
	parentByChild := map[string]hierarchyRow{}
	subtasksByParent := map[string][]hierarchyRow{}
	for _, edge := range hierarchy {
		parentByChild[edge.childID] = edge
		subtasksByParent[edge.parentID] = append(subtasksByParent[edge.parentID], edge)
	}

	indegree := make(map[string]int, len(tasks))
	downstream := map[string][]string{}
	openUpstream := map[string][]string{}
	phases := make(map[string]int, len(tasks))
	for _, task := range tasks {
		indegree[task.ID] = 0
		phases[task.ID] = 0
	}
	for _, edge := range dependencies {
		indegree[edge.childID]++
		downstream[edge.parentID] = append(downstream[edge.parentID], edge.childID)
		if edge.satisfiedAt == nil {
			openUpstream[edge.childID] = append(openUpstream[edge.childID], edge.parentID)
		}
	}

	topological := make([]string, 0, len(tasks))
	for _, task := range tasks {
		if indegree[task.ID] == 0 {
			topological = append(topological, task.ID)
		}
	}
	sort.Strings(topological)
	processed := 0
	for index := 0; index < len(topological); index++ {
		current := topological[index]
		processed++
		for _, dependent := range downstream[current] {
			phases[dependent] = max(phases[dependent], phases[current]+1)
			indegree[dependent]--
			if indegree[dependent] == 0 {
				topological = append(topological, dependent)
			}
		}
	}
	if processed != len(tasks) {
		for id, remaining := range indegree {
			if remaining > 0 {
				phases[id] = -1
			}
		}
	}

	hierarchyDepth := func(id string) *int {
		_, hasParent := parentByChild[id]
		_, hasChildren := subtasksByParent[id]
		if !hasParent && !hasChildren {
			return nil
		}
		depth, current, seen := 0, id, map[string]bool{}
		for {
			edge, ok := parentByChild[current]
			if !ok || seen[current] {
				break
			}
			seen[current] = true
			current, depth = edge.parentID, depth+1
		}
		return &depth
	}

	nodes := make([]model.RelationshipNode, 0, len(tasks))
	validMax := -1
	for _, task := range tasks {
		edge, hasParent := parentByChild[task.ID]
		var parentID *string
		position := 0
		if hasParent {
			value := edge.parentID
			parentID, position = &value, edge.position
		}
		subtaskIDs := make([]string, 0, len(subtasksByParent[task.ID]))
		for _, child := range subtasksByParent[task.ID] {
			subtaskIDs = append(subtaskIDs, child.childID)
		}
		blockedBy := append([]string{}, openUpstream[task.ID]...)
		unlocks := append([]string{}, downstream[task.ID]...)
		phase := phases[task.ID]
		if phase >= 0 {
			validMax = max(validMax, phase)
		}
		nodes = append(nodes, model.RelationshipNode{
			Task: model.RelationshipTask{
				ID: task.ID, Board: task.Board, Tenant: task.Tenant, Title: task.Title,
				Assignee: task.Assignee, Runtime: task.Runtime, Status: task.Status,
				WorkflowRole: task.WorkflowRole, Priority: task.Priority,
				CreatedAt: task.CreatedAt, UpdatedAt: task.UpdatedAt,
			},
			ParentTaskID: parentID, SubtaskIDs: subtaskIDs,
			HierarchyDepth: hierarchyDepth(task.ID), Position: position,
			Phase: phase, BlockedBy: blockedBy, Unlocks: unlocks,
		})
	}

	sort.Slice(nodes, func(i, j int) bool {
		left, right := nodes[i], nodes[j]
		leftPhase, rightPhase := left.Phase, right.Phase
		if leftPhase < 0 {
			leftPhase = math.MaxInt
		}
		if rightPhase < 0 {
			rightPhase = math.MaxInt
		}
		if leftPhase != rightPhase {
			return leftPhase < rightPhase
		}
		leftDepth, rightDepth := math.MaxInt, math.MaxInt
		if left.HierarchyDepth != nil {
			leftDepth = *left.HierarchyDepth
		}
		if right.HierarchyDepth != nil {
			rightDepth = *right.HierarchyDepth
		}
		if leftDepth != rightDepth {
			return leftDepth < rightDepth
		}
		if left.Position != right.Position {
			return left.Position < right.Position
		}
		if left.Task.CreatedAt != right.Task.CreatedAt {
			return left.Task.CreatedAt < right.Task.CreatedAt
		}
		return left.Task.ID < right.Task.ID
	})

	totalPhases := 0
	if validMax >= 0 {
		totalPhases = validMax + 1
	}
	return nodes, totalPhases
}

func boardHierarchyEdges(values []hierarchyRow) []model.HierarchyEdge {
	result := make([]model.HierarchyEdge, 0, len(values))
	for _, edge := range values {
		result = append(result, model.HierarchyEdge{
			ParentTaskID: edge.parentID,
			SubtaskID:    edge.childID,
			Position:     edge.position,
		})
	}
	return result
}

func boardDependencyEdges(values []dependencyRow) []model.DependencyEdge {
	result := make([]model.DependencyEdge, 0, len(values))
	for _, edge := range values {
		result = append(result, model.DependencyEdge{
			PrerequisiteID: edge.parentID,
			DependentID:    edge.childID,
			SatisfiedAt:    edge.satisfiedAt,
			SatisfiedRunID: edge.satisfiedRunID,
		})
	}
	return result
}

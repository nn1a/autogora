package store

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"

	"github.com/nn1a/autogora/internal/model"
)

type hierarchyRow struct {
	parentID string
	childID  string
	position int
}

type dependencyRow struct {
	parentID    string
	childID     string
	satisfiedAt *string
}

func (s *Store) RelationshipGraph(ctx context.Context, taskID string) (model.RelationshipGraph, error) {
	focus, err := requireTask(ctx, s.db, taskID)
	if err != nil {
		return model.RelationshipGraph{}, err
	}
	tasks, err := queryTasks(ctx, s.db, "SELECT "+taskColumns+" FROM tasks WHERE board = ? ORDER BY created_at, id", focus.Board)
	if err != nil {
		return model.RelationshipGraph{}, err
	}
	tasksByID := make(map[string]model.Task, len(tasks))
	for _, task := range tasks {
		tasksByID[task.ID] = task
	}
	hierarchy, err := s.queryHierarchy(ctx, focus.Board)
	if err != nil {
		return model.RelationshipGraph{}, err
	}
	dependencies, err := s.queryDependencies(ctx, focus.Board)
	if err != nil {
		return model.RelationshipGraph{}, err
	}

	adjacency := map[string]map[string]bool{}
	connect := func(left, right string) {
		if adjacency[left] == nil {
			adjacency[left] = map[string]bool{}
		}
		if adjacency[right] == nil {
			adjacency[right] = map[string]bool{}
		}
		adjacency[left][right], adjacency[right][left] = true, true
	}
	for _, edge := range hierarchy {
		connect(edge.parentID, edge.childID)
	}
	for _, edge := range dependencies {
		connect(edge.parentID, edge.childID)
	}
	connected := map[string]bool{taskID: true}
	queue := []string{taskID}
	for index := 0; index < len(queue); index++ {
		current := queue[index]
		neighbors := keys(adjacency[current])
		sort.Strings(neighbors)
		for _, related := range neighbors {
			if connected[related] {
				continue
			}
			connected[related] = true
			queue = append(queue, related)
		}
	}
	hierarchy = filterHierarchy(hierarchy, connected)
	dependencies = filterDependencies(dependencies, connected)
	parentByChild := map[string]hierarchyRow{}
	subtasksByParent := map[string][]hierarchyRow{}
	for _, edge := range hierarchy {
		parentByChild[edge.childID] = edge
		subtasksByParent[edge.parentID] = append(subtasksByParent[edge.parentID], edge)
	}
	rootID := taskID
	ancestorGuard := map[string]bool{}
	for {
		edge, ok := parentByChild[rootID]
		if !ok || ancestorGuard[rootID] {
			break
		}
		ancestorGuard[rootID] = true
		rootID = edge.parentID
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

	indegree, downstream, openUpstream := map[string]int{}, map[string][]string{}, map[string][]string{}
	for id := range connected {
		indegree[id] = 0
	}
	for _, edge := range dependencies {
		indegree[edge.childID]++
		downstream[edge.parentID] = append(downstream[edge.parentID], edge.childID)
		if edge.satisfiedAt == nil {
			openUpstream[edge.childID] = append(openUpstream[edge.childID], edge.parentID)
		}
	}
	phases := map[string]int{}
	topological := []string{}
	for id := range connected {
		phases[id] = 0
		if indegree[id] == 0 {
			topological = append(topological, id)
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
	if processed != len(connected) {
		for id, remaining := range indegree {
			if remaining > 0 {
				phases[id] = -1
			}
		}
	}

	nodes := make([]model.RelationshipNode, 0, len(connected))
	for id := range connected {
		task, ok := tasksByID[id]
		if !ok {
			return model.RelationshipGraph{}, fmt.Errorf("related task not found: %s", id)
		}
		edge, hasParent := parentByChild[id]
		var parentID *string
		position := 0
		if hasParent {
			value := edge.parentID
			parentID, position = &value, edge.position
		}
		subtaskIDs := make([]string, 0, len(subtasksByParent[id]))
		for _, child := range subtasksByParent[id] {
			subtaskIDs = append(subtaskIDs, child.childID)
		}
		blockedBy := append([]string{}, openUpstream[id]...)
		unlocks := append([]string{}, downstream[id]...)
		nodes = append(nodes, model.RelationshipNode{
			Task: model.RelationshipTask{ID: task.ID, Board: task.Board, Tenant: task.Tenant, Title: task.Title,
				Assignee: task.Assignee, Runtime: task.Runtime, Status: task.Status, Priority: task.Priority,
				CreatedAt: task.CreatedAt, UpdatedAt: task.UpdatedAt},
			ParentTaskID: parentID, SubtaskIDs: subtaskIDs, HierarchyDepth: hierarchyDepth(id), Position: position,
			Phase: phases[id], BlockedBy: blockedBy, Unlocks: unlocks,
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

	const nodeLimit = 500
	selected := map[string]bool{}
	selectID := func(id string) {
		if len(selected) < nodeLimit && connected[id] {
			selected[id] = true
		}
	}
	selectID(taskID)
	selectID(rootID)
	ancestorID, selectedAncestors := taskID, map[string]bool{}
	for {
		edge, ok := parentByChild[ancestorID]
		if !ok || selectedAncestors[ancestorID] {
			break
		}
		selectedAncestors[ancestorID] = true
		ancestorID = edge.parentID
		selectID(ancestorID)
	}
	neighbors := keys(adjacency[taskID])
	sort.Strings(neighbors)
	for _, id := range neighbors {
		selectID(id)
	}
	for _, edge := range subtasksByParent[rootID] {
		selectID(edge.childID)
	}
	for _, node := range nodes {
		selectID(node.Task.ID)
	}
	selectedNodes := make([]model.RelationshipNode, 0, min(len(nodes), nodeLimit))
	validMax := -1
	for _, node := range nodes {
		if node.Phase >= 0 {
			validMax = max(validMax, node.Phase)
		}
		if selected[node.Task.ID] {
			selectedNodes = append(selectedNodes, node)
		}
	}
	result := model.RelationshipGraph{FocusTaskID: taskID, RootTaskID: rootID, TotalConnectedNodes: len(nodes),
		Truncated: len(nodes) > len(selectedNodes), OmittedNodeCount: len(nodes) - len(selectedNodes), Nodes: selectedNodes,
		Hierarchy: []model.HierarchyEdge{}, Dependencies: []model.DependencyEdge{}}
	if validMax >= 0 {
		result.TotalPhases = validMax + 1
	}
	for _, edge := range hierarchy {
		if selected[edge.parentID] && selected[edge.childID] {
			result.Hierarchy = append(result.Hierarchy, model.HierarchyEdge{ParentTaskID: edge.parentID, SubtaskID: edge.childID, Position: edge.position})
		}
	}
	for _, edge := range dependencies {
		if selected[edge.parentID] && selected[edge.childID] {
			result.Dependencies = append(result.Dependencies, model.DependencyEdge{PrerequisiteID: edge.parentID, DependentID: edge.childID, SatisfiedAt: edge.satisfiedAt})
		}
	}
	return result, nil
}

func (s *Store) queryHierarchy(ctx context.Context, board string) ([]hierarchyRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT h.parent_id, h.child_id, h.position FROM task_hierarchy h
		JOIN tasks p ON p.id = h.parent_id WHERE p.board = ? ORDER BY h.parent_id, h.position, h.child_id`, board)
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

func (s *Store) queryDependencies(ctx context.Context, board string) ([]dependencyRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT l.parent_id, l.child_id, l.satisfied_at FROM task_links l
		JOIN tasks p ON p.id = l.parent_id WHERE p.board = ? ORDER BY l.parent_id, l.child_id`, board)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []dependencyRow{}
	for rows.Next() {
		var edge dependencyRow
		var satisfied sql.NullString
		if err := rows.Scan(&edge.parentID, &edge.childID, &satisfied); err != nil {
			return nil, err
		}
		edge.satisfiedAt = stringPointer(satisfied)
		result = append(result, edge)
	}
	return result, rows.Err()
}

func filterHierarchy(values []hierarchyRow, selected map[string]bool) []hierarchyRow {
	result := make([]hierarchyRow, 0, len(values))
	for _, value := range values {
		if selected[value.parentID] && selected[value.childID] {
			result = append(result, value)
		}
	}
	return result
}

func filterDependencies(values []dependencyRow, selected map[string]bool) []dependencyRow {
	result := make([]dependencyRow, 0, len(values))
	for _, value := range values {
		if selected[value.parentID] && selected[value.childID] {
			result = append(result, value)
		}
	}
	return result
}

func keys(values map[string]bool) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	return result
}

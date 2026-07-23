package model

type RelationshipTask struct {
	ID        string     `json:"id"`
	Board     string     `json:"board"`
	Tenant    *string    `json:"tenant"`
	Title     string     `json:"title"`
	Assignee  *string    `json:"assignee"`
	Runtime   Runtime    `json:"runtime"`
	Status    TaskStatus `json:"status"`
	Priority  int        `json:"priority"`
	CreatedAt string     `json:"createdAt"`
	UpdatedAt string     `json:"updatedAt"`
}

type RelationshipNode struct {
	Task           RelationshipTask `json:"task"`
	ParentTaskID   *string          `json:"parentTaskId"`
	SubtaskIDs     []string         `json:"subtaskIds"`
	HierarchyDepth *int             `json:"hierarchyDepth"`
	Position       int              `json:"position"`
	Phase          int              `json:"phase"`
	BlockedBy      []string         `json:"blockedBy"`
	Unlocks        []string         `json:"unlocks"`
}

type HierarchyEdge struct {
	ParentTaskID string `json:"parentTaskId"`
	SubtaskID    string `json:"subtaskId"`
	Position     int    `json:"position"`
}

type DependencyEdge struct {
	PrerequisiteID string  `json:"prerequisiteId"`
	DependentID    string  `json:"dependentId"`
	SatisfiedAt    *string `json:"satisfiedAt"`
	SatisfiedRunID *string `json:"satisfiedRunId"`
}

type RelationshipGraph struct {
	FocusTaskID         string             `json:"focusTaskId"`
	RootTaskID          string             `json:"rootTaskId"`
	TotalPhases         int                `json:"totalPhases"`
	TotalConnectedNodes int                `json:"totalConnectedNodes"`
	Truncated           bool               `json:"truncated"`
	OmittedNodeCount    int                `json:"omittedNodeCount"`
	Nodes               []RelationshipNode `json:"nodes"`
	Hierarchy           []HierarchyEdge    `json:"hierarchy"`
	Dependencies        []DependencyEdge   `json:"dependencies"`
}

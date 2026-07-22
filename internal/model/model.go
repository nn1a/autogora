package model

import "encoding/json"

type TaskStatus string

const (
	TaskStatusTriage    TaskStatus = "triage"
	TaskStatusTodo      TaskStatus = "todo"
	TaskStatusScheduled TaskStatus = "scheduled"
	TaskStatusReady     TaskStatus = "ready"
	TaskStatusRunning   TaskStatus = "running"
	TaskStatusBlocked   TaskStatus = "blocked"
	TaskStatusReview    TaskStatus = "review"
	TaskStatusDone      TaskStatus = "done"
	TaskStatusArchived  TaskStatus = "archived"
)

var TaskStatuses = []TaskStatus{
	TaskStatusTriage,
	TaskStatusTodo,
	TaskStatusScheduled,
	TaskStatusReady,
	TaskStatusRunning,
	TaskStatusBlocked,
	TaskStatusReview,
	TaskStatusDone,
	TaskStatusArchived,
}

type Runtime string

const (
	RuntimeClaude Runtime = "claude"
	RuntimeCodex  Runtime = "codex"
	RuntimeCline  Runtime = "cline"
	RuntimeGemini Runtime = "gemini"
	RuntimeManual Runtime = "manual"
)

var WorkerRuntimes = []Runtime{RuntimeClaude, RuntimeCodex, RuntimeCline, RuntimeGemini}
var Runtimes = []Runtime{RuntimeClaude, RuntimeCodex, RuntimeCline, RuntimeGemini, RuntimeManual}

type BlockKind string

const (
	BlockKindDependency BlockKind = "dependency"
	BlockKindNeedsInput BlockKind = "needs_input"
	BlockKindCapability BlockKind = "capability"
	BlockKindTransient  BlockKind = "transient"
)

type RunStatus string

const (
	RunStatusRunning           RunStatus = "running"
	RunStatusCompleted         RunStatus = "completed"
	RunStatusBlocked           RunStatus = "blocked"
	RunStatusFailed            RunStatus = "failed"
	RunStatusReclaimed         RunStatus = "reclaimed"
	RunStatusCrashed           RunStatus = "crashed"
	RunStatusTimedOut          RunStatus = "timed_out"
	RunStatusRateLimited       RunStatus = "rate_limited"
	RunStatusSpawnFailed       RunStatus = "spawn_failed"
	RunStatusProtocolViolation RunStatus = "protocol_violation"
)

type WorkspaceKind string

const (
	WorkspaceScratch  WorkspaceKind = "scratch"
	WorkspaceDir      WorkspaceKind = "dir"
	WorkspaceWorktree WorkspaceKind = "worktree"
)

type Task struct {
	ID                 string        `json:"id"`
	Board              string        `json:"board"`
	Tenant             *string       `json:"tenant"`
	IdempotencyKey     *string       `json:"idempotencyKey"`
	Title              string        `json:"title"`
	Body               string        `json:"body"`
	Assignee           *string       `json:"assignee"`
	Runtime            Runtime       `json:"runtime"`
	Status             TaskStatus    `json:"status"`
	Priority           int           `json:"priority"`
	Workspace          *string       `json:"workspace"`
	WorkspaceKind      WorkspaceKind `json:"workspaceKind"`
	Branch             *string       `json:"branch"`
	CurrentRunID       *string       `json:"currentRunId"`
	Result             *string       `json:"result"`
	ScheduledAt        *string       `json:"scheduledAt"`
	MaxRuntimeSeconds  *int          `json:"maxRuntimeSeconds"`
	Skills             []string      `json:"skills"`
	GoalMode           bool          `json:"goalMode"`
	GoalMaxTurns       int           `json:"goalMaxTurns"`
	WorkflowTemplateID *string       `json:"workflowTemplateId"`
	CurrentStepKey     *string       `json:"currentStepKey"`
	BlockKind          *BlockKind    `json:"blockKind"`
	BlockReason        *string       `json:"blockReason"`
	BlockRecurrences   int           `json:"blockRecurrences"`
	FailureCount       int           `json:"failureCount"`
	MaxRetries         int           `json:"maxRetries"`
	CreatedAt          string        `json:"createdAt"`
	UpdatedAt          string        `json:"updatedAt"`
}

type Run struct {
	ID             string         `json:"id"`
	TaskID         string         `json:"taskId"`
	WorkerID       string         `json:"workerId"`
	Runtime        Runtime        `json:"runtime"`
	Status         RunStatus      `json:"status"`
	ClaimedAt      string         `json:"claimedAt"`
	ClaimExpiresAt string         `json:"claimExpiresAt"`
	HeartbeatAt    string         `json:"heartbeatAt"`
	EndedAt        *string        `json:"endedAt"`
	PID            *int           `json:"pid"`
	LogPath        *string        `json:"logPath"`
	ExitCode       *int           `json:"exitCode"`
	Summary        *string        `json:"summary"`
	Metadata       map[string]any `json:"metadata"`
	Error          *string        `json:"error"`
}

type RunWorkspace struct {
	RunID          string        `json:"runId"`
	TaskID         string        `json:"taskId"`
	Path           string        `json:"path"`
	Kind           WorkspaceKind `json:"kind"`
	RepositoryPath *string       `json:"repositoryPath"`
	BaseCommit     *string       `json:"baseCommit"`
	Generated      bool          `json:"generated"`
	PreparedAt     string        `json:"preparedAt"`
}

type Comment struct {
	ID        int64  `json:"id"`
	TaskID    string `json:"taskId"`
	Author    string `json:"author"`
	Body      string `json:"body"`
	CreatedAt string `json:"createdAt"`
}

type Attachment struct {
	ID        string  `json:"id"`
	TaskID    string  `json:"taskId"`
	Kind      string  `json:"kind"`
	Name      string  `json:"name"`
	MediaType *string `json:"mediaType"`
	Size      *int64  `json:"size"`
	SHA256    *string `json:"sha256"`
	Path      *string `json:"path"`
	URL       *string `json:"url"`
	CreatedAt string  `json:"createdAt"`
}

type TaskEvent struct {
	ID        int64           `json:"id"`
	TaskID    string          `json:"taskId"`
	RunID     *string         `json:"runId"`
	Kind      string          `json:"kind"`
	Payload   json.RawMessage `json:"payload"`
	CreatedAt string          `json:"createdAt"`
}

type TaskDetail struct {
	Task          Task           `json:"task"`
	Parents       []Task         `json:"parents"`
	Children      []Task         `json:"children"`
	Prerequisites []Task         `json:"prerequisites"`
	Dependents    []Task         `json:"dependents"`
	ParentTask    *Task          `json:"parentTask"`
	Subtasks      []Task         `json:"subtasks"`
	Comments      []Comment      `json:"comments"`
	Attachments   []Attachment   `json:"attachments"`
	Runs          []Run          `json:"runs"`
	RunWorkspaces []RunWorkspace `json:"runWorkspaces"`
	Events        []TaskEvent    `json:"events"`
}

type ClaimedTask struct {
	Task       TaskDetail    `json:"task"`
	Run        Run           `json:"run"`
	ClaimToken string        `json:"claimToken"`
	Workspace  *RunWorkspace `json:"workspace,omitempty"`
}

func ValidTaskStatus(value TaskStatus) bool {
	for _, candidate := range TaskStatuses {
		if candidate == value {
			return true
		}
	}
	return false
}

func ValidRuntime(value Runtime) bool {
	for _, candidate := range Runtimes {
		if candidate == value {
			return true
		}
	}
	return false
}

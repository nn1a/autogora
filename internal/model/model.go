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

type WorkflowRole string

const (
	WorkflowRoleWorker    WorkflowRole = "worker"
	WorkflowRoleReviewer  WorkflowRole = "reviewer"
	WorkflowRoleFinalizer WorkflowRole = "finalizer"
	WorkflowRoleControl   WorkflowRole = "control"
)

var WorkflowRoles = []WorkflowRole{
	WorkflowRoleWorker,
	WorkflowRoleReviewer,
	WorkflowRoleFinalizer,
	WorkflowRoleControl,
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

type AgentHealthStatus string

const (
	AgentHealthUnknown      AgentHealthStatus = "unknown"
	AgentHealthReady        AgentHealthStatus = "ready"
	AgentHealthMissing      AgentHealthStatus = "missing"
	AgentHealthAuthRequired AgentHealthStatus = "auth_required"
	AgentHealthRateLimited  AgentHealthStatus = "rate_limited"
	AgentHealthUnhealthy    AgentHealthStatus = "unhealthy"
)

var AgentHealthStatuses = []AgentHealthStatus{
	AgentHealthUnknown,
	AgentHealthReady,
	AgentHealthMissing,
	AgentHealthAuthRequired,
	AgentHealthRateLimited,
	AgentHealthUnhealthy,
}

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
	WorkflowRole       WorkflowRole  `json:"workflowRole"`
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

// RunAgentConfig is the immutable execution configuration selected for a run.
// Keeping it separate from the task preserves the actual agent choice when
// profiles or global defaults change later.
type RunAgentConfig struct {
	RunID        string  `json:"runId"`
	TaskID       string  `json:"taskId"`
	Profile      string  `json:"profile"`
	Runtime      Runtime `json:"runtime"`
	Model        string  `json:"model"`
	Provider     string  `json:"provider"`
	Source       string  `json:"source"`
	FallbackFrom *string `json:"fallbackFrom"`
	ConfiguredAt string  `json:"configuredAt"`
}

// AgentHealth records the latest runtime availability observation for one
// configured agent. An empty UpdatedAt denotes a synthesized unknown state
// for an agent that has not been checked yet.
type AgentHealth struct {
	AgentID       string            `json:"agentId"`
	Status        AgentHealthStatus `json:"status"`
	CooldownUntil *string           `json:"cooldownUntil"`
	LastError     *string           `json:"lastError"`
	LastRunID     *string           `json:"lastRunId"`
	UpdatedAt     string            `json:"updatedAt"`
}

type TerminalRequest struct {
	RunID       string         `json:"runId"`
	Kind        string         `json:"kind"`
	Summary     *string        `json:"summary"`
	Result      *string        `json:"result"`
	Metadata    map[string]any `json:"metadata"`
	Artifacts   []string       `json:"artifacts"`
	BlockKind   *BlockKind     `json:"blockKind"`
	Reason      *string        `json:"reason"`
	RequestedAt string         `json:"requestedAt"`
	FinalizedAt *string        `json:"finalizedAt"`
}

type ChangeSet struct {
	ID             string   `json:"id"`
	RunID          string   `json:"runId"`
	TaskID         string   `json:"taskId"`
	RepositoryPath string   `json:"repositoryPath"`
	WorktreePath   string   `json:"worktreePath"`
	BaseCommit     string   `json:"baseCommit"`
	HeadCommit     string   `json:"headCommit"`
	DurableRef     string   `json:"durableRef"`
	State          string   `json:"state"`
	ChangedFiles   []string `json:"changedFiles"`
	CreatedAt      string   `json:"createdAt"`
}

// PrerequisiteHandoff identifies the immutable completion run that satisfied
// one dependency edge. Older or administrative completions may not have a run;
// callers must treat a nil Run as a valid handoff without a change set.
type PrerequisiteHandoff struct {
	PrerequisiteID string     `json:"prerequisiteId"`
	DependentID    string     `json:"dependentId"`
	SatisfiedAt    string     `json:"satisfiedAt"`
	SatisfiedRunID *string    `json:"satisfiedRunId"`
	Run            *Run       `json:"run"`
	ChangeSet      *ChangeSet `json:"changeSet"`
}

// IntegrationResolutionTarget identifies one immutable prerequisite commit
// that a finalizer must retain in its resolved history. MergeInProgress is true
// only for the target currently represented by Git's MERGE_HEAD.
type IntegrationResolutionTarget struct {
	PrerequisiteID  string `json:"prerequisiteId"`
	ChangeSetID     string `json:"changeSetId"`
	HeadCommit      string `json:"headCommit"`
	DurableRef      string `json:"durableRef"`
	MergeInProgress bool   `json:"mergeInProgress"`
}

const (
	IntegrationResolutionManifestVersion  = 1
	IntegrationResolutionManifestMaxBytes = 16 << 20
)

// IntegrationResolutionManifest is the complete host-authored handoff kept in
// Git's private metadata directory. Worker argv and environment receive only
// its path and digest, so a large fan-in cannot exceed process argument limits.
type IntegrationResolutionManifest struct {
	Version                 int                           `json:"version"`
	TaskID                  string                        `json:"taskId"`
	RunID                   string                        `json:"runId"`
	ConflictFingerprint     string                        `json:"conflictFingerprint"`
	WorkspacePath           string                        `json:"workspacePath"`
	Targets                 []IntegrationResolutionTarget `json:"targets"`
	ConflictingFiles        []string                      `json:"conflictingFiles"`
	ConflictingFileCount    int                           `json:"conflictingFileCount"`
	ConflictingFilesOmitted int                           `json:"conflictingFilesOmitted"`
}

// IntegrationResolution is an execution-only handoff. It is deliberately
// attached to the claimed run instead of board topology: the generated
// worktree and its unresolved index belong to exactly one active claim.
type IntegrationResolution struct {
	Attempt                 int                           `json:"attempt"`
	MaxAttempts             int                           `json:"maxAttempts"`
	ConflictFingerprint     string                        `json:"conflictFingerprint"`
	WorkspacePath           string                        `json:"workspacePath"`
	ManifestPath            string                        `json:"manifestPath"`
	ManifestSHA256          string                        `json:"manifestSha256"`
	ConflictingFileCount    int                           `json:"conflictingFileCount"`
	ConflictingFilesOmitted int                           `json:"conflictingFilesOmitted"`
	TargetCount             int                           `json:"targetCount"`
	ConflictingFiles        []string                      `json:"-"`
	Targets                 []IntegrationResolutionTarget `json:"-"`
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
	Task                 Task                  `json:"task"`
	Parents              []Task                `json:"parents"`
	Children             []Task                `json:"children"`
	Prerequisites        []Task                `json:"prerequisites"`
	Dependents           []Task                `json:"dependents"`
	ParentTask           *Task                 `json:"parentTask"`
	Subtasks             []Task                `json:"subtasks"`
	Comments             []Comment             `json:"comments"`
	Attachments          []Attachment          `json:"attachments"`
	Runs                 []Run                 `json:"runs"`
	RunAgentConfigs      []RunAgentConfig      `json:"runAgentConfigs"`
	RunWorkspaces        []RunWorkspace        `json:"runWorkspaces"`
	TerminalRequests     []TerminalRequest     `json:"terminalRequests"`
	ChangeSets           []ChangeSet           `json:"changeSets"`
	PrerequisiteHandoffs []PrerequisiteHandoff `json:"prerequisiteHandoffs"`
	Events               []TaskEvent           `json:"events"`
}

type ClaimedTask struct {
	Task                  TaskDetail             `json:"task"`
	Run                   Run                    `json:"run"`
	ClaimToken            string                 `json:"claimToken"`
	Workspace             *RunWorkspace          `json:"workspace,omitempty"`
	IntegrationResolution *IntegrationResolution `json:"integrationResolution,omitempty"`
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

func ValidWorkflowRole(value WorkflowRole) bool {
	for _, candidate := range WorkflowRoles {
		if candidate == value {
			return true
		}
	}
	return false
}

func ValidAgentHealthStatus(value AgentHealthStatus) bool {
	for _, candidate := range AgentHealthStatuses {
		if candidate == value {
			return true
		}
	}
	return false
}

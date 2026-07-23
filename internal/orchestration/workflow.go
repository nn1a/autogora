package orchestration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/store"
)

type ProfileRoute struct {
	Name          string        `json:"name"`
	Runtime       model.Runtime `json:"runtime"`
	Model         string        `json:"model,omitempty"`
	Provider      string        `json:"provider,omitempty"`
	Description   string        `json:"description,omitempty"`
	Disabled      bool          `json:"disabled,omitempty"`
	MaxConcurrent int           `json:"maxConcurrent,omitempty"`
	Priority      int           `json:"priority,omitempty"`
	Fallbacks     []string      `json:"fallbacks,omitempty"`
}

type SpecificationPlan struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

type DecompositionTask struct {
	Key      string        `json:"key"`
	Title    string        `json:"title"`
	Body     string        `json:"body"`
	Assignee string        `json:"assignee"`
	Runtime  model.Runtime `json:"runtime"`
	Priority int           `json:"priority"`
	Skills   []string      `json:"skills"`
}

type DecompositionPlan struct {
	Fanout       bool                        `json:"fanout"`
	RootTitle    string                      `json:"rootTitle"`
	RootBody     string                      `json:"rootBody"`
	Reason       string                      `json:"reason"`
	Tasks        []DecompositionTask         `json:"tasks"`
	Dependencies []store.TaskGraphDependency `json:"dependencies"`
}

type DecompositionResult struct {
	Fanout bool                   `json:"fanout"`
	Reason string                 `json:"reason"`
	Task   model.TaskDetail       `json:"task"`
	Graph  *store.TaskGraphResult `json:"graph,omitempty"`
}

type GoalJudgment struct {
	Complete   bool   `json:"complete"`
	Reason     string `json:"reason"`
	NextPrompt string `json:"nextPrompt"`
}

var specificationSchema = map[string]any{
	"type": "object", "additionalProperties": false,
	"properties": map[string]any{
		"title": map[string]any{"type": "string", "minLength": 1},
		"body":  map[string]any{"type": "string", "minLength": 1},
	},
	"required": []string{"title", "body"},
}

var decompositionSchema = map[string]any{
	"type": "object", "additionalProperties": false,
	"properties": map[string]any{
		"fanout":    map[string]any{"type": "boolean"},
		"rootTitle": map[string]any{"type": "string"},
		"rootBody":  map[string]any{"type": "string"},
		"reason":    map[string]any{"type": "string"},
		"tasks": map[string]any{"type": "array", "maxItems": 100, "items": map[string]any{
			"type": "object", "additionalProperties": false,
			"properties": map[string]any{
				"key": map[string]any{"type": "string", "minLength": 1}, "title": map[string]any{"type": "string", "minLength": 1},
				"body": map[string]any{"type": "string", "minLength": 1}, "assignee": map[string]any{"type": "string", "minLength": 1},
				"runtime":  map[string]any{"type": "string", "enum": []string{"claude", "codex", "cline", "gemini"}},
				"priority": map[string]any{"type": "integer"}, "skills": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			},
			"required": []string{"key", "title", "body", "assignee", "runtime", "priority", "skills"},
		}},
		"dependencies": map[string]any{"type": "array", "items": map[string]any{
			"type": "object", "additionalProperties": false,
			"properties": map[string]any{
				"parent": map[string]any{"type": "string", "minLength": 1}, "child": map[string]any{"type": "string", "minLength": 1},
			}, "required": []string{"parent", "child"},
		}},
	},
	"required": []string{"fanout", "rootTitle", "rootBody", "reason", "tasks", "dependencies"},
}

var goalJudgeSchema = map[string]any{
	"type": "object", "additionalProperties": false,
	"properties": map[string]any{
		"complete": map[string]any{"type": "boolean"}, "reason": map[string]any{"type": "string", "minLength": 1}, "nextPrompt": map[string]any{"type": "string"},
	}, "required": []string{"complete", "reason", "nextPrompt"},
}

var profileDescriptionSchema = map[string]any{
	"type": "object", "additionalProperties": false,
	"properties": map[string]any{"description": map[string]any{"type": "string", "minLength": 1, "maxLength": 500}},
	"required":   []string{"description"},
}

func decodePlan(value any, destination any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(strings.NewReader(string(encoded)))
	decoder.DisallowUnknownFields()
	return decoder.Decode(destination)
}

func validateSpecification(plan *SpecificationPlan) error {
	plan.Title, plan.Body = strings.TrimSpace(plan.Title), strings.TrimSpace(plan.Body)
	if plan.Title == "" {
		return errors.New("specification title must be a non-empty string")
	}
	if plan.Body == "" {
		return errors.New("specification body must be a non-empty string")
	}
	return nil
}

func validateDecomposition(plan *DecompositionPlan) error {
	plan.RootTitle, plan.RootBody, plan.Reason = strings.TrimSpace(plan.RootTitle), strings.TrimSpace(plan.RootBody), strings.TrimSpace(plan.Reason)
	if plan.RootTitle == "" || plan.RootBody == "" {
		return errors.New("decomposition root title and body must be non-empty")
	}
	if len(plan.Tasks) > 100 {
		return errors.New("decomposition cannot exceed 100 tasks")
	}
	if plan.Fanout && len(plan.Tasks) == 0 {
		return errors.New("a fanout decomposition must include tasks")
	}
	seen := map[string]bool{}
	for index := range plan.Tasks {
		task := &plan.Tasks[index]
		task.Key, task.Title, task.Body, task.Assignee = strings.TrimSpace(task.Key), strings.TrimSpace(task.Title), strings.TrimSpace(task.Body), strings.TrimSpace(task.Assignee)
		if task.Key == "" || task.Title == "" || task.Body == "" || task.Assignee == "" {
			return fmt.Errorf("decomposition task %d has an empty required field", index+1)
		}
		if seen[task.Key] {
			return fmt.Errorf("duplicate decomposition task key: %s", task.Key)
		}
		seen[task.Key] = true
		if task.Runtime == model.RuntimeManual || !model.ValidRuntime(task.Runtime) {
			return fmt.Errorf("invalid task runtime: %s", task.Runtime)
		}
		unique := map[string]bool{}
		cleaned := make([]string, 0, len(task.Skills))
		for _, skill := range task.Skills {
			if skill = strings.TrimSpace(skill); skill != "" && !unique[skill] {
				unique[skill] = true
				cleaned = append(cleaned, skill)
			}
		}
		task.Skills = cleaned
	}
	for index := range plan.Dependencies {
		dependency := &plan.Dependencies[index]
		dependency.Parent, dependency.Child = strings.TrimSpace(dependency.Parent), strings.TrimSpace(dependency.Child)
		if dependency.Parent == "" || dependency.Child == "" {
			return fmt.Errorf("dependency %d has an empty endpoint", index+1)
		}
	}
	return nil
}

func specificationPrompt(task model.TaskDetail) string {
	tenant := "(none)"
	if task.Task.Tenant != nil {
		tenant = *task.Task.Tenant
	}
	body := task.Task.Body
	if body == "" {
		body = "(empty)"
	}
	return strings.Join([]string{
		"You are an Autogora triage specifier.",
		"Rewrite the rough idea into a precise, executable task without inventing external facts.",
		"The body must include scope, concrete deliverables, acceptance criteria, constraints, and verification.",
		"Return only the requested structured object.", "",
		"Task id: " + task.Task.ID, "Title: " + task.Task.Title, "Body: " + body, "Tenant: " + tenant,
	}, "\n")
}

func decompositionPrompt(task model.TaskDetail, profiles []ProfileRoute) string {
	roster := make([]string, 0, len(profiles))
	for _, profile := range profiles {
		if profile.Disabled {
			continue
		}
		description := strings.TrimSpace(profile.Description)
		if description == "" {
			description = "no description"
		}
		execution := string(profile.Runtime)
		if profile.Model != "" {
			execution += "/" + profile.Model
		}
		roster = append(roster, fmt.Sprintf("- %s [%s]: %s", profile.Name, execution, description))
	}
	if len(roster) == 0 {
		roster = append(roster, "(empty)")
	}
	body := task.Task.Body
	if body == "" {
		body = "(empty)"
	}
	tenant := "(none)"
	if task.Task.Tenant != nil {
		tenant = *task.Task.Tenant
	}
	return strings.Join([]string{
		"You are an Autogora graph decomposer.",
		"Decide whether this triage idea benefits from independent parallel or sequential specialist tasks.",
		"If not, set fanout=false and return an improved rootTitle/rootBody with empty tasks and dependencies.",
		"If yes, produce a small acyclic graph. Every generated task becomes a direct subtask of the triage root.",
		"Dependencies control execution only: dependency parent is the prerequisite and child waits for parent.",
		"Do not add dependency edges to the root; Autogora automatically makes the root wait for every terminal subtask so it can perform final coordination or verification.",
		"Use only assignee names from the profile roster. Every task needs a complete handoff-ready body.",
		"Return only the requested structured object.", "",
		"Task id: " + task.Task.ID, "Title: " + task.Task.Title, "Body: " + body, "Tenant: " + tenant, "", "Profile roster:", strings.Join(roster, "\n"),
	}, "\n")
}

func SpecifyTriageTask(ctx context.Context, opened *store.Store, taskID string, planner Planner, explicit *SpecificationPlan, author string) (model.TaskDetail, error) {
	task, err := opened.GetTask(ctx, taskID)
	if err != nil {
		return model.TaskDetail{}, err
	}
	if task.Task.Status != model.TaskStatusTriage {
		return model.TaskDetail{}, fmt.Errorf("task is not in triage: %s", taskID)
	}
	plan := SpecificationPlan{}
	if explicit != nil {
		plan = *explicit
	} else {
		if planner == nil {
			return model.TaskDetail{}, errors.New("a planner or explicit specification is required")
		}
		value, err := planner(ctx, PlannerRequest{Kind: PlannerSpecify, Prompt: specificationPrompt(task), Schema: specificationSchema})
		if err != nil {
			return model.TaskDetail{}, err
		}
		if err := decodePlan(value, &plan); err != nil {
			return model.TaskDetail{}, fmt.Errorf("invalid specification plan: %w", err)
		}
	}
	if err := validateSpecification(&plan); err != nil {
		return model.TaskDetail{}, err
	}
	return opened.SpecifyTask(ctx, taskID, plan.Title, plan.Body, author)
}

type DecomposeOptions struct {
	Profiles            []ProfileRoute
	DefaultProfile      ProfileRoute
	OrchestratorProfile *ProfileRoute
	AutoPromoteChildren *bool
	Planner             Planner
	Plan                *DecompositionPlan
}

func DecomposeTriageTask(ctx context.Context, opened *store.Store, taskID string, options DecomposeOptions) (DecompositionResult, error) {
	task, err := opened.GetTask(ctx, taskID)
	if err != nil {
		return DecompositionResult{}, err
	}
	if task.Task.Status != model.TaskStatusTriage {
		return DecompositionResult{}, fmt.Errorf("task is not in triage: %s", taskID)
	}
	if !RunnableProfileRoute(options.DefaultProfile) {
		return DecompositionResult{}, errors.New("decomposition requires an enabled worker profile")
	}
	profiles := make([]ProfileRoute, 0, len(options.Profiles)+1)
	byName := map[string]ProfileRoute{}
	for _, profile := range append(append([]ProfileRoute{}, options.Profiles...), options.DefaultProfile) {
		if !RunnableProfileRoute(profile) {
			continue
		}
		if _, exists := byName[profile.Name]; !exists {
			byName[profile.Name] = profile
			profiles = append(profiles, profile)
		}
	}
	plan := DecompositionPlan{}
	if options.Plan != nil {
		plan = *options.Plan
	} else {
		if options.Planner == nil {
			return DecompositionResult{}, errors.New("a planner or explicit decomposition plan is required")
		}
		value, err := options.Planner(ctx, PlannerRequest{Kind: PlannerDecompose, Prompt: decompositionPrompt(task, profiles), Schema: decompositionSchema})
		if err != nil {
			return DecompositionResult{}, err
		}
		if err := decodePlan(value, &plan); err != nil {
			return DecompositionResult{}, fmt.Errorf("invalid decomposition plan: %w", err)
		}
	}
	if err := validateDecomposition(&plan); err != nil {
		return DecompositionResult{}, err
	}
	if !plan.Fanout {
		specified, err := opened.SpecifyTask(ctx, taskID, plan.RootTitle, plan.RootBody, "decomposer")
		if err == nil && strings.TrimSpace(options.DefaultProfile.Name) != "" && options.DefaultProfile.Runtime != model.RuntimeManual && model.ValidRuntime(options.DefaultProfile.Runtime) {
			assignee, runtime := options.DefaultProfile.Name, options.DefaultProfile.Runtime
			specified, err = opened.UpdateTask(ctx, taskID, store.UpdateTaskInput{
				Assignee: store.OptionalString{Set: true, Value: &assignee}, Runtime: &runtime,
			})
		}
		return DecompositionResult{Fanout: false, Reason: plan.Reason, Task: specified}, err
	}
	nodes := make([]store.TaskGraphNode, 0, len(plan.Tasks))
	for _, planned := range plan.Tasks {
		profile, exists := byName[planned.Assignee]
		if !exists {
			profile = options.DefaultProfile
		}
		priority := planned.Priority
		nodes = append(nodes, store.TaskGraphNode{Key: planned.Key, Title: planned.Title, Body: planned.Body, Assignee: profile.Name, Runtime: profile.Runtime, Priority: &priority, Skills: planned.Skills})
	}
	orchestrator := options.DefaultProfile
	if options.OrchestratorProfile != nil {
		orchestrator = *options.OrchestratorProfile
	}
	graph, err := opened.ApplyTaskGraph(ctx, store.TaskGraphInput{
		RootTaskID: taskID, RootTitle: plan.RootTitle, RootBody: plan.RootBody,
		OrchestratorAssignee: orchestrator.Name, OrchestratorRuntime: orchestrator.Runtime,
		AutoPromoteChildren: options.AutoPromoteChildren, Nodes: nodes, Dependencies: plan.Dependencies,
	})
	if err != nil {
		return DecompositionResult{}, err
	}
	return DecompositionResult{Fanout: true, Reason: plan.Reason, Task: graph.Root, Graph: &graph}, nil
}

func JudgeGoalProgress(ctx context.Context, task model.TaskDetail, turn int, workerOutput string, planner Planner) (GoalJudgment, error) {
	if planner == nil {
		return GoalJudgment{}, errors.New("a goal judgment planner is required")
	}
	if len(workerOutput) > 32*1024 {
		workerOutput = workerOutput[len(workerOutput)-32*1024:]
		for !utf8.ValidString(workerOutput) && len(workerOutput) > 0 {
			workerOutput = workerOutput[1:]
		}
	}
	result := task.Task.Result
	resultText := "(none)"
	if result != nil {
		resultText = *result
	}
	body := task.Task.Body
	if body == "" {
		body = "(empty)"
	}
	if workerOutput == "" {
		workerOutput = "(empty)"
	}
	prompt := strings.Join([]string{
		"You are the independent completion judge for a goal-mode Autogora worker.",
		"Compare the worker's latest output and durable task state against every acceptance criterion.",
		"Set complete=true only when the goal is demonstrably satisfied. Otherwise give one concrete next-turn instruction.",
		"Do not treat confidence, effort, or a promise to finish as evidence.", "Return only the requested structured object.", "",
		fmt.Sprintf("Turn: %d of %d", turn, task.Task.GoalMaxTurns), "Task: " + task.Task.Title,
		"Acceptance body:\n" + body, "Current status: " + string(task.Task.Status), "Current result: " + resultText, "", "Latest worker output:\n" + workerOutput,
	}, "\n")
	value, err := planner(ctx, PlannerRequest{Kind: PlannerGoalJudge, Prompt: prompt, Schema: goalJudgeSchema})
	if err != nil {
		return GoalJudgment{}, err
	}
	judgment := GoalJudgment{}
	if err := decodePlan(value, &judgment); err != nil {
		return GoalJudgment{}, fmt.Errorf("invalid goal judgment: %w", err)
	}
	judgment.Reason, judgment.NextPrompt = strings.TrimSpace(judgment.Reason), strings.TrimSpace(judgment.NextPrompt)
	if judgment.Reason == "" {
		return GoalJudgment{}, errors.New("goal judgment reason must be non-empty")
	}
	return judgment, nil
}

type ProfileEvidence struct {
	Title  string
	Body   string
	Skills []string
}

func DescribeProfileRoute(ctx context.Context, profile ProfileRoute, evidence []ProfileEvidence, planner Planner) (ProfileRoute, error) {
	if planner == nil {
		return ProfileRoute{}, errors.New("a profile description planner is required")
	}
	examples := make([]string, 0, min(20, len(evidence)))
	for _, task := range evidence[:min(20, len(evidence))] {
		body := task.Body
		if len(body) > 300 {
			body = body[:300]
		}
		if body == "" {
			body = "(no body)"
		}
		skills := strings.Join(task.Skills, ", ")
		if skills == "" {
			skills = "none"
		}
		examples = append(examples, fmt.Sprintf("- %s: %s; skills=%s", task.Title, body, skills))
	}
	if len(examples) == 0 {
		examples = append(examples, "(none; describe it conservatively from its name and runtime)")
	}
	existing := strings.TrimSpace(profile.Description)
	if existing == "" {
		existing = "(none)"
	}
	prompt := strings.Join([]string{
		"You describe a Claude, Codex, Cline, or Gemini Autogora worker profile for a task-routing planner.",
		"Write one concise capability description grounded only in the supplied evidence.",
		"State the work this profile should receive and any evident specialization. Do not use marketing language.",
		"Return only the requested structured object.", "", "Profile: " + profile.Name, "Runtime: " + string(profile.Runtime),
		"Existing description: " + existing, "Observed tasks:", strings.Join(examples, "\n"),
	}, "\n")
	value, err := planner(ctx, PlannerRequest{Kind: PlannerProfileDescribe, Prompt: prompt, Schema: profileDescriptionSchema})
	if err != nil {
		return ProfileRoute{}, err
	}
	result := struct {
		Description string `json:"description"`
	}{}
	if err := decodePlan(value, &result); err != nil {
		return ProfileRoute{}, fmt.Errorf("invalid profile description: %w", err)
	}
	description := strings.TrimSpace(result.Description)
	if description == "" {
		return ProfileRoute{}, errors.New("profile description must be non-empty")
	}
	runes := []rune(description)
	if len(runes) > 500 {
		description = string(runes[:500])
	}
	profile.Description = description
	return profile, nil
}

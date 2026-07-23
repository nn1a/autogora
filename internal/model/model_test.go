package model

import (
	"encoding/json"
	"testing"
)

func TestTaskJSONUsesExistingContractNames(t *testing.T) {
	task := Task{
		ID:            "task-1",
		Board:         "default",
		Title:         "Port Autogora",
		Runtime:       RuntimeCodex,
		Status:        TaskStatusReady,
		WorkflowRole:  WorkflowRoleReviewer,
		WorkspaceKind: WorkspaceScratch,
		Skills:        []string{},
	}

	encoded, err := json.Marshal(task)
	if err != nil {
		t.Fatal(err)
	}

	var value map[string]any
	if err := json.Unmarshal(encoded, &value); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"id", "currentRunId", "workspaceKind", "workflowRole", "goalMaxTurns", "createdAt"} {
		if _, ok := value[key]; !ok {
			t.Fatalf("JSON contract is missing %q: %s", key, encoded)
		}
	}
}

func TestStatusAndRuntimeValidation(t *testing.T) {
	if !ValidTaskStatus(TaskStatusRunning) || ValidTaskStatus("unknown") {
		t.Fatal("task status validation does not match the public contract")
	}
	if !ValidRuntime(RuntimeGemini) || ValidRuntime("unknown") {
		t.Fatal("runtime validation does not match the public contract")
	}
	if !ValidWorkflowRole(WorkflowRoleControl) || ValidWorkflowRole("coordinator") {
		t.Fatal("workflow role validation does not match the public contract")
	}
}

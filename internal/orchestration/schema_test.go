package orchestration

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/nn1a/autogora/internal/model"
)

func TestPlannerSchemasMeetStrictObjectRequirements(t *testing.T) {
	for name, schema := range map[string]map[string]any{
		"specification":       specificationSchema,
		"decomposition":       decompositionSchema,
		"goal judgment":       goalJudgeSchema,
		"profile description": profileDescriptionSchema,
	} {
		t.Run(name, func(t *testing.T) {
			assertStrictObjectSchema(t, "$", schema)
		})
	}
}

func TestLegacyNullableDecompositionWorkflowRoleKeepsWorkerDefault(t *testing.T) {
	raw := map[string]any{
		"fanout": true, "rootTitle": "Root", "rootBody": "Root body",
		"reason": "parallel work",
		"tasks": []any{map[string]any{
			"key": "worker", "title": "Worker", "body": "Implement",
			"assignee": "codex", "runtime": "codex", "workflowRole": nil,
			"priority": 1, "skills": []any{},
		}},
		"dependencies": []any{},
	}
	var plan DecompositionPlan
	if err := decodePlan(raw, &plan); err != nil {
		t.Fatal(err)
	}
	if err := validateDecomposition(&plan, nil); err != nil {
		t.Fatal(err)
	}
	if len(plan.Tasks) != 1 ||
		plan.Tasks[0].WorkflowRole != model.WorkflowRoleWorker {
		t.Fatalf("nullable workflow role did not use worker default: %+v", plan.Tasks)
	}
}

func TestDecompositionSchemaRequiresExplicitWorkflowRole(t *testing.T) {
	tasks := decompositionSchema["properties"].(map[string]any)["tasks"].(map[string]any)
	item := tasks["items"].(map[string]any)
	role := item["properties"].(map[string]any)["workflowRole"].(map[string]any)
	if role["type"] != "string" {
		t.Fatalf("workflowRole type = %#v, want string", role["type"])
	}
}

func TestDecompositionSchemaConstrainsSkillsToRootAllowlist(t *testing.T) {
	withoutSkills := decompositionSkillsSchema(t, decompositionSchemaForSkills(nil))
	if withoutSkills["maxItems"] != 0 {
		t.Fatalf("skills maxItems = %#v, want 0 when root has no configured skills", withoutSkills["maxItems"])
	}
	if _, exists := withoutSkills["items"].(map[string]any)["enum"]; exists {
		t.Fatal("empty root skill schema has an enum; maxItems=0 should require []")
	}

	withSkills := decompositionSkillsSchema(t, decompositionSchemaForSkills([]string{
		" go ", "accessibility", "go", "",
	}))
	if _, exists := withSkills["maxItems"]; exists {
		t.Fatalf("configured skill schema unexpectedly limits the array to zero: %#v", withSkills)
	}
	got, ok := withSkills["items"].(map[string]any)["enum"].([]string)
	if !ok || !reflect.DeepEqual(got, []string{"go", "accessibility"}) {
		t.Fatalf("skill enum = %#v, want normalized root skill IDs", withSkills["items"].(map[string]any)["enum"])
	}
	assertStrictObjectSchema(t, "$", decompositionSchemaForSkills([]string{"go"}))
}

func decompositionSkillsSchema(t *testing.T, schema map[string]any) map[string]any {
	t.Helper()
	tasks := schema["properties"].(map[string]any)["tasks"].(map[string]any)
	item := tasks["items"].(map[string]any)
	return item["properties"].(map[string]any)["skills"].(map[string]any)
}

func assertStrictObjectSchema(t *testing.T, path string, value any) {
	t.Helper()
	switch typed := value.(type) {
	case map[string]any:
		if typed["type"] == "object" {
			if additional, exists := typed["additionalProperties"]; !exists || additional != false {
				t.Errorf("%s object additionalProperties = %#v, want false", path, additional)
			}
			properties, ok := typed["properties"].(map[string]any)
			if !ok {
				t.Errorf("%s object properties = %#v, want map", path, typed["properties"])
			} else {
				required, ok := typed["required"].([]string)
				if !ok {
					t.Errorf("%s object required = %#v, want []string", path, typed["required"])
				} else {
					requiredSet := make(map[string]bool, len(required))
					for _, name := range required {
						if requiredSet[name] {
							t.Errorf("%s required contains duplicate %q", path, name)
						}
						requiredSet[name] = true
						if _, exists := properties[name]; !exists {
							t.Errorf("%s requires unknown property %q", path, name)
						}
					}
					for name := range properties {
						if !requiredSet[name] {
							t.Errorf("%s property %q is not required", path, name)
						}
					}
					if len(required) != len(properties) {
						t.Errorf(
							"%s required/property count = %d/%d",
							path,
							len(required),
							len(properties),
						)
					}
				}
			}
		}
		for name, nested := range typed {
			assertStrictObjectSchema(t, fmt.Sprintf("%s.%s", path, name), nested)
		}
	case []any:
		for index, nested := range typed {
			assertStrictObjectSchema(t, fmt.Sprintf("%s[%d]", path, index), nested)
		}
	}
}

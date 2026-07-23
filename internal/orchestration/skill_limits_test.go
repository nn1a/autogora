package orchestration

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/store"
)

func TestRootSkillProductLimitBoundaries(t *testing.T) {
	t.Run("100 unique skills", func(t *testing.T) {
		skills := numberedSkillIDs(maxRootTaskSkills)
		normalized, err := normalizeAndValidateRootSkills(skills)
		if err != nil || !reflect.DeepEqual(normalized, skills) {
			t.Fatalf("exact unique skill boundary = %#v, %v", normalized, err)
		}
	})

	t.Run("128 UTF-8 bytes per skill", func(t *testing.T) {
		skill := strings.Repeat("가", 42) + "ab"
		if len(skill) != maxRootTaskSkillBytes || utf8.RuneCountInString(skill) >= len(skill) {
			t.Fatalf("test skill is %d bytes and %d runes", len(skill), utf8.RuneCountInString(skill))
		}
		normalized, err := normalizeAndValidateRootSkills([]string{skill})
		if err != nil || len(normalized) != 1 || normalized[0] != skill {
			t.Fatalf("exact per-skill byte boundary = %#v, %v", normalized, err)
		}
	})

	t.Run("8192 total bytes", func(t *testing.T) {
		skills := fixedWidthSkillIDs(64, maxRootTaskSkillBytes)
		normalized, err := normalizeAndValidateRootSkills(skills)
		if err != nil || !reflect.DeepEqual(normalized, skills) {
			t.Fatalf("exact total byte boundary = %d skills, %v", len(normalized), err)
		}
	})

	t.Run("trim and dedupe precede limits", func(t *testing.T) {
		skill := strings.Repeat("x", maxRootTaskSkillBytes)
		raw := make([]string, 0, maxRootTaskSkills+3)
		for range maxRootTaskSkills + 1 {
			raw = append(raw, " \t"+skill+"\n")
		}
		raw = append(raw, "", "   ")
		normalized, err := normalizeAndValidateRootSkills(raw)
		if err != nil || !reflect.DeepEqual(normalized, []string{skill}) {
			t.Fatalf("normalized duplicates = %#v, %v", normalized, err)
		}
	})
}

func TestRootSkillProductLimitsRejectOversizedOrInvalidInput(t *testing.T) {
	overlongMultibyte := strings.Repeat("가", 43)
	if len(overlongMultibyte) != maxRootTaskSkillBytes+1 ||
		utf8.RuneCountInString(overlongMultibyte) != 43 {
		t.Fatalf("multibyte fixture is %d bytes and %d runes", len(overlongMultibyte), utf8.RuneCountInString(overlongMultibyte))
	}
	overTotal := append(fixedWidthSkillIDs(64, maxRootTaskSkillBytes), "z")
	invalidUTF8 := string([]byte{'s', 'k', 'i', 'l', 'l', '-', 0xff})

	for _, test := range []struct {
		name   string
		skills []string
		want   string
	}{
		{name: "101 unique", skills: numberedSkillIDs(maxRootTaskSkills + 1), want: "101 unique skills"},
		{name: "129 byte multibyte ID", skills: []string{overlongMultibyte}, want: "129 bytes"},
		{name: "8193 total bytes", skills: overTotal, want: "total 8193 bytes"},
		{name: "invalid UTF-8", skills: []string{invalidUTF8}, want: "not valid UTF-8"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := normalizeAndValidateRootSkills(test.skills); err == nil ||
				!strings.Contains(err.Error(), test.want) {
				t.Fatalf("validation error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestDecomposeRejectsInvalidRootSkillsBeforePlannerOrMutation(t *testing.T) {
	for _, mode := range []string{"planner", "explicit plan"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			opened := openMemoryStore(t)
			root, err := opened.CreateTask(ctx, store.CreateTaskInput{
				Title:  "Build game",
				Body:   "Keep this task unchanged.",
				Status: model.TaskStatusTriage,
				Skills: numberedSkillIDs(maxRootTaskSkills + 1),
			})
			if err != nil {
				t.Fatal(err)
			}
			before, err := opened.GetTask(ctx, root.Task.ID)
			if err != nil {
				t.Fatal(err)
			}

			plannerCalled := false
			options := DecomposeOptions{
				DefaultProfile: ProfileRoute{Name: "worker", Runtime: model.RuntimeCodex},
				Planner: func(context.Context, PlannerRequest) (any, error) {
					plannerCalled = true
					return map[string]any{
						"fanout": false, "rootTitle": "Changed", "rootBody": "Changed",
						"reason": "not reached", "tasks": []any{}, "dependencies": []any{},
					}, nil
				},
			}
			if mode == "explicit plan" {
				options.Plan = &DecompositionPlan{
					Fanout: false, RootTitle: "Changed", RootBody: "Changed", Reason: "not reached",
				}
			}

			_, err = DecomposeTriageTask(ctx, opened, root.Task.ID, options)
			if err == nil || !strings.Contains(err.Error(), "invalid root task skills") {
				t.Fatalf("decomposition error = %v", err)
			}
			if plannerCalled {
				t.Fatal("planner was called for an invalid root skill allowlist")
			}
			after, err := opened.GetTask(ctx, root.Task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(after, before) ||
				after.Task.Status != model.TaskStatusTriage ||
				len(after.Subtasks) != 0 {
				t.Fatalf("invalid root skill allowlist mutated the task:\nbefore=%+v\nafter=%+v", before, after)
			}
		})
	}
}

func numberedSkillIDs(count int) []string {
	result := make([]string, count)
	for index := range count {
		result[index] = fmt.Sprintf("skill-%03d", index)
	}
	return result
}

func fixedWidthSkillIDs(count, width int) []string {
	result := make([]string, count)
	for index := range count {
		prefix := fmt.Sprintf("skill-%02d-", index)
		result[index] = prefix + strings.Repeat("x", width-len(prefix))
	}
	return result
}

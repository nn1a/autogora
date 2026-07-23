package dashboard

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/nn1a/autogora/internal/boards"
	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/store"
)

func createDashboardPublication(
	t *testing.T,
	server *Server,
	board, suffix string,
	mode model.PublicationMode,
	requireApproval bool,
) model.Publication {
	t.Helper()
	ctx := context.Background()
	opened, err := server.manager.OpenStore(ctx, board)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	detail, err := opened.CreateTask(ctx, store.CreateTaskInput{
		Title: "dashboard publication " + suffix, Board: board,
		Runtime: model.RuntimeManual, Status: model.TaskStatusReady,
		WorkflowRole: model.WorkflowRoleFinalizer,
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := opened.ClaimTask(ctx, store.ClaimOptions{
		TaskID: detail.Task.ID, Board: board, WorkerID: "dashboard-publication-test",
		ClaimTTLSeconds: 300,
	})
	if err != nil || claimed == nil {
		t.Fatalf("claim publication source: value=%+v err=%v", claimed, err)
	}
	scope := store.RunScope{
		RunID: claimed.Run.ID, ClaimToken: claimed.ClaimToken,
	}
	if _, err := opened.RequestRunCompletion(ctx, scope, store.CompletionInput{
		Summary: "publication source ready",
	}); err != nil {
		t.Fatal(err)
	}
	changeSet, err := opened.RecordRunChangeSet(ctx, scope, store.RecordChangeSetInput{
		RunID: claimed.Run.ID, RepositoryPath: "/repo/" + suffix,
		WorktreePath: "/worktree/" + suffix, BaseCommit: "base-" + suffix,
		HeadCommit: "head-" + suffix,
		DurableRef: "refs/autogora/runs/" + claimed.Run.ID,
		State:      "ready", ChangedFiles: []string{suffix + ".go"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := opened.FinalizeRunTerminal(ctx, claimed.Run.ID, 0); err != nil {
		t.Fatal(err)
	}
	value, created, err := opened.EnsurePublication(ctx, store.EnsurePublicationInput{
		Board: board, ChangeSetID: changeSet.ID, Mode: mode,
		TargetBranch: "main", Remote: "origin", RequireApproval: requireApproval,
	})
	if err != nil || !created {
		t.Fatalf("ensure publication: value=%+v created=%v err=%v", value, created, err)
	}
	return value
}

func failDashboardPublication(
	t *testing.T,
	server *Server,
	board string,
	value model.Publication,
) model.Publication {
	t.Helper()
	ctx := context.Background()
	opened, err := server.manager.OpenStore(ctx, board)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	current := time.Now().UTC()
	claimed, acquired, err := opened.ClaimPublication(
		ctx,
		value.ID,
		store.ClaimPublicationInput{
			ExpectedUpdatedAt: value.UpdatedAt, TTL: time.Minute, Current: current,
		},
	)
	if err != nil || !acquired {
		t.Fatalf("claim publication: value=%+v acquired=%v err=%v", claimed, acquired, err)
	}
	failed, err := opened.FailPublication(ctx, value.ID, store.FailPublicationInput{
		ExpectedUpdatedAt: claimed.UpdatedAt, ClaimToken: claimed.ClaimToken,
		Current: current.Add(time.Second), Error: "temporary publication failure",
	})
	if err != nil {
		t.Fatal(err)
	}
	return failed
}

func TestPublicationsAPIListsFiltersAndScopesRecords(t *testing.T) {
	server := startTestServer(t)
	ctx := context.Background()
	if _, err := server.manager.Create(ctx, "other", boards.Update{}); err != nil {
		t.Fatal(err)
	}
	first := createDashboardPublication(
		t, server, "default", "list-first", model.PublicationModePullRequest, false,
	)
	createDashboardPublication(
		t, server, "default", "list-second", model.PublicationModeManual, true,
	)
	other := createDashboardPublication(
		t, server, "other", "list-other", model.PublicationModeManual, false,
	)

	path := "/api/publications?board=default&status=pending&task=" +
		first.TaskID + "&run=" + first.RunID + "&changeSet=" +
		first.ChangeSetID + "&limit=1"
	response, raw := apiRequest(t, server, http.MethodGet, path, nil)
	values := arrayValue(t, raw)
	if response.StatusCode != http.StatusOK || len(values) != 1 ||
		mapValue(t, values[0])["id"] != first.ID {
		t.Fatalf("filtered publications: status=%d values=%#v", response.StatusCode, values)
	}
	if _, leaked := mapValue(t, values[0])["claimToken"]; leaked {
		t.Fatalf("publication list leaked claim token: %#v", values[0])
	}

	response, raw = apiRequest(
		t,
		server,
		http.MethodGet,
		"/api/publications/"+first.ID+"?board=default",
		nil,
	)
	if response.StatusCode != http.StatusOK ||
		mapValue(t, raw)["changeSetId"] != first.ChangeSetID {
		t.Fatalf("publication detail: status=%d value=%#v", response.StatusCode, raw)
	}
	response, _ = apiRequest(
		t,
		server,
		http.MethodGet,
		"/api/publications/"+other.ID+"?board=default",
		nil,
	)
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-board publication status = %d", response.StatusCode)
	}
	response, raw = apiRequest(
		t,
		server,
		http.MethodGet,
		"/api/publications?board=default&status=unknown",
		nil,
	)
	if response.StatusCode != http.StatusBadRequest ||
		!strings.Contains(mapValue(t, raw)["error"].(string), "invalid publication status") {
		t.Fatalf("invalid status filter: status=%d value=%#v", response.StatusCode, raw)
	}
	response, _ = apiRequest(
		t,
		server,
		http.MethodGet,
		"/api/publications?board=default&limit=501",
		nil,
	)
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid publication limit status = %d", response.StatusCode)
	}
}

func TestPublicationsAPIMutationsUseStrictCASInputs(t *testing.T) {
	server := startTestServer(t)

	awaiting := createDashboardPublication(
		t, server, "default", "approve", model.PublicationModePullRequest, true,
	)
	approvePath := "/api/publications/" + awaiting.ID + "/approve?board=default"
	body := map[string]any{"expectedUpdatedAt": awaiting.UpdatedAt}
	response, raw := apiRequest(t, server, http.MethodPost, approvePath, body)
	approved := mapValue(t, raw)
	if response.StatusCode != http.StatusOK || approved["status"] != "pending" ||
		approved["approvedAt"] == nil {
		t.Fatalf("approve publication: status=%d value=%#v", response.StatusCode, approved)
	}
	if _, leaked := approved["claimToken"]; leaked {
		t.Fatalf("approve leaked claim token: %#v", approved)
	}
	duplicate, _ := apiRequest(t, server, http.MethodPost, approvePath, body)
	if duplicate.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate approval status = %d", duplicate.StatusCode)
	}

	rejected := createDashboardPublication(
		t, server, "default", "reject", model.PublicationModeLocalFF, true,
	)
	rejectPath := "/api/publications/" + rejected.ID + "/reject?board=default"
	response, raw = apiRequest(t, server, http.MethodPost, rejectPath, map[string]any{
		"expectedUpdatedAt": rejected.UpdatedAt,
		"reason":            "operator rejected this target",
	})
	rejectedValue := mapValue(t, raw)
	if response.StatusCode != http.StatusOK ||
		rejectedValue["status"] != "superseded" ||
		rejectedValue["error"] != "operator rejected this target" {
		t.Fatalf("reject publication: status=%d value=%#v", response.StatusCode, raw)
	}

	failed := failDashboardPublication(t, server, "default", createDashboardPublication(
		t, server, "default", "retry", model.PublicationModePullRequest, false,
	))
	retryPath := "/api/publications/" + failed.ID + "/retry?board=default"
	response, raw = apiRequest(t, server, http.MethodPost, retryPath, map[string]any{
		"expectedUpdatedAt": failed.UpdatedAt,
	})
	if response.StatusCode != http.StatusOK ||
		mapValue(t, raw)["status"] != "pending" ||
		mapValue(t, raw)["error"] != nil {
		t.Fatalf("retry publication: status=%d value=%#v", response.StatusCode, raw)
	}

	manual := createDashboardPublication(
		t, server, "default", "complete", model.PublicationModeManual, false,
	)
	completePath := "/api/publications/" + manual.ID + "/complete?board=default"
	response, raw = apiRequest(t, server, http.MethodPost, completePath, map[string]any{
		"expectedUpdatedAt": manual.UpdatedAt,
		"url":               "https://example.test/releases/manual",
	})
	completed := mapValue(t, raw)
	if response.StatusCode != http.StatusOK ||
		completed["status"] != "published" ||
		completed["url"] != "https://example.test/releases/manual" {
		t.Fatalf("complete publication: status=%d value=%#v", response.StatusCode, raw)
	}
}

func TestPublicationsAPIRejectsUnsafeMutationInputsAndWrongBoard(t *testing.T) {
	server := startTestServer(t)
	ctx := context.Background()
	if _, err := server.manager.Create(ctx, "other", boards.Update{}); err != nil {
		t.Fatal(err)
	}
	value := createDashboardPublication(
		t, server, "default", "strict", model.PublicationModeManual, true,
	)
	path := "/api/publications/" + value.ID + "/approve?board=default"
	for name, body := range map[string]map[string]any{
		"missing CAS": {},
		"claim token": {
			"expectedUpdatedAt": value.UpdatedAt,
			"claimToken":        "never-accepted",
		},
		"unknown field": {
			"expectedUpdatedAt": value.UpdatedAt,
			"unexpected":        true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			response, _ := apiRequest(t, server, http.MethodPost, path, body)
			if response.StatusCode != http.StatusBadRequest {
				t.Fatalf("unsafe input status = %d", response.StatusCode)
			}
		})
	}
	response, _ := apiRequest(
		t,
		server,
		http.MethodPost,
		"/api/publications/"+value.ID+"/approve?board=other",
		map[string]any{"expectedUpdatedAt": value.UpdatedAt},
	)
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-board mutation status = %d", response.StatusCode)
	}
}

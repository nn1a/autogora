package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/nn1a/autogora/internal/operatorrecovery"
	"github.com/nn1a/autogora/internal/store"
)

func activateDashboardRecoverySource(
	t *testing.T,
	server *Server,
) (
	store.AutomationQuarantine,
	store.AutomationQuarantineSource,
) {
	t.Helper()
	authority, err := server.manager.OpenCoordinationStore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer authority.Close()
	gate, activated, err := authority.ActivateAutomationQuarantine(
		context.Background(),
		store.AutomationQuarantineSourceInput{
			Board:             "*",
			Kind:              "dispatcher_session_expired",
			SourceID:          "dashboard-recovery-session",
			ObservedUpdatedAt: "2026-07-24T00:00:00.000000000Z",
			DiagnosticCode:    "session_expired_without_release",
		},
	)
	if err != nil || !activated {
		t.Fatalf(
			"activate dashboard recovery quarantine: gate=%+v activated=%t err=%v",
			gate,
			activated,
			err,
		)
	}
	sources, err := authority.ListAutomationQuarantineSources(
		context.Background(),
		store.AutomationQuarantineSourceFilter{
			ActiveOnly: true,
			Limit:      1000,
		},
	)
	if err != nil || len(sources) != 1 {
		t.Fatalf("dashboard recovery sources=%+v err=%v", sources, err)
	}
	return gate, sources[0]
}

func dashboardRecoveryConfirmation(
	gate store.AutomationQuarantine,
	source store.AutomationQuarantineSource,
) operatorrecovery.Confirmation {
	return operatorrecovery.Confirmation{
		Generation:            gate.Generation,
		Actor:                 "operator@example.test",
		Reason:                "verified helper processes and external writers stopped",
		HelpersStopped:        true,
		ExternalWritesStopped: true,
		Sources: []operatorrecovery.ConfirmationSource{{
			SourceKey:          source.SourceKey,
			Board:              source.Board,
			Kind:               source.Kind,
			SourceID:           source.SourceID,
			ObservedUpdatedAt:  source.ObservedUpdatedAt,
			ObservedClaimEpoch: source.ObservedClaimEpoch,
			DiagnosticCode:     source.DiagnosticCode,
			Disposition:        store.AutomationSourceAbandoned,
		}},
	}
}

func rawDashboardRecoveryRequest(
	t *testing.T,
	server *Server,
	method string,
	path string,
	body string,
	token string,
) (*http.Response, []byte) {
	t.Helper()
	request, err := http.NewRequest(
		method,
		server.URL+path,
		bytes.NewBufferString(body),
	)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	contents, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	return response, contents
}

func TestDashboardOperatorRecoveryStatusConflictConfirmAndReplay(t *testing.T) {
	server := startTestServer(t)
	gate, source := activateDashboardRecoverySource(t, server)

	response, value := apiRequest(
		t,
		server,
		http.MethodGet,
		"/api/recovery/quarantine",
		nil,
	)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("recovery status=%d value=%#v", response.StatusCode, value)
	}
	status := mapValue(t, value)
	sources := arrayValue(t, status["sources"])
	encoded, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	if mapValue(t, status["gate"])["active"] != true ||
		len(sources) != 1 ||
		mapValue(t, sources[0])["sourceKey"] != source.SourceKey ||
		strings.Contains(string(encoded), "claimToken") ||
		strings.Contains(string(encoded), "permitToken") ||
		strings.Contains(string(encoded), server.options.DBPath) {
		t.Fatalf("unsafe or incomplete recovery status: %s", encoded)
	}

	confirmation := dashboardRecoveryConfirmation(gate, source)
	stale := confirmation
	stale.Generation++
	response, value = apiRequest(
		t,
		server,
		http.MethodPost,
		"/api/recovery/quarantine/confirm",
		stale,
	)
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("stale recovery=%d value=%#v", response.StatusCode, value)
	}

	response, value = apiRequest(
		t,
		server,
		http.MethodPost,
		"/api/recovery/quarantine/confirm",
		confirmation,
	)
	result := mapValue(t, value)
	if response.StatusCode != http.StatusOK ||
		result["cleared"] != true ||
		mapValue(t, result["gate"])["active"] != false {
		t.Fatalf("recovery confirmation=%d value=%#v", response.StatusCode, value)
	}

	response, value = apiRequest(
		t,
		server,
		http.MethodPost,
		"/api/recovery/quarantine/confirm",
		confirmation,
	)
	result = mapValue(t, value)
	if response.StatusCode != http.StatusOK ||
		result["cleared"] != false ||
		mapValue(t, result["gate"])["active"] != false {
		t.Fatalf("recovery replay=%d value=%#v", response.StatusCode, value)
	}
}

func TestDashboardOperatorRecoveryRejectsUnsafeTransportInputs(t *testing.T) {
	server := startTestServer(t)
	gate, source := activateDashboardRecoverySource(t, server)
	confirmation := dashboardRecoveryConfirmation(gate, source)
	raw, err := json.Marshal(confirmation)
	if err != nil {
		t.Fatal(err)
	}
	duplicate := strings.Replace(
		string(raw),
		`"generation":`,
		`"generation":`+strconv.FormatInt(gate.Generation, 10)+`,"generation":`,
		1,
	)

	for name, body := range map[string]string{
		"claim token": `{"generation":1,"claimToken":"secret"}`,
		"duplicate":   duplicate,
		"trailing":    string(raw) + `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			response, contents := rawDashboardRecoveryRequest(
				t,
				server,
				http.MethodPost,
				"/api/recovery/quarantine/confirm",
				body,
				testToken,
			)
			if response.StatusCode != http.StatusBadRequest ||
				strings.Contains(string(contents), "secret-publication-token") {
				t.Fatalf(
					"unsafe recovery input status=%d body=%s",
					response.StatusCode,
					contents,
				)
			}
		})
	}

	response, _ := apiRequest(
		t,
		server,
		http.MethodGet,
		"/api/recovery/quarantine?board=default",
		nil,
	)
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("board-scoped recovery status=%d", response.StatusCode)
	}

	response, _ = apiRequest(
		t,
		server,
		http.MethodPost,
		"/api/recovery/quarantine",
		map[string]any{},
	)
	if response.StatusCode != http.StatusMethodNotAllowed ||
		response.Header.Get("Allow") != http.MethodGet {
		t.Fatalf(
			"recovery status method=%d allow=%q",
			response.StatusCode,
			response.Header.Get("Allow"),
		)
	}
	response, _ = apiRequest(
		t,
		server,
		http.MethodGet,
		"/api/recovery/quarantine/confirm",
		nil,
	)
	if response.StatusCode != http.StatusMethodNotAllowed ||
		response.Header.Get("Allow") != http.MethodPost {
		t.Fatalf(
			"recovery confirm method=%d allow=%q",
			response.StatusCode,
			response.Header.Get("Allow"),
		)
	}

	unauthenticated, _ := rawDashboardRecoveryRequest(
		t,
		server,
		http.MethodGet,
		"/api/recovery/quarantine",
		"",
		"",
	)
	if unauthenticated.StatusCode != http.StatusUnauthorized {
		t.Fatalf(
			"unauthenticated recovery status=%d",
			unauthenticated.StatusCode,
		)
	}
}

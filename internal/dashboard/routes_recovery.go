package dashboard

import (
	"bytes"
	"net/http"
	"strings"

	"github.com/nn1a/autogora/internal/operatorrecovery"
)

const operatorRecoveryBodyLimit = 1024 * 1024

func (s *Server) handleOperatorRecovery(
	response http.ResponseWriter,
	request *http.Request,
	segments []string,
) error {
	if strings.TrimSpace(request.URL.Query().Get("board")) != "" {
		sendJSON(response, http.StatusBadRequest, map[string]any{
			"error": "operator recovery is global and does not accept a board",
		})
		return nil
	}
	service, err := operatorrecovery.New(s.manager)
	if err != nil {
		return err
	}
	if len(segments) == 3 && segments[2] == "quarantine" {
		if request.Method != http.MethodGet {
			response.Header().Set("Allow", http.MethodGet)
			sendJSON(response, http.StatusMethodNotAllowed, map[string]any{
				"error": "recovery quarantine endpoint requires GET",
			})
			return nil
		}
		value, err := service.Status(request.Context())
		if err != nil {
			return err
		}
		sendJSON(response, http.StatusOK, value)
		return nil
	}
	if len(segments) == 4 &&
		segments[2] == "quarantine" &&
		segments[3] == "confirm" {
		if request.Method != http.MethodPost {
			response.Header().Set("Allow", http.MethodPost)
			sendJSON(response, http.StatusMethodNotAllowed, map[string]any{
				"error": "recovery quarantine confirmation requires POST",
			})
			return nil
		}
		raw, err := readBody(request, operatorRecoveryBodyLimit)
		if err != nil {
			return err
		}
		input, err := operatorrecovery.DecodeConfirmation(
			bytes.NewReader(raw),
		)
		if err != nil {
			return err
		}
		value, err := service.Confirm(request.Context(), input)
		if err != nil {
			return err
		}
		sendJSON(response, http.StatusOK, value)
		return nil
	}
	sendJSON(response, http.StatusNotFound, map[string]any{
		"error": "Not found",
	})
	return nil
}

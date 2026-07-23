package dashboard

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"
	"time"
)

type operationRecord struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	Board       string `json:"board"`
	TaskID      string `json:"taskId,omitempty"`
	Status      string `json:"status"`
	Mode        string `json:"mode"`
	AllowWrites bool   `json:"allowWrites"`
	StartedAt   string `json:"startedAt"`
	EndedAt     string `json:"endedAt,omitempty"`
	Error       string `json:"error,omitempty"`
}

func newOperationID() string {
	value := make([]byte, 6)
	if _, err := rand.Read(value); err != nil {
		return time.Now().UTC().Format("20060102T150405.000000000")
	}
	return "op_" + hex.EncodeToString(value)
}

func (s *Server) beginOperation(kind, board, taskID, mode string, allowWrites bool) operationRecord {
	operation := operationRecord{
		ID: newOperationID(), Kind: kind, Board: board, TaskID: taskID,
		Status: "running", Mode: mode, AllowWrites: allowWrites,
		StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	s.operationsMu.Lock()
	s.operations = append([]operationRecord{operation}, s.operations...)
	s.trimOperationsLocked()
	s.operationsMu.Unlock()
	return operation
}

func (s *Server) finishOperation(id string, err error) {
	s.operationsMu.Lock()
	defer s.operationsMu.Unlock()
	for index := range s.operations {
		if s.operations[index].ID != id {
			continue
		}
		s.operations[index].EndedAt = time.Now().UTC().Format(time.RFC3339Nano)
		if err != nil {
			s.operations[index].Status = "failed"
			s.operations[index].Error = err.Error()
		} else {
			s.operations[index].Status = "completed"
		}
		s.trimOperationsLocked()
		return
	}
}

// trimOperationsLocked keeps every in-flight operation observable while
// bounding only completed/failed history. Callers must hold operationsMu.
func (s *Server) trimOperationsLocked() {
	const terminalHistoryLimit = 100
	terminalCount := 0
	kept := s.operations[:0]
	for _, operation := range s.operations {
		if operation.Status == "running" {
			kept = append(kept, operation)
			continue
		}
		if terminalCount < terminalHistoryLimit {
			kept = append(kept, operation)
			terminalCount++
		}
	}
	s.operations = kept
}

func (s *Server) handleOperations(response http.ResponseWriter, request *http.Request, segments []string) error {
	if request.Method != http.MethodGet || len(segments) != 2 {
		response.Header().Set("Allow", http.MethodGet)
		sendJSON(response, http.StatusMethodNotAllowed, map[string]any{"error": "operations endpoint requires GET"})
		return nil
	}
	board := strings.TrimSpace(request.URL.Query().Get("board"))
	s.operationsMu.Lock()
	values := make([]operationRecord, 0, len(s.operations))
	for _, operation := range s.operations {
		if board == "" || operation.Board == board {
			values = append(values, operation)
		}
	}
	s.operationsMu.Unlock()
	sendJSON(response, http.StatusOK, values)
	return nil
}

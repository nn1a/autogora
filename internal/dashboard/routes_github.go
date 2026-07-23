package dashboard

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/nn1a/autogora/internal/githubissues"
	"github.com/nn1a/autogora/internal/store"
)

func splitNonEmpty(value string) []string {
	seen := map[string]bool{}
	result := []string{}
	for _, item := range strings.Split(value, ",") {
		item = strings.TrimSpace(item)
		if item != "" && !seen[item] {
			seen[item] = true
			result = append(result, item)
		}
	}
	return result
}

func importIssueNumbers(value any) ([]int, error) {
	items := []string{}
	switch typed := value.(type) {
	case nil:
		return nil, nil
	case string:
		items = splitNonEmpty(typed)
	case []any:
		for _, item := range typed {
			switch number := item.(type) {
			case float64:
				// JSON numbers decode as float64. Formatting before Atoi
				// rejects fractions, non-finite values, and values outside
				// the current platform's int range without a lossy cast.
				items = append(items, strconv.FormatFloat(number, 'f', -1, 64))
			case string:
				items = append(items, strings.TrimSpace(number))
			default:
				return nil, errors.New("issue numbers must be integers")
			}
		}
	default:
		return nil, errors.New("issue numbers must be a comma-separated string or array")
	}
	result := make([]int, 0, len(items))
	for _, item := range items {
		number, err := strconv.Atoi(item)
		if err != nil || number < 1 {
			return nil, errors.New("issue numbers must be positive integers")
		}
		result = append(result, number)
	}
	return result, nil
}

func (s *Server) handleGitHubImport(response http.ResponseWriter, request *http.Request, segments []string, board string) error {
	if len(segments) != 3 || segments[2] != "import" || request.Method != http.MethodPost {
		response.Header().Set("Allow", http.MethodPost)
		sendJSON(response, http.StatusMethodNotAllowed, map[string]any{"error": "GitHub import endpoint requires POST"})
		return nil
	}
	body, err := readJSON(request)
	if err != nil {
		return err
	}
	numbers, err := importIssueNumbers(body["issues"])
	if err != nil {
		return err
	}
	labels := stringArray(body["labels"])
	if len(labels) == 0 {
		labels = splitNonEmpty(stringValue(body["labels"]))
	}
	metadata, err := s.manager.Read(board)
	if err != nil {
		return err
	}
	directory := ""
	if metadata.DefaultWorkdir != nil {
		directory = *metadata.DefaultWorkdir
	}
	directory, err = githubissues.WorkingDirectory(directory)
	if err != nil {
		return err
	}
	result, importErr := usingStore(request.Context(), s, board, func(opened *store.Store) (githubissues.ImportResult, error) {
		runner := s.options.GitHubRunner
		if runner == nil {
			runner = githubissues.ExecRunner{}
		}
		return (githubissues.Importer{Store: opened, Runner: runner, Directory: directory}).Import(request.Context(), githubissues.ImportOptions{
			Repository: stringValue(body["repository"]), Host: stringValue(body["host"]),
			State: stringValue(body["state"]), Labels: labels, Search: stringValue(body["search"]),
			Limit: intValue(body["limit"], 30), Numbers: numbers, Tenant: optionalString(body, "tenant").Value,
			Priority: intValue(body["priority"], 0), DryRun: boolValue(body["dryRun"], false),
		})
	})
	if importErr != nil {
		if githubissues.IsPartialImportError(importErr) {
			sendJSON(response, http.StatusMultiStatus, result)
			return nil
		}
		return importErr
	}
	sendJSON(response, http.StatusOK, result)
	return nil
}

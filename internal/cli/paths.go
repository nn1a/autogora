package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nn1a/autogora/internal/boards"
	"github.com/nn1a/autogora/internal/projectdata"
)

const initHelp = `autogora init [options]

Initializes the default board at the resolved project data location.

Options:
  --db <path>           Initialize one explicit SQLite path without persisting it
  --data-dir <path>     Persist a project data root; relative paths use the Git root
  --reset-data-dir      Return this project to the native user-data default

Use --data-dir .autogora for an ignored repository-local installation.
Data directories inside .git are rejected.
`

const pathsHelp = `autogora paths [options]

Shows the project identity and resolved database, board, attachment, log, and
workspace paths without creating them.

Options:
  --db <path>           Inspect one explicit SQLite path
  --board <slug>        Show paths for a specific existing board
`

type pathReport struct {
	Project         projectdata.Project `json:"project"`
	Source          string              `json:"source"`
	AppDataRoot     string              `json:"appDataRoot"`
	DataRoot        string              `json:"dataRoot"`
	Database        string              `json:"database"`
	Board           string              `json:"board"`
	BoardDatabase   string              `json:"boardDatabase"`
	BoardsRoot      string              `json:"boardsRoot"`
	AttachmentsRoot string              `json:"attachmentsRoot"`
	LogsRoot        string              `json:"logsRoot"`
	WorkspacesRoot  string              `json:"workspacesRoot"`
}

func (a *App) projectDataOptions() (projectdata.Options, error) {
	cwd, err := a.workingDirectory()
	if err != nil {
		return projectdata.Options{}, err
	}
	return projectdata.Options{WorkingDirectory: cwd, Getenv: a.Getenv}, nil
}

func (a *App) databasePath(explicit string) (string, error) {
	if strings.TrimSpace(explicit) != "" {
		return a.absoluteWorkingPath(explicit)
	}
	if environment := a.env("AUTOGORA_DB"); environment != "" {
		return a.absoluteWorkingPath(environment)
	}
	options, err := a.projectDataOptions()
	if err != nil {
		return "", err
	}
	location, err := projectdata.Resolve(options)
	if err != nil {
		return "", err
	}
	return location.DBPath, nil
}

func (a *App) absoluteWorkingPath(value string) (string, error) {
	value = strings.TrimSpace(value)
	if filepath.IsAbs(value) {
		return filepath.Clean(value), nil
	}
	cwd, err := a.workingDirectory()
	if err != nil {
		return "", err
	}
	return filepath.Abs(filepath.Join(cwd, value))
}

func (a *App) initManager(opts options) (*boards.Manager, error) {
	dataDir := strings.TrimSpace(opts.value("data-dir"))
	reset := opts.flags["reset-data-dir"]
	if dataDir != "" && reset {
		return nil, errors.New("--data-dir and --reset-data-dir cannot be used together")
	}
	if dataDir == "" && !reset {
		return a.managerFor(opts.value("db"))
	}
	if opts.value("db") != "" || a.env("AUTOGORA_DB") != "" {
		return nil, errors.New("--data-dir and --reset-data-dir cannot be combined with --db or AUTOGORA_DB")
	}
	options, err := a.projectDataOptions()
	if err != nil {
		return nil, err
	}
	var location projectdata.Location
	if reset {
		location, err = projectdata.Reset(options)
	} else {
		location, err = projectdata.Configure(options, dataDir)
	}
	if err != nil {
		return nil, err
	}
	if err := ensureProjectLocalDataIgnored(location); err != nil {
		return nil, err
	}
	return boards.NewManager(location.DBPath)
}

func ensureProjectLocalDataIgnored(location projectdata.Location) error {
	relative, err := filepath.Rel(location.Project.Root, location.DataRoot)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return nil
	}
	if err := os.MkdirAll(location.DataRoot, 0o700); err != nil {
		return err
	}
	ignorePath := filepath.Join(location.DataRoot, ".gitignore")
	if _, err := os.Stat(ignorePath); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.WriteFile(ignorePath, []byte("*\n"), 0o600); err != nil {
		return fmt.Errorf("protect project-local data from Git: %w", err)
	}
	return nil
}

func (a *App) runPaths(opts options) error {
	options, err := a.projectDataOptions()
	if err != nil {
		return err
	}
	location, err := projectdata.Resolve(options)
	if err != nil {
		return err
	}
	database, source := location.DBPath, location.Source
	if explicit := strings.TrimSpace(opts.value("db")); explicit != "" {
		database, err = a.absoluteWorkingPath(explicit)
		source = "option"
	} else if environment := a.env("AUTOGORA_DB"); environment != "" {
		database, err = a.absoluteWorkingPath(environment)
		source = "environment"
	}
	if err != nil {
		return err
	}
	manager, err := boards.NewManager(database)
	if err != nil {
		return err
	}
	board := strings.TrimSpace(a.board(opts))
	if board == "" {
		board = manager.Current()
	} else {
		board, err = manager.Resolve(board)
		if err != nil {
			return err
		}
	}
	metadata, err := manager.Read(board)
	if err != nil {
		return err
	}
	dataRoot := filepath.Dir(database)
	return writeJSON(a.Stdout, pathReport{
		Project: location.Project, Source: source, AppDataRoot: location.AppDataRoot,
		DataRoot: dataRoot, Database: database, Board: metadata.Slug,
		BoardDatabase: metadata.DBPath, BoardsRoot: filepath.Join(dataRoot, "boards"),
		AttachmentsRoot: metadata.AttachmentsRoot, LogsRoot: metadata.LogsRoot,
		WorkspacesRoot: metadata.WorkspaceRoot,
	})
}

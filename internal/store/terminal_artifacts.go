package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/nn1a/autogora/internal/model"
)

type terminalArtifactIdentity struct {
	RunID       string `json:"runId"`
	RequestedAt string `json:"requestedAt"`
	Index       int    `json:"index"`
	Path        string `json:"path"`
}

// terminalArtifactID binds a captured file to one immutable terminal request
// and one position in that request. A failed finalization can therefore reuse
// the same database key and filesystem destination without accumulating
// visible attachments or abandoned files on every retry.
func terminalArtifactID(runID, requestedAt string, index int, artifact string) string {
	normalized := filepath.ToSlash(filepath.Clean(strings.TrimSpace(artifact)))
	encoded, _ := json.Marshal(terminalArtifactIdentity{
		RunID:       runID,
		RequestedAt: requestedAt,
		Index:       index,
		Path:        normalized,
	})
	digest := sha256.Sum256(encoded)
	return "a_terminal_" + hex.EncodeToString(digest[:])
}

func terminalArtifactWorkspace(task model.Task, workspacePath string) string {
	if strings.TrimSpace(workspacePath) != "" {
		return workspacePath
	}
	if task.Workspace == nil {
		return ""
	}
	return strings.TrimPrefix(
		strings.TrimPrefix(*task.Workspace, "dir:"),
		"worktree:",
	)
}

func (s *Store) captureTerminalArtifacts(
	ctx context.Context,
	tx *sql.Tx,
	task model.Task,
	run model.Run,
	request model.TerminalRequest,
	workspacePath string,
) ([]model.Attachment, error) {
	artifacts := normalizeSkills(request.Artifacts)
	if len(artifacts) == 0 {
		return nil, nil
	}
	workspacePath = terminalArtifactWorkspace(task, workspacePath)
	captured := make([]model.Attachment, 0, len(artifacts))
	for index, artifact := range artifacts {
		source := artifact
		if !filepath.IsAbs(source) {
			source = filepath.Join(workspacePath, source)
		}
		attachment, err := s.captureTerminalArtifact(
			ctx,
			tx,
			task.ID,
			run.ID,
			request.RequestedAt,
			index,
			artifact,
			source,
		)
		if err != nil {
			return nil, err
		}
		captured = append(captured, attachment)
	}
	return captured, nil
}

func (s *Store) captureTerminalArtifact(
	ctx context.Context,
	tx *sql.Tx,
	taskID, runID, requestedAt string,
	index int,
	artifact, sourcePath string,
) (model.Attachment, error) {
	id := terminalArtifactID(runID, requestedAt, index, artifact)
	source, err := filepath.Abs(sourcePath)
	if err != nil {
		return model.Attachment{}, err
	}
	name, err := cleanAttachmentName(source)
	if err != nil {
		return model.Attachment{}, err
	}
	directory := filepath.Join(s.attachmentsRoot, taskID)
	target := filepath.Join(directory, id)

	existing, err := scanAttachment(tx.QueryRowContext(
		ctx,
		`SELECT id, task_id, kind, name, media_type, size, sha256, path, url, created_at
		FROM task_attachments WHERE id = ?`,
		id,
	))
	switch {
	case err == nil:
		if existing.TaskID != taskID ||
			existing.Kind != "file" ||
			existing.Name != name ||
			existing.Path == nil ||
			*existing.Path != target {
			return model.Attachment{}, fmt.Errorf(
				"terminal artifact identity collision for %s",
				id,
			)
		}
		if info, statErr := os.Stat(target); statErr != nil {
			return model.Attachment{}, fmt.Errorf(
				"terminal artifact %s is not durable: %w",
				id,
				statErr,
			)
		} else if !info.Mode().IsRegular() {
			return model.Attachment{}, fmt.Errorf(
				"terminal artifact %s is not a regular file",
				id,
			)
		}
		return existing, nil
	case !errors.Is(err, sql.ErrNoRows):
		return model.Attachment{}, err
	}

	info, err := os.Stat(source)
	if errors.Is(err, os.ErrNotExist) {
		return model.Attachment{}, fmt.Errorf(
			"attachment file not found: %s",
			source,
		)
	}
	if err != nil {
		return model.Attachment{}, err
	}
	if !info.Mode().IsRegular() {
		return model.Attachment{}, fmt.Errorf(
			"attachment source is not a file: %s",
			source,
		)
	}
	if info.Size() > AttachmentMaxBytes {
		return model.Attachment{}, fmt.Errorf(
			"attachment exceeds the %d byte limit",
			AttachmentMaxBytes,
		)
	}
	missingDirectories, err := terminalArtifactMissingDirectories(directory)
	if err != nil {
		return model.Attachment{}, err
	}
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return model.Attachment{}, err
	}

	// The staging name is deterministic as well. An abrupt process exit can
	// leave at most one staging file per requested artifact, and the next
	// attempt truncates and replaces it.
	staging := filepath.Join(directory, "."+id+".staging")
	output, err := os.OpenFile(
		staging,
		os.O_CREATE|os.O_TRUNC|os.O_WRONLY,
		0o600,
	)
	if err != nil {
		return model.Attachment{}, err
	}
	removeStaging := true
	defer func() {
		if removeStaging {
			_ = os.Remove(staging)
		}
	}()
	if err := output.Chmod(0o600); err != nil {
		_ = output.Close()
		return model.Attachment{}, err
	}
	input, err := os.Open(source)
	if err != nil {
		_ = output.Close()
		return model.Attachment{}, err
	}
	hash := sha256.New()
	written, copyErr := io.Copy(
		io.MultiWriter(output, hash),
		io.LimitReader(input, AttachmentMaxBytes+1),
	)
	closeInputErr := input.Close()
	syncErr := output.Sync()
	closeOutputErr := output.Close()
	if copyErr != nil {
		return model.Attachment{}, copyErr
	}
	if closeInputErr != nil {
		return model.Attachment{}, closeInputErr
	}
	if syncErr != nil {
		return model.Attachment{}, syncErr
	}
	if closeOutputErr != nil {
		return model.Attachment{}, closeOutputErr
	}
	if written > AttachmentMaxBytes {
		return model.Attachment{}, fmt.Errorf(
			"attachment exceeds the %d byte limit",
			AttachmentMaxBytes,
		)
	}
	if err := replaceTerminalArtifactFile(staging, target); err != nil {
		return model.Attachment{}, err
	}
	removeStaging = false
	// SQLite must never commit a terminal attachment row before the renamed
	// file and every newly-created directory entry leading to it are durable.
	// A failed sync leaves only this request-scoped deterministic file; the
	// database transaction still rolls back and a retry safely replaces it.
	if err := syncTerminalArtifactDirectory(directory); err != nil {
		return model.Attachment{}, err
	}
	for _, createdDirectory := range missingDirectories {
		parent := filepath.Dir(createdDirectory)
		if parent == createdDirectory {
			continue
		}
		if err := syncTerminalArtifactDirectory(parent); err != nil {
			return model.Attachment{}, err
		}
	}

	digest := hex.EncodeToString(hash.Sum(nil))
	mediaType := attachmentMediaType(name)
	createdAt := now()
	if _, err := tx.ExecContext(
		ctx,
		`INSERT INTO task_attachments(
			id, task_id, kind, name, media_type, size, sha256, path, url, created_at
		) VALUES (?, ?, 'file', ?, ?, ?, ?, ?, NULL, ?)`,
		id,
		taskID,
		name,
		nullableString(mediaType),
		written,
		digest,
		target,
		createdAt,
	); err != nil {
		return model.Attachment{}, err
	}
	if err := appendEvent(ctx, tx, taskID, "attached", map[string]any{
		"attachmentId": id,
		"kind":         "file",
		"name":         name,
		"size":         written,
	}, nil); err != nil {
		return model.Attachment{}, err
	}
	size := written
	return model.Attachment{
		ID:        id,
		TaskID:    taskID,
		Kind:      "file",
		Name:      name,
		MediaType: mediaType,
		Size:      &size,
		SHA256:    &digest,
		Path:      &target,
		CreatedAt: createdAt,
	}, nil
}

func terminalArtifactMissingDirectories(path string) ([]string, error) {
	missing := make([]string, 0, 2)
	for {
		info, err := os.Stat(path)
		switch {
		case err == nil:
			if !info.IsDir() {
				return nil, fmt.Errorf(
					"terminal artifact directory is not a directory: %s",
					path,
				)
			}
			return missing, nil
		case !errors.Is(err, os.ErrNotExist):
			return nil, err
		}
		missing = append(missing, path)
		parent := filepath.Dir(path)
		if parent == path {
			return nil, fmt.Errorf(
				"terminal artifact directory has no existing ancestor: %s",
				path,
			)
		}
		path = parent
	}
}

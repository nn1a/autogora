package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/nn1a/autogora/internal/model"
)

const AttachmentMaxBytes int64 = 25 * 1024 * 1024

func cleanAttachmentName(value string) (string, error) {
	name := strings.TrimSpace(strings.ReplaceAll(filepath.Base(value), "\x00", ""))
	if name == "" || name == "." || name == ".." {
		return "", errors.New("attachment name cannot be empty")
	}
	if len(name) > 255 {
		name = name[:255]
	}
	return name, nil
}

func attachmentMediaType(name string) *string {
	known := map[string]string{".txt": "text/plain", ".md": "text/markdown", ".json": "application/json",
		".pdf": "application/pdf", ".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg",
		".gif": "image/gif", ".webp": "image/webp", ".csv": "text/csv", ".html": "text/html",
		".xml": "application/xml", ".zip": "application/zip"}
	value := known[strings.ToLower(filepath.Ext(name))]
	if value == "" {
		value = mime.TypeByExtension(strings.ToLower(filepath.Ext(name)))
	}
	if value == "" {
		return nil
	}
	return &value
}

func (s *Store) ListAttachments(ctx context.Context, taskID string) ([]model.Attachment, error) {
	if _, err := requireTask(ctx, s.db, taskID); err != nil {
		return nil, err
	}
	return s.listAttachments(ctx, taskID)
}

func requireAttachmentMutationAllowed(
	ctx context.Context,
	q querier,
	task model.Task,
	completionArtifact bool,
) error {
	if task.Status != model.TaskStatusRunning && task.CurrentRunID == nil {
		return nil
	}
	if completionArtifact && task.Status == model.TaskStatusRunning && task.CurrentRunID != nil {
		request, err := getTerminalRequest(ctx, q, *task.CurrentRunID)
		if err != nil {
			return err
		}
		if request != nil && request.Kind == "complete" && request.FinalizedAt == nil {
			return nil
		}
	}
	return errors.New("cannot change task attachments while running; terminate or complete the active run first")
}

func (s *Store) AttachFile(ctx context.Context, taskID, sourcePath, displayName string) (model.Attachment, error) {
	return s.attachFile(ctx, taskID, sourcePath, displayName, false)
}

func (s *Store) attachFile(
	ctx context.Context,
	taskID, sourcePath, displayName string,
	completionArtifact bool,
) (model.Attachment, error) {
	if _, err := requireTask(ctx, s.db, taskID); err != nil {
		return model.Attachment{}, err
	}
	source, err := filepath.Abs(sourcePath)
	if err != nil {
		return model.Attachment{}, err
	}
	info, err := os.Stat(source)
	if errors.Is(err, os.ErrNotExist) {
		return model.Attachment{}, fmt.Errorf("attachment file not found: %s", source)
	}
	if err != nil {
		return model.Attachment{}, err
	}
	if !info.Mode().IsRegular() {
		return model.Attachment{}, fmt.Errorf("attachment source is not a file: %s", source)
	}
	if info.Size() > AttachmentMaxBytes {
		return model.Attachment{}, fmt.Errorf("attachment exceeds the %d byte limit", AttachmentMaxBytes)
	}
	if displayName == "" {
		displayName = source
	}
	name, err := cleanAttachmentName(displayName)
	if err != nil {
		return model.Attachment{}, err
	}
	id := newID("a")
	directory := filepath.Join(s.attachmentsRoot, taskID)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return model.Attachment{}, err
	}
	target := filepath.Join(directory, id+"-"+name)
	temporary, err := os.CreateTemp(directory, ".attachment-*")
	if err != nil {
		return model.Attachment{}, err
	}
	temporaryName := temporary.Name()
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(temporaryName)
			_ = os.Remove(target)
		}
	}()
	sourceFile, err := os.Open(source)
	if err != nil {
		temporary.Close()
		return model.Attachment{}, err
	}
	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(temporary, hash), io.LimitReader(sourceFile, AttachmentMaxBytes+1))
	closeSourceErr := sourceFile.Close()
	syncErr := temporary.Sync()
	closeTargetErr := temporary.Close()
	if copyErr != nil {
		return model.Attachment{}, copyErr
	}
	if closeSourceErr != nil {
		return model.Attachment{}, closeSourceErr
	}
	if syncErr != nil {
		return model.Attachment{}, syncErr
	}
	if closeTargetErr != nil {
		return model.Attachment{}, closeTargetErr
	}
	if written > AttachmentMaxBytes {
		return model.Attachment{}, fmt.Errorf("attachment exceeds the %d byte limit", AttachmentMaxBytes)
	}
	if err := os.Rename(temporaryName, target); err != nil {
		return model.Attachment{}, err
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	mediaType := attachmentMediaType(name)
	err = s.withWrite(ctx, func(tx *sql.Tx) error {
		task, err := requireTask(ctx, tx, taskID)
		if err != nil {
			return err
		}
		if err := requireAttachmentMutationAllowed(ctx, tx, task, completionArtifact); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO task_attachments(id, task_id, kind, name, media_type, size, sha256, path, url, created_at)
		VALUES (?, ?, 'file', ?, ?, ?, ?, ?, NULL, ?)`, id, taskID, name, nullableString(mediaType), written, digest, target, now()); err != nil {
			return err
		}
		return appendEvent(ctx, tx, taskID, "attached", map[string]any{"attachmentId": id, "kind": "file", "name": name, "size": written}, nil)
	})
	if err != nil {
		return model.Attachment{}, err
	}
	committed = true
	return scanAttachment(s.db.QueryRowContext(ctx, "SELECT id, task_id, kind, name, media_type, size, sha256, path, url, created_at FROM task_attachments WHERE id = ?", id))
}

func (s *Store) AttachURL(ctx context.Context, taskID, rawURL, displayName string) (model.Attachment, error) {
	normalized, name, err := normalizeAttachmentURL(rawURL, displayName)
	if err != nil {
		return model.Attachment{}, err
	}
	if _, err := requireTask(ctx, s.db, taskID); err != nil {
		return model.Attachment{}, err
	}
	id := newID("a")
	err = s.withWrite(ctx, func(tx *sql.Tx) error {
		task, err := requireTask(ctx, tx, taskID)
		if err != nil {
			return err
		}
		if err := requireAttachmentMutationAllowed(ctx, tx, task, false); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO task_attachments(id, task_id, kind, name, media_type, size, sha256, path, url, created_at)
		VALUES (?, ?, 'url', ?, NULL, NULL, NULL, NULL, ?, ?)`, id, taskID, name, normalized, now()); err != nil {
			return err
		}
		return appendEvent(ctx, tx, taskID, "attached", map[string]any{"attachmentId": id, "kind": "url", "name": name, "url": normalized}, nil)
	})
	if err != nil {
		return model.Attachment{}, err
	}
	return scanAttachment(s.db.QueryRowContext(ctx, "SELECT id, task_id, kind, name, media_type, size, sha256, path, url, created_at FROM task_attachments WHERE id = ?", id))
}

func normalizeAttachmentURL(rawURL, displayName string) (string, string, error) {
	parsed, err := url.ParseRequestURI(rawURL)
	if err != nil || parsed.Host == "" {
		return "", "", errors.New("attachment URL must be valid")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", "", errors.New("attachment URL must use http or https")
	}
	if displayName == "" {
		displayName = filepath.Base(parsed.Path)
		if displayName == "." || displayName == "/" || displayName == "" {
			displayName = parsed.Host
		}
	}
	name, err := cleanAttachmentName(displayName)
	if err != nil {
		return "", "", err
	}
	return parsed.String(), name, nil
}

// CreateTaskWithURLSource commits an idempotent external task and its source
// provenance together. A valid result can never expose a task whose source URL
// failed to attach, and an existing task receives at most one matching URL.
func (s *Store) CreateTaskWithURLSource(ctx context.Context, input CreateTaskInput, rawURL, displayName string) (model.TaskDetail, bool, error) {
	normalized, name, err := normalizeAttachmentURL(rawURL, displayName)
	if err != nil {
		return model.TaskDetail{}, false, err
	}
	var taskID string
	created := true
	err = s.withWrite(ctx, func(tx *sql.Tx) error {
		if key := normalizedPointer(input.IdempotencyKey); key != nil {
			err := tx.QueryRowContext(ctx,
				"SELECT id FROM tasks WHERE board = ? AND idempotency_key = ? AND status <> 'archived'",
				orFallback(input.Board, s.board), *key,
			).Scan(&taskID)
			if err == nil {
				created = false
			} else if err != sql.ErrNoRows {
				return err
			}
		}
		if taskID == "" {
			var err error
			taskID, err = s.createTask(ctx, tx, input)
			if err != nil {
				return err
			}
			if len(input.Parents) > 0 {
				if _, err := bumpBoardGraphRevision(ctx, tx, orFallback(input.Board, s.board)); err != nil {
					return err
				}
			}
		}
		var existing int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM task_attachments WHERE task_id = ? AND kind = 'url' AND url = ?", taskID, normalized).Scan(&existing); err != nil {
			return err
		}
		if existing > 0 {
			return nil
		}
		task, err := requireTask(ctx, tx, taskID)
		if err != nil {
			return err
		}
		if err := requireAttachmentMutationAllowed(ctx, tx, task, false); err != nil {
			return err
		}
		id := newID("a")
		if _, err := tx.ExecContext(ctx, `INSERT INTO task_attachments(id, task_id, kind, name, media_type, size, sha256, path, url, created_at)
			VALUES (?, ?, 'url', ?, NULL, NULL, NULL, NULL, ?, ?)`, id, taskID, name, normalized, now()); err != nil {
			return err
		}
		return appendEvent(ctx, tx, taskID, "attached", map[string]any{"attachmentId": id, "kind": "url", "name": name, "url": normalized}, nil)
	})
	if err != nil {
		return model.TaskDetail{}, false, err
	}
	detail, err := s.GetTask(ctx, taskID)
	return detail, created, err
}

func (s *Store) RemoveAttachment(ctx context.Context, taskID, attachmentID string) error {
	attachment, err := scanAttachment(s.db.QueryRowContext(ctx, "SELECT id, task_id, kind, name, media_type, size, sha256, path, url, created_at FROM task_attachments WHERE id = ? AND task_id = ?", attachmentID, taskID))
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("attachment not found: %s", attachmentID)
	}
	if err != nil {
		return err
	}
	err = s.withWrite(ctx, func(tx *sql.Tx) error {
		task, err := requireTask(ctx, tx, taskID)
		if err != nil {
			return err
		}
		if err := requireAttachmentMutationAllowed(ctx, tx, task, false); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM task_attachments WHERE id = ? AND task_id = ?", attachmentID, taskID); err != nil {
			return err
		}
		return appendEvent(ctx, tx, taskID, "attachment_removed", map[string]any{"attachmentId": attachmentID, "name": attachment.Name}, nil)
	})
	if err != nil {
		return err
	}
	if attachment.Path != nil {
		if err := os.Remove(*attachment.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func (s *Store) captureArtifacts(ctx context.Context, task model.Task, artifacts []string) ([]model.Attachment, error) {
	workspace := ""
	if task.Workspace != nil {
		workspace = strings.TrimPrefix(strings.TrimPrefix(*task.Workspace, "dir:"), "worktree:")
	}
	if workspace == "" {
		workspace, _ = os.Getwd()
	}
	return s.captureArtifactsAt(ctx, task, workspace, artifacts)
}

func (s *Store) captureArtifactsAt(ctx context.Context, task model.Task, workspace string, artifacts []string) ([]model.Attachment, error) {
	if len(artifacts) == 0 {
		return nil, nil
	}
	result := []model.Attachment{}
	for _, artifact := range normalizeSkills(artifacts) {
		path := artifact
		if !filepath.IsAbs(path) {
			path = filepath.Join(workspace, path)
		}
		attachment, err := s.attachFile(ctx, task.ID, path, "", true)
		if err != nil {
			return nil, err
		}
		result = append(result, attachment)
	}
	return result, nil
}

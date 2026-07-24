package boards

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/nn1a/autogora/internal/store"
)

func archivedPublicationRecoveryFixture(
	t *testing.T,
) (*Manager, Metadata) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "autogora.db")
	manager, err := NewManager(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := manager.Create(ctx, "default", Update{}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Create(ctx, "archived", Update{}); err != nil {
		t.Fatal(err)
	}
	opened, err := manager.OpenStore(ctx, "archived")
	if err != nil {
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Remove("archived", false); err != nil {
		t.Fatal(err)
	}
	listed, err := manager.ListMetadata(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, metadata := range listed {
		if metadata.Archived && metadata.Slug == "archived" {
			return manager, metadata
		}
	}
	t.Fatal("archived board metadata was not listed")
	return nil, Metadata{}
}

func activePublicationRecoveryFixture(
	t *testing.T,
) (*Manager, Metadata) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "autogora.db")
	manager, err := NewManager(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := manager.Create(ctx, "default", Update{}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Create(ctx, "active", Update{}); err != nil {
		t.Fatal(err)
	}
	listed, err := manager.ListMetadata(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, metadata := range listed {
		if !metadata.Archived && metadata.Slug == "active" {
			return manager, metadata
		}
	}
	t.Fatal("active board metadata was not listed")
	return nil, Metadata{}
}

func TestOpenListedPublicationRecoveryReaderRejectsArchivePathTampering(
	t *testing.T,
) {
	t.Run("outside path", func(t *testing.T) {
		manager, metadata := archivedPublicationRecoveryFixture(t)
		metadata.DBPath = filepath.Join(t.TempDir(), "autogora.db")
		if _, err := manager.OpenListedPublicationRecoveryReader(
			context.Background(),
			metadata,
		); err == nil || !strings.Contains(err.Error(), "outside") {
			t.Fatalf("outside archive path error = %v", err)
		}
	})

	t.Run("malformed metadata", func(t *testing.T) {
		manager, metadata := archivedPublicationRecoveryFixture(t)
		if err := os.WriteFile(
			filepath.Join(filepath.Dir(metadata.DBPath), "board.json"),
			[]byte(`{"slug":`),
			0o600,
		); err != nil {
			t.Fatal(err)
		}
		if _, err := manager.OpenListedPublicationRecoveryReader(
			context.Background(),
			metadata,
		); err == nil || !strings.Contains(err.Error(), "decode archived board metadata") {
			t.Fatalf("malformed archive metadata error = %v", err)
		}
	})

	t.Run("database symlink", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("symbolic link creation requires platform privileges")
		}
		manager, metadata := archivedPublicationRecoveryFixture(t)
		realDatabase := filepath.Join(t.TempDir(), "archived.db")
		if err := os.Rename(metadata.DBPath, realDatabase); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(realDatabase, metadata.DBPath); err != nil {
			t.Fatal(err)
		}
		if _, err := manager.OpenListedPublicationRecoveryReader(
			context.Background(),
			metadata,
		); err == nil || !strings.Contains(err.Error(), "regular file") {
			t.Fatalf("archive database symlink error = %v", err)
		}
	})
}

func TestPublicationRecoveryInventoryRejectsArchivedDirectorySymlink(
	t *testing.T,
) {
	if runtime.GOOS == "windows" {
		t.Skip("symbolic link creation requires platform privileges")
	}
	manager, _ := archivedPublicationRecoveryFixture(t)
	archivedRoot := filepath.Join(manager.boardsRoot, "_archived")
	if err := os.Symlink(
		t.TempDir(),
		filepath.Join(archivedRoot, "linked-candidate"),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ListMetadata(
		context.Background(),
		true,
	); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("archived directory symlink inventory error = %v", err)
	}
}

func TestActivePublicationRecoveryReaderHoldsLifecycleLock(
	t *testing.T,
) {
	manager, metadata := activePublicationRecoveryFixture(t)
	reader, err := manager.OpenListedPublicationRecoveryReader(
		context.Background(),
		metadata,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Remove("active", false); !errors.Is(
		err,
		store.ErrBoardBusy,
	) {
		reader.Close()
		t.Fatalf("archive while recovery reader is open error = %v", err)
	}
	if !manager.Exists("active") {
		reader.Close()
		t.Fatal("failed archive removed the active board")
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Remove("active", false); err != nil {
		t.Fatalf("archive after recovery reader close: %v", err)
	}
}

func TestOpenListedPublicationRecoveryReaderRejectsChangedActiveBoard(
	t *testing.T,
) {
	t.Run("archived after inventory", func(t *testing.T) {
		manager, metadata := activePublicationRecoveryFixture(t)
		if _, err := manager.Remove("active", false); err != nil {
			t.Fatal(err)
		}
		if _, err := manager.OpenListedPublicationRecoveryReader(
			context.Background(),
			metadata,
		); err == nil {
			t.Fatal("stale active inventory opened an archived board")
		}
		listed, err := manager.ListMetadata(context.Background(), true)
		if err != nil {
			t.Fatal(err)
		}
		for _, current := range listed {
			if !current.Archived || current.Slug != "active" {
				continue
			}
			reader, err := manager.OpenListedPublicationRecoveryReader(
				context.Background(),
				current,
			)
			if err != nil {
				t.Fatal(err)
			}
			if err := reader.Close(); err != nil {
				t.Fatal(err)
			}
			return
		}
		t.Fatal("new recovery inventory did not include archived board")
	})

	t.Run("hard deleted and recreated after inventory", func(t *testing.T) {
		manager, metadata := activePublicationRecoveryFixture(t)
		if _, err := manager.Remove("active", true); err != nil {
			t.Fatal(err)
		}
		if _, err := manager.Create(
			context.Background(),
			"active",
			Update{},
		); err != nil {
			t.Fatal(err)
		}
		if _, err := manager.OpenListedPublicationRecoveryReader(
			context.Background(),
			metadata,
		); err == nil || !strings.Contains(
			err.Error(),
			"changed since recovery inventory",
		) {
			t.Fatalf("stale recreated board inventory error = %v", err)
		}
		listed, err := manager.ListMetadata(context.Background(), false)
		if err != nil {
			t.Fatal(err)
		}
		for _, current := range listed {
			if current.Slug != "active" {
				continue
			}
			reader, err := manager.OpenListedPublicationRecoveryReader(
				context.Background(),
				current,
			)
			if err != nil {
				t.Fatal(err)
			}
			if err := reader.Close(); err != nil {
				t.Fatal(err)
			}
			return
		}
		t.Fatal("new recovery inventory did not include recreated board")
	})
}

func TestOpenListedPublicationRecoveryReaderDoesNotChangeArchiveDatabase(
	t *testing.T,
) {
	manager, metadata := archivedPublicationRecoveryFixture(t)
	before, err := os.Stat(metadata.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := manager.OpenListedPublicationRecoveryReader(
		context.Background(),
		metadata,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := reader.ListPublishingAfter(
		context.Background(),
		"",
	); err != nil {
		reader.Close()
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(metadata.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	if before.Size() != after.Size() ||
		!before.ModTime().Equal(after.ModTime()) {
		t.Fatalf(
			"archived database changed during recovery read: before=%v after=%v",
			before,
			after,
		)
	}
}

func publicationRecoveryWriterInput() store.PublicationRecoveryInput {
	return store.PublicationRecoveryInput{
		SourceKey:          strings.Repeat("a", 64),
		FirstGeneration:    1,
		PublicationID:      "pub_writer_identity",
		ObservedUpdatedAt:  "2026-07-24T01:02:03.000000000Z",
		ObservedClaimEpoch: 1,
		Outcome:            store.PublicationRecoveryFailed,
		Disposition:        store.AutomationSourceAbandoned,
		Actor:              "operator",
		Reason:             "verified writer identity test",
	}
}

func TestApplyListedPublicationRecoveryRejectsChangedBoardIdentity(
	t *testing.T,
) {
	t.Run("active archived after inventory", func(t *testing.T) {
		manager, metadata := activePublicationRecoveryFixture(t)
		if _, err := manager.Remove("active", false); err != nil {
			t.Fatal(err)
		}
		if _, err := manager.ApplyListedPublicationRecovery(
			context.Background(),
			metadata,
			nil,
			publicationRecoveryWriterInput(),
		); err == nil || !strings.Contains(
			err.Error(),
			"changed since operator recovery inventory",
		) {
			t.Fatalf("stale active writer identity error = %v", err)
		}
	})

	t.Run("archived database replaced after inventory", func(t *testing.T) {
		manager, metadata := archivedPublicationRecoveryFixture(t)
		original := metadata.DBPath + ".original"
		if err := os.Rename(metadata.DBPath, original); err != nil {
			t.Fatal(err)
		}
		contents, err := os.ReadFile(original)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(metadata.DBPath, contents, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := manager.ApplyListedPublicationRecovery(
			context.Background(),
			metadata,
			nil,
			publicationRecoveryWriterInput(),
		); err == nil || !strings.Contains(
			err.Error(),
			"database changed since recovery inventory",
		) {
			t.Fatalf("replaced archived writer identity error = %v", err)
		}
	})

	t.Run("archived root replaced after inventory", func(t *testing.T) {
		manager, metadata := archivedPublicationRecoveryFixture(t)
		archiveRoot := filepath.Dir(filepath.Dir(metadata.DBPath))
		originalRoot := archiveRoot + ".original"
		if err := os.Rename(archiveRoot, originalRoot); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(archiveRoot, 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := manager.ApplyListedPublicationRecovery(
			context.Background(),
			metadata,
			nil,
			publicationRecoveryWriterInput(),
		); err == nil || !strings.Contains(
			err.Error(),
			"root changed since recovery inventory",
		) {
			t.Fatalf("replaced archived root identity error = %v", err)
		}
	})
}

func TestPublicationRecoveryRejectsActiveDatabaseSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symbolic link creation requires platform privileges")
	}
	manager, metadata := activePublicationRecoveryFixture(t)
	realDatabase := metadata.DBPath + ".real"
	if err := os.Rename(metadata.DBPath, realDatabase); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realDatabase, metadata.DBPath); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ListMetadata(
		context.Background(),
		false,
	); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("active database symlink inventory error = %v", err)
	}
}

func TestArchivedRecoveryReaderBlocksManagerArchiveMutation(t *testing.T) {
	manager, metadata := archivedPublicationRecoveryFixture(t)
	if _, err := manager.Create(
		context.Background(),
		"next",
		Update{},
	); err != nil {
		t.Fatal(err)
	}
	reader, err := manager.OpenListedPublicationRecoveryReader(
		context.Background(),
		metadata,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Remove("next", false); !errors.Is(
		err,
		ErrBoardMutationInProgress,
	) {
		reader.Close()
		t.Fatalf("archive during archived recovery read error = %v", err)
	}
	if !manager.Exists("next") {
		reader.Close()
		t.Fatal("blocked archive removed the active board")
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Remove("next", false); err != nil {
		t.Fatalf("archive after recovery reader close: %v", err)
	}
}

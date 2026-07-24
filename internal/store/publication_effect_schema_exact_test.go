package store

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

func newPublicationEffectSchemaTestDatabase(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "board.db")
	opened, err := Open(path, "default", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func mutatePublicationEffectSchemaForTest(
	t *testing.T,
	path string,
	statement string,
) {
	t.Helper()
	db, err := sql.Open("sqlite", dataSourceName(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(statement); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func requirePublicationEffectSchemaReopenFailure(
	t *testing.T,
	path string,
	errorFragment string,
) {
	t.Helper()
	reopened, err := Open(path, "default", "")
	if err == nil {
		reopened.Close()
		t.Fatal("tampered publication effect schema unexpectedly opened")
	}
	if !strings.Contains(err.Error(), errorFragment) {
		t.Fatalf(
			"publication effect schema error = %v, want %q",
			err,
			errorFragment,
		)
	}
}

func TestPublicationEffectSQLNormalizationPreservesQuotedTokens(
	t *testing.T,
) {
	expected := `
		CREATE TRIGGER Example
		BEFORE INSERT ON Things
		BEGIN
			SELECT X'ABCD', 'unknown', "QuotedName";
		END
	`
	keywordAndWhitespaceVariant := `create
		trigger example before insert on things begin
		select x'ABCD','unknown',"QuotedName";end`
	if compactPublicationEffectSQL(expected) !=
		compactPublicationEffectSQL(keywordAndWhitespaceVariant) {
		t.Fatal("keyword case or whitespace changed the normalized SQL")
	}
	for _, test := range []struct {
		name  string
		value string
	}{
		{
			name: "string literal case",
			value: `CREATE TRIGGER Example BEFORE INSERT ON Things BEGIN
				SELECT X'ABCD', 'UNKNOWN', "QuotedName"; END`,
		},
		{
			name: "blob literal case",
			value: `CREATE TRIGGER Example BEFORE INSERT ON Things BEGIN
				SELECT X'abcd', 'unknown', "QuotedName"; END`,
		},
		{
			name: "quoted identifier case",
			value: `CREATE TRIGGER Example BEFORE INSERT ON Things BEGIN
				SELECT X'ABCD', 'unknown', "quotedname"; END`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if compactPublicationEffectSQL(expected) ==
				compactPublicationEffectSQL(test.value) {
				t.Fatal("quoted token mutation was normalized away")
			}
		})
	}
}

func TestPublicationEffectSchemaRejectsLiteralCaseRewriteOnReopen(
	t *testing.T,
) {
	path := newPublicationEffectSchemaTestDatabase(t)
	mutatePublicationEffectSchemaForTest(t, path, `
		DROP TRIGGER
			publication_attempt_results_require_unknown_effect_outcome;
		CREATE TRIGGER
			publication_attempt_results_require_unknown_effect_outcome
		BEFORE INSERT ON publication_attempt_results
		WHEN NEW.outcome <> 'UNKNOWN'
			AND EXISTS (
				SELECT 1 FROM publication_effect_intents e
				INNER JOIN publication_effect_results r
					ON r.effect_id = e.id
				WHERE e.attempt_id = NEW.attempt_id
					AND r.outcome = 'UNKNOWN'
			)
		BEGIN
			SELECT RAISE(
				ABORT,
				'publication attempt with an unknown command effect requires an unknown result'
			);
		END;
	`)
	requirePublicationEffectSchemaReopenFailure(
		t,
		path,
		"publication_attempt_results_require_unknown_effect_outcome definition",
	)
}

func TestPublicationEffectSchemaRejectsAdditionalTriggersOnReopen(
	t *testing.T,
) {
	tests := []struct {
		name   string
		table  string
		timing string
	}{
		{
			name:   "effect intent before",
			table:  "publication_effect_intents",
			timing: "BEFORE",
		},
		{
			name:   "effect result before",
			table:  "publication_effect_results",
			timing: "BEFORE",
		},
		{
			name:   "parent result before",
			table:  "publication_attempt_results",
			timing: "BEFORE",
		},
		{
			name:   "effect result after",
			table:  "publication_effect_results",
			timing: "AFTER",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := newPublicationEffectSchemaTestDatabase(t)
			mutatePublicationEffectSchemaForTest(
				t,
				path,
				"CREATE TRIGGER unexpected_publication_effect_trigger "+
					test.timing+" INSERT ON "+test.table+" "+
					"BEGIN SELECT RAISE(IGNORE); END;",
			)
			requirePublicationEffectSchemaReopenFailure(
				t,
				path,
				"publication effect trigger set for "+test.table,
			)
		})
	}
}

func TestPublicationEffectSchemaRejectsAdditionalIndexesOnReopen(
	t *testing.T,
) {
	tests := []struct {
		name      string
		table     string
		column    string
		errorText string
	}{
		{
			name:      "effect intent table",
			table:     "publication_effect_intents",
			column:    "kind",
			errorText: "explicit index set for publication_effect_intents",
		},
		{
			name:      "effect result table",
			table:     "publication_effect_results",
			column:    "outcome",
			errorText: "explicit index set for publication_effect_results",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := newPublicationEffectSchemaTestDatabase(t)
			mutatePublicationEffectSchemaForTest(
				t,
				path,
				"CREATE INDEX unexpected_publication_effect_index ON "+
					test.table+"("+test.column+");",
			)
			requirePublicationEffectSchemaReopenFailure(
				t,
				path,
				test.errorText,
			)
		})
	}

	t.Run("owned index namespace", func(t *testing.T) {
		path := newPublicationEffectSchemaTestDatabase(t)
		mutatePublicationEffectSchemaForTest(t, path, `
			CREATE INDEX idx_publication_effect_unexpected
			ON tasks(id);
		`)
		requirePublicationEffectSchemaReopenFailure(
			t,
			path,
			"publication effect owned index set",
		)
	})
}

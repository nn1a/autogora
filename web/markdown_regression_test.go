package webui

import (
	"strings"
	"testing"
)

func TestDashboardUsesEscapedDependencyFreeMarkdownForCardsAndDetails(t *testing.T) {
	javascript, err := Files.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	source := string(javascript)

	for _, marker := range []string{
		`function renderMarkdownText(value = "")`,
		`let safe = escapeHtml(source.slice(cursor, urlStart))`,
		`const safeURL = escapeHtml(url)`,
		`href="${safeURL}" target="_blank" rel="noopener noreferrer"`,
		`function renderInlineMarkdown(value = "")`,
		"return `<code>${escapeHtml(part.slice(1, -1))}</code>`",
		`return renderMarkdownText(part)`,
		`function compactMarkdown(value = "")`,
		`function markdown(value = "", options = {})`,
		`if (options.compact) return compactMarkdown(value)`,
		"output.push(`<pre><code>${escapeHtml(code.join(\"\\n\"))}</code></pre>`)",
		`${markdown(task.body.trim(), { compact: true })}`,
		`class="markdown comment-body"`,
		`class="markdown">${markdown(task.result)}`,
	} {
		if !strings.Contains(source, marker) {
			t.Fatalf("safe Markdown marker %q is missing", marker)
		}
	}

	if strings.Contains(source, `<div class="card-summary">${escapeHtml(task.body.trim())}</div>`) {
		t.Fatal("task cards still render their Markdown source as escaped plain text")
	}
}

func TestDashboardUsesReadableAccessibleCommentPresentation(t *testing.T) {
	javascript, err := Files.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	styles, err := Files.ReadFile("styles.css")
	if err != nil {
		t.Fatal(err)
	}
	javascriptSource := string(javascript)
	styleSource := string(styles)

	for _, marker := range []string{
		`<label class="sr-only" for="comment-body">Comment</label>`,
		`<textarea id="comment-body" name="comment" rows="3" required`,
		`const input = $("#comment-body", event.currentTarget)`,
		`class="card-indicator" title="Comments" aria-label="`,
		`class="card-indicator" title="Relationships" aria-label="`,
	} {
		if !strings.Contains(javascriptSource, marker) {
			t.Fatalf("accessible task content marker %q is missing", marker)
		}
	}
	for _, marker := range []string{
		`.card-summary {`,
		`font-size: 13px;`,
		`-webkit-line-clamp: 3;`,
		`.markdown { overflow-wrap: anywhere; color: var(--text-subtle); font-size: 14px; line-height: 1.65; }`,
		`.comment-row { font-size: 14px; line-height: 1.55; }`,
		`.comment-form textarea { min-height: 82px; line-height: 1.55; }`,
	} {
		if !strings.Contains(styleSource, marker) {
			t.Fatalf("readable task content CSS marker %q is missing", marker)
		}
	}
}

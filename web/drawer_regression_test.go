package webui

import (
	"strings"
	"testing"
)

func TestTaskDrawerUsesNativeModalWithoutLegacyScrimState(t *testing.T) {
	html, err := Files.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	javascript, err := Files.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}

	htmlSource := string(html)
	javascriptSource := string(javascript)
	const drawerOpen = `<dialog id="drawer" class="drawer" aria-label="Task details">`
	if !strings.Contains(htmlSource, drawerOpen) {
		t.Fatal("task drawer is not a native, labelled dialog")
	}
	drawerStart := strings.Index(htmlSource, drawerOpen)
	drawerEnd := strings.Index(htmlSource[drawerStart:], "</dialog>")
	if drawerEnd < 0 {
		t.Fatal("task drawer dialog is not closed")
	}
	drawerMarkup := htmlSource[drawerStart : drawerStart+drawerEnd]
	for _, obsolete := range []string{
		`<aside id="drawer"`,
		`id="scrim"`,
		`aria-hidden="true"`,
		`aria-modal="true"`,
	} {
		if strings.Contains(drawerMarkup, obsolete) || (obsolete == `id="scrim"` && strings.Contains(htmlSource, obsolete)) {
			t.Fatalf("task drawer still contains legacy modal marker %q", obsolete)
		}
	}
	for _, marker := range []string{
		`if (!drawer.open) drawer.showModal()`,
		`if (drawer.open) drawer.close()`,
		`$("#drawer").addEventListener("cancel"`,
	} {
		if !strings.Contains(javascriptSource, marker) {
			t.Fatalf("native task drawer behavior marker %q is missing", marker)
		}
	}
	for _, obsolete := range []string{`$("#scrim")`, `.setAttribute("aria-hidden"`, `.removeAttribute("aria-hidden"`} {
		if strings.Contains(javascriptSource, obsolete) {
			t.Fatalf("task drawer JavaScript still contains legacy modal state %q", obsolete)
		}
	}
}

func TestTaskDrawerBoundsAndWrapsLongContent(t *testing.T) {
	styles, err := Files.ReadFile("styles.css")
	if err != nil {
		t.Fatal(err)
	}
	source := string(styles)

	for _, marker := range []string{
		`.drawer::backdrop`,
		`.drawer-content > *, .drawer-grid > *, .drawer-title-block, .markdown { min-width: 0; max-width: 100%; }`,
		`.drawer-content h1 { margin: 4px 0 0; overflow-wrap: anywhere;`,
		`.markdown { overflow-wrap: anywhere;`,
		`.markdown a, .detail-row a { overflow-wrap: anywhere;`,
	} {
		if !strings.Contains(source, marker) {
			t.Fatalf("long-content drawer CSS marker %q is missing", marker)
		}
	}
}

func TestTaskDrawerRefreshCannotOverwriteNewDirtyEdits(t *testing.T) {
	javascript, err := Files.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	source := string(javascript)

	for _, marker := range []string{
		`drawerDirty: false, drawerVersion: null, drawerRequest: 0`,
		`const requestID = ++state.drawerRequest`,
		`if (requestID !== state.drawerRequest) return`,
		`if (!force && state.drawerTask === taskId && state.drawerDirty)`,
		`$("#drawer-refresh").classList.remove("hidden")`,
		`state.drawerRequest++`,
	} {
		if !strings.Contains(source, marker) {
			t.Fatalf("drawer refresh generation marker %q is missing", marker)
		}
	}

	fetchIndex := strings.Index(source, "const detail = await api")
	generationIndex := strings.Index(source, "if (requestID !== state.drawerRequest) return")
	dirtyIndex := strings.Index(source[generationIndex:], "if (!force && state.drawerTask === taskId && state.drawerDirty)")
	resetIndex := strings.Index(source, "state.drawerDirty = false")
	renderIndex := strings.Index(source, "renderDrawer(detail)")
	dirtyIndex += generationIndex
	if fetchIndex < 0 || generationIndex <= fetchIndex || dirtyIndex <= generationIndex ||
		resetIndex <= dirtyIndex || renderIndex <= resetIndex {
		t.Fatal("drawer fetch must recheck request generation and dirty state before rendering")
	}
}

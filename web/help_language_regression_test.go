package webui

import (
	"strings"
	"testing"
)

func TestDashboardUsesOnePersistentLanguageForDescriptiveHelp(t *testing.T) {
	html := dashboardAsset(t, "index.html")
	javascript := dashboardAsset(t, "app.js")

	for _, marker := range []string{
		`id="help-language"`,
		`id="automation-help-language"`,
		`id="swarm-help-title"`,
		`id="swarm-help-description"`,
		`id="stage-focus-help"`,
		`id="board-view-help"`,
		`>All stages</button>`,
		`>Planning</button>`,
		`>Execution</button>`,
		`>Overview</button>`,
		`>Compact</button>`,
		`>Flow</button>`,
		`>Graph</button>`,
	} {
		if !strings.Contains(html, marker) {
			t.Fatalf("shared help language HTML marker %q is missing", marker)
		}
	}

	for _, marker := range []string{
		`const DASHBOARD_HELP =`,
		`function updateHelpLanguageButtons()`,
		`["#help-language", "#automation-help-language"]`,
		`function updateDashboardHelp()`,
		`swarmButton.title = help.swarm.buttonTitle`,
		`$("#swarm-button-help").textContent = help.swarm.buttonDescription`,
		`$("#stage-focus-help").textContent = help.focus.description`,
		`button.title = help.focus.titles[button.dataset.stageFocus]`,
		`$("#board-view-help").textContent = help.view.description`,
		`button.title = help.view.titles[button.dataset.boardView]`,
		`$("#help-language").addEventListener("click", toggleAutomationHelpLanguage)`,
		`$("#automation-help-language").addEventListener("click", toggleAutomationHelpLanguage)`,
		`localStorage.setItem("autogora.automationHelpLanguage", state.automationHelpLanguage)`,
		`initializeSelects(); updateDashboardHelp(); bindGlobalActions()`,
		`Swarm은 병렬 worker 뒤에 verifier와 synthesizer를 실행하는 고급 DAG를 만듭니다.`,
		`Focus는 보드를 workflow stage 단위로 좁혀 보여줍니다.`,
		`Graph는 작업 의존성과 계층을 다이어그램으로 보여줍니다.`,
	} {
		if !strings.Contains(javascript, marker) {
			t.Fatalf("shared help language behavior marker %q is missing", marker)
		}
	}
}

func TestAgentDialogHeaderWrapsControlsOnNarrowScreens(t *testing.T) {
	html := dashboardAsset(t, "index.html")
	styles := dashboardAsset(t, "styles.css")

	for _, marker := range []string{
		`<header class="agents-header">`,
		`<div class="agents-heading">`,
		`id="agents-reload"`,
		`aria-label="Close coding agent setup"`,
	} {
		if !strings.Contains(html, marker) {
			t.Fatalf("responsive agent dialog HTML marker %q is missing", marker)
		}
	}

	mobileStart := strings.Index(styles, "@media (max-width: 600px)")
	if mobileStart < 0 {
		t.Fatal("mobile style block is missing")
	}
	mobileStyles := styles[mobileStart:]
	for _, marker := range []string{
		`.agents-header { align-items: flex-start !important; flex-wrap: wrap; }`,
		`.agents-heading { width: 100%; flex-basis: 100%; }`,
		`.agents-header .section-actions { display: flex; min-width: 0; max-width: 100%; }`,
		`.agents-header #agents-reload { min-width: 0; flex: 1 1 auto; }`,
		`.agents-header .icon-button { flex: 0 0 40px; }`,
	} {
		if !strings.Contains(mobileStyles, marker) {
			t.Fatalf("responsive agent dialog style %q is missing", marker)
		}
	}
}

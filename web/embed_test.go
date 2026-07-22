package webui

import "testing"

func TestDashboardAssetsAreEmbedded(t *testing.T) {
	for _, name := range []string{"index.html", "app.js", "styles.css"} {
		contents, err := Files.ReadFile(name)
		if err != nil {
			t.Fatalf("read embedded %s: %v", name, err)
		}
		if len(contents) == 0 {
			t.Fatalf("embedded %s is empty", name)
		}
	}
}

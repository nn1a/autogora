package skills

import "testing"

func TestPortableSkillsAreEmbedded(t *testing.T) {
	for _, name := range Names {
		for _, path := range []string{name + "/SKILL.md", name + "/agents/openai.yaml"} {
			contents, err := Files.ReadFile(path)
			if err != nil {
				t.Fatalf("read embedded %s: %v", path, err)
			}
			if len(contents) == 0 {
				t.Fatalf("embedded %s is empty", path)
			}
		}
	}
}

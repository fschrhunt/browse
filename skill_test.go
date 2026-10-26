package main

import (
	"os"
	"path/filepath"
	"testing"
)

// Adding the skill writes browse/SKILL.md into the folder and records the
// folder once for uninstall, however often it is added.
func TestAddSkillRecordsFolderOnce(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", "")

	for range 2 {
		if err := addSkill("~/.someagent/skills"); err != nil {
			t.Fatal(err)
		}
	}
	dir := filepath.Join(home, ".someagent/skills/browse")
	if got, _ := os.ReadFile(filepath.Join(dir, "SKILL.md")); string(got) != string(skillText) {
		t.Error("SKILL.md was not written")
	}
	if list, _ := os.ReadFile(filepath.Join(home, ".local/share/browse/skills")); string(list) != dir+"\n" {
		t.Errorf("recorded %q", list)
	}
}

// Installing the agent skill: SKILL.md, built into the binary, copied into
// agents' skills folders as browse/SKILL.md. Every copy is recorded, so
// scripts/uninstall.sh removes exactly what was added.
package main

import (
	"bufio"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/term"
)

//go:embed SKILL.md
var skillText []byte

// agentSkillDirs are the user-wide skills folders of the agents browse knows,
// each used when its agent's own folder exists. ~/.agents/skills is the
// folder agents share.
var agentSkillDirs = []struct{ agent, skills string }{
	{".claude", ".claude/skills"},
	{".codex", ".codex/skills"},
	{".agents", ".agents/skills"},
	{".config/opencode", ".config/opencode/skills"},
	{".gemini", ".gemini/skills"},
	{".cursor", ".cursor/skills"},
	{".copilot", ".copilot/skills"},
	{".factory", ".factory/skills"},
	{".pi", ".pi/agent/skills"},
}

// installSkill adds the skill to folder, or with no folder to every agent
// found, then asks in a terminal for more folders.
func installSkill(folder string) error {
	if folder != "" {
		return addSkill(folder)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	for _, d := range agentSkillDirs {
		if _, err := os.Stat(filepath.Join(home, d.agent)); err == nil {
			if err := addSkill(filepath.Join(home, d.skills)); err != nil {
				return err
			}
		}
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return nil
	}
	in := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("Another agent's skills folder to add browse to (Enter to finish): ")
		if !in.Scan() || strings.TrimSpace(in.Text()) == "" {
			return in.Err()
		}
		if err := addSkill(strings.TrimSpace(in.Text())); err != nil {
			fmt.Println(err)
		}
	}
}

// addSkill writes folder/browse/SKILL.md and records it for uninstall.
func addSkill(folder string) error {
	if rest, ok := strings.CutPrefix(folder, "~/"); ok {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		folder = filepath.Join(home, rest)
	}
	dir, err := filepath.Abs(filepath.Join(folder, "browse"))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), skillText, 0o644); err != nil {
		return err
	}
	if err := recordSkill(dir); err != nil {
		return err
	}
	fmt.Println("added", filepath.Join(dir, "SKILL.md"))
	return nil
}

// recordSkill adds dir to ~/.local/share/browse/skills, the list of skill
// folders scripts/uninstall.sh deletes.
func recordSkill(dir string) error {
	data, err := xdgDir("XDG_DATA_HOME", ".local/share")
	if err != nil {
		return err
	}
	list := filepath.Join(data, "skills")
	old, err := os.ReadFile(list)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if slices.Contains(strings.Split(string(old), "\n"), dir) {
		return nil
	}
	if err := os.MkdirAll(data, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(list, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintln(f, dir)
	return err
}

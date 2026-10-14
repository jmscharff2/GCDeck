package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"

	tea "github.com/charmbracelet/bubbletea"
)

func inHerdr() bool { return os.Getenv("HERDR_ENV") == "1" && os.Getenv("HERDR_PANE_ID") != "" }

// execMsg asks Update to hand the terminal to command, for when there is no Herdr to open a pane in.
type execMsg struct {
	label, dir, command string
}

// openPane runs command in a new Herdr pane below this one, or in this terminal outside Herdr.
// focus moves the cursor into the new pane, for interactive things like a console.
func openPane(label, dir, command string, focus bool) tea.Cmd {
	return func() tea.Msg {
		if !inHerdr() {
			return execMsg{label: label, dir: dir, command: command}
		}
		args := []string{"pane", "split", "--current", "--direction", "down", "--cwd", dir}
		if !focus {
			args = append(args, "--no-focus")
		}
		out, err := run("herdr", args...)
		if err != nil {
			return actionMsg{err: fmt.Errorf("herdr split: %w", err)}
		}
		var resp struct {
			Result struct {
				Pane struct {
					ID string `json:"pane_id"`
				} `json:"pane"`
			} `json:"result"`
		}
		if err := json.Unmarshal(out, &resp); err != nil || resp.Result.Pane.ID == "" {
			return actionMsg{err: fmt.Errorf("herdr split: unexpected reply %q", out)}
		}
		id := resp.Result.Pane.ID
		if _, err := run("herdr", "pane", "run", id, command); err != nil {
			return actionMsg{err: fmt.Errorf("herdr run: %w", err)}
		}
		return actionMsg{text: fmt.Sprintf("%s → pane %s", label, id)}
	}
}

func execHere(m execMsg) tea.Cmd {
	c := exec.Command("sh", "-c", m.command)
	c.Dir = m.dir
	return tea.ExecProcess(c, func(err error) tea.Msg {
		if err != nil {
			return actionMsg{err: fmt.Errorf("%s: %w", m.label, err), refresh: true}
		}
		return actionMsg{text: m.label + " finished", refresh: true}
	})
}

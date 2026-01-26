package mpv

import (
	"fmt"
	"os/exec"
	"syscall"
)

// Start spawns mpv and returns the started *exec.Cmd. Caller may kill or Wait on it.
func Start(url string, title string, device string, resample bool) (*exec.Cmd, error) {
	// Start mpv in audio-only mode by default for a terminal music player.
	// Use --really-quiet to suppress all terminal output that would corrupt TUI.
	// Use --no-terminal to prevent mpv from trying to read/write the terminal.
	args := []string{
		"--no-video",
		"--no-terminal",
		"--really-quiet",
	}
	if device != "" {
		args = append(args, "--audio-device="+device)
	}
	// Append the target URL as the last argument
	args = append(args, url)

	cmd := exec.Command("mpv", args...)
	// Redirect stdout/stderr to null to prevent TUI corruption
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.Stdin = nil
	// ensure mpv does not remain in process group if we kill
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start mpv: %w", err)
	}
	return cmd, nil
}

// KillCmd attempts to kill the mpv process (and its process group) started by Start
func KillCmd(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	// kill process group
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err == nil {
		_ = syscall.Kill(-pgid, syscall.SIGTERM)
	}
	// fallback kill
	return cmd.Process.Kill()
}

// RunCapture runs mpv and captures combined stdout/stderr; returns output and error.
func RunCapture(url string, title string, device string, resample bool) (string, error) {
	args := []string{"--no-config", "--no-video"}
	if device != "" {
		args = append(args, "--audio-device="+device)
	}
	args = append(args, url)
	cmd := exec.Command("mpv", args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

//go:build windows
// +build windows

package main

import "os/exec"

func setDetachedProcess(cmd *exec.Cmd) {
	// Windows does not support POSIX setsid; daemonize/self-fork is ignored here.
}

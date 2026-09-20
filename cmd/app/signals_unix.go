//go:build !windows

package main

import (
	"fmt"
	"os"
	"strings"
	"syscall"
)

var sighupSignal os.Signal = syscall.SIGHUP

func supportsSIGHUP() bool { return true }

// sighupSelf шлёт SIGHUP запущенному daemon-процессу по pid-файлу.
func sighupSelf() error {
	pidFile := cfgPath + ".pid"
	data, err := os.ReadFile(pidFile)
	if err != nil {
		return fmt.Errorf("cannot read pid file %s (is daemon running?): %w", pidFile, err)
	}
	var pid int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &pid); err != nil {
		return fmt.Errorf("bad pid file %s: %w", pidFile, err)
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	if err := proc.Signal(syscall.SIGHUP); err != nil {
		return fmt.Errorf("send SIGHUP to pid %d: %w", pid, err)
	}
	fmt.Printf("SIGHUP sent to daemon (pid %d)\n", pid)
	return nil
}

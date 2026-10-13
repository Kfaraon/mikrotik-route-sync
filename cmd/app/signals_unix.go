//go:build !windows

package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
)

var sighupSignal os.Signal = syscall.SIGHUP

func supportsSIGHUP() bool { return true }

// processAlive сообщает, существует ли процесс с данным pid. Signal(0) —
// проверка без отправки сигнала; EPERM означает, что процесс есть (чужой).
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	if err == nil {
		return true
	}
	return errors.Is(err, syscall.EPERM)
}

// writePidFile пишет pid текущего процесса. Протухший файл (процесса нет)
// перезаписывается; живой чужой процесс — ошибка (два демона на один конфиг
// затёрли бы pid друг друга). Ошибки чтения/разбора не фатальны.
func writePidFile(path string) error {
	pid := os.Getpid()
	if data, err := os.ReadFile(path); err == nil {
		var old int
		if _, perr := fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &old); perr == nil && old != pid && processAlive(old) {
			return fmt.Errorf("pid file %s: another daemon is running (pid %d)", path, old)
		}
	}
	// O_TRUNC: файл может существовать (stale), но новый pid пишем под 0600.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(f, "%d", pid); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// sighupSelf шлёт SIGHUP запущенному daemon-процессу по pid-файлу.
// Протухший pid (процесса нет) не даёт послать сигнал чужому процессу:
// файл удаляется, возвращается понятная ошибка.
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
	if !processAlive(pid) {
		_ = os.Remove(pidFile)
		return fmt.Errorf("stale pid file %s: process %d is not running", pidFile, pid)
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

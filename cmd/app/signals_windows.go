//go:build windows

package main

import (
	"fmt"
	"os"
)

type sighupPlaceholder struct{}

func (sighupPlaceholder) String() string { return "SIGHUP (unsupported on Windows)" }
func (sighupPlaceholder) Signal()        {}

var sighupSignal os.Signal = sighupPlaceholder{}

func supportsSIGHUP() bool { return false }

func sighupSelf() error {
	return fmt.Errorf("SIGHUP is not supported on Windows")
}

// writePidFile пишет pid текущего процесса (без stale-check: сигналов нет).
func writePidFile(path string) error {
	return os.WriteFile(path, []byte(fmt.Sprint(os.Getpid())), 0o600)
}

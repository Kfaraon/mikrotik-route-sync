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

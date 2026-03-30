//go:build !windows

package main

import "syscall"

// detachedSysProcAttr returns a SysProcAttr that starts the process in its
// own session (Setsid), detaching it from the parent's terminal.
func detachedSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

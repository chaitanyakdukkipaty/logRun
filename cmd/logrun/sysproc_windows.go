//go:build windows

package main

import "syscall"

// detachedSysProcAttr returns a SysProcAttr for Windows.
// Windows has no Setsid; CREATE_NEW_PROCESS_GROUP is the closest equivalent.
func detachedSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: 0x00000200} // CREATE_NEW_PROCESS_GROUP
}

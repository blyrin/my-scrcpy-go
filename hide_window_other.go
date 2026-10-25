//go:build !windows

package main

import "syscall"

func hideWindow() *syscall.SysProcAttr {
	return nil
}

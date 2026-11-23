//go:build !windows

package main

import (
	"os"
	"syscall"
)

// restartSelf 用新程序原地替换当前进程（同一个终端窗口里继续跑）
func restartSelf(exePath string) error {
	return syscall.Exec(exePath, append([]string{exePath}, os.Args[1:]...), os.Environ())
}

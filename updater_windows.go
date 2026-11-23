//go:build windows

package main

import (
	"os"
	"os/exec"
)

// restartSelf 启动新程序（共用同一个黑色控制台窗口），然后当前进程退出
func restartSelf(exePath string) error {
	cmd := exec.Command(exePath, os.Args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	os.Exit(0)
	return nil
}

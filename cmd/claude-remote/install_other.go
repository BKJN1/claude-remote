//go:build !windows

package main

import (
	"fmt"
	"os"
)

func installCmd(args []string) int {
	fmt.Fprintln(os.Stderr, "Автозапуск через Task Scheduler поддерживается только в Windows.")
	return 1
}

func uninstallCmd() int { return installCmd(nil) }

func taskCheck() check {
	return check{warn: true, name: "Автозапуск", note: "поддерживается только в Windows"}
}

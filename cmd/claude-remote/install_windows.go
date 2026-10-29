//go:build windows

package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"unicode/utf16"

	"golang.org/x/sys/windows"
)

const taskName = "claude-remote"

// xmlEscape is enough for the values we put into the task definition.
func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;").Replace(s)
}

// utf16le returns s as UTF-16LE with a BOM, which schtasks /XML requires.
func utf16le(s string) []byte {
	u := utf16.Encode([]rune(s))
	b := make([]byte, 2, 2+2*len(u))
	b[0], b[1] = 0xFF, 0xFE
	for _, c := range u {
		b = append(b, byte(c), byte(c>>8))
	}
	return b
}

// installCmd registers a Task Scheduler job that starts the server when the
// current user logs on. An XML definition is used instead of `schtasks /TR`
// because /TR is limited to 261 characters, which a few --project flags exceed.
func installCmd(args []string) int {
	set, o := newFlagSet("install")
	if err := set.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if len(o.projects) == 0 && o.workdir == "" {
		fmt.Fprintln(os.Stderr, "Ошибка: укажите хотя бы один проект, например:\n  claude-remote install --project мой-проект=C:\\Users\\you\\projects\\my-project")
		return 2
	}
	// Run the same checks as `doctor` first so mistakes surface now, not at logon.
	problems := 0
	for _, c := range runChecks(o) {
		if !c.ok && !c.warn && c.name != "Tailscale" { // Tailscale may start after logon; the task retries
			c.print()
			problems++
		}
	}
	if problems > 0 {
		fmt.Fprintln(os.Stderr, "\nАвтозапуск не создан: сначала исправьте проблемы выше.")
		return 1
	}
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Ошибка:", err)
		return 1
	}
	u, err := user.Current()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Ошибка:", err)
		return 1
	}
	// The flags are passed through unchanged (already validated above).
	cmdArgs := windows.ComposeCommandLine(args)
	xml := `<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo><Description>claude-remote: remote terminal over Tailscale</Description></RegistrationInfo>
  <Triggers>
    <LogonTrigger><Enabled>true</Enabled><UserId>` + xmlEscape(u.Username) + `</UserId><Delay>PT30S</Delay></LogonTrigger>
  </Triggers>
  <Principals>
    <Principal id="Author"><UserId>` + xmlEscape(u.Username) + `</UserId><LogonType>InteractiveToken</LogonType><RunLevel>LeastPrivilege</RunLevel></Principal>
  </Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <RestartOnFailure><Interval>PT1M</Interval><Count>5</Count></RestartOnFailure>
  </Settings>
  <Actions Context="Author">
    <Exec><Command>` + xmlEscape(exe) + `</Command><Arguments>` + xmlEscape(cmdArgs) + `</Arguments><WorkingDirectory>` + xmlEscape(filepath.Dir(exe)) + `</WorkingDirectory></Exec>
  </Actions>
</Task>`
	f, err := os.CreateTemp("", "claude-remote-task-*.xml")
	if err != nil {
		fmt.Fprintln(os.Stderr, "Ошибка:", err)
		return 1
	}
	defer os.Remove(f.Name())
	f.Write(utf16le(xml))
	f.Close()

	if out, err := exec.Command("schtasks", "/Create", "/TN", taskName, "/XML", f.Name(), "/F").CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "Не удалось создать задачу: %v\n%s", err, out)
		return 1
	}
	fmt.Println("Автозапуск создан: сервер стартует через 30 секунд после входа в Windows.")
	fmt.Println("Запустить сейчас:  schtasks /Run /TN " + taskName)
	fmt.Println("Убрать:            claude-remote uninstall")
	return 0
}

func uninstallCmd() int {
	out, err := exec.Command("schtasks", "/Delete", "/TN", taskName, "/F").CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Не удалось удалить задачу (возможно, её нет): %v\n%s", err, out)
		return 1
	}
	fmt.Println("Автозапуск удалён. Если сервер сейчас работает, остановите его: schtasks /End /TN " + taskName)
	return 0
}

func taskCheck() check {
	if err := exec.Command("schtasks", "/Query", "/TN", taskName).Run(); err != nil {
		return check{warn: true, name: "Автозапуск не настроен", note: "создать: claude-remote install --project имя=путь"}
	}
	return check{ok: true, name: "Автозапуск настроен (задача " + taskName + ")"}
}

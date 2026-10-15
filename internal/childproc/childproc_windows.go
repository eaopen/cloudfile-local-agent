//go:build windows

// Package childproc 统一处理「本程序启动的子进程不要弹控制台窗口」。
//
// 本程序多数时候由 Chrome 作为 native messaging host 启动，宿主没有控制台。
// 此时用 exec.Command 启动一个**控制台程序**，Windows 会给子进程新开一个控制台
// 窗口——用户看到的就是一闪而过的黑框。实测一次 App Paths 探测能弹 48 个。
//
// 这里只设 CREATE_NO_WINDOW，不用 HideWindow/SW_HIDE：后者会把子进程**主窗口**
// 的初始显示状态也设成隐藏，而我们恰恰要用它打开 Word/CAD 这类 GUI 程序。
// CREATE_NO_WINDOW 只影响控制台，对 GUI 程序完全无效，所以可以无差别地加。
package childproc

import (
	"os/exec"
	"syscall"
)

// createNoWindow：子进程不创建控制台窗口。
// 与 CREATE_NEW_CONSOLE(0x00000010)、DETACHED_PROCESS(0x00000008) 互斥且优先级更低，
// 所以已经用 detachedProcess 启动的后台进程（internal/launch）不受影响。
const createNoWindow = 0x08000000

// Hide 让 command 在启动后没有控制台窗口。可以安全地在任何启动点调用。
func Hide(command *exec.Cmd) {
	if command == nil {
		return
	}
	if command.SysProcAttr == nil {
		command.SysProcAttr = &syscall.SysProcAttr{}
	}
	command.SysProcAttr.CreationFlags |= createNoWindow
}

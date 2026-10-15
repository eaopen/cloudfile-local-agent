//go:build !windows

// 非 Windows 平台上没有「控制台窗口」这回事，Hide 是空操作。
package childproc

import "os/exec"

// Hide 在所有非 Windows 平台上是空操作。
func Hide(command *exec.Cmd) {}

//go:build !windows

// 非 Windows 上这个包只是为了让 go build ./... 能过：注册表是 Windows 概念，
// 真正使用它的文件（internal/appfinder/apps_windows.go）本身带 windows 构建标签。
package winreg

import "errors"

// Hive 是注册表根键；非 Windows 上没有任何合法值。
type Hive uintptr

const (
	CurrentUser  Hive = 0
	LocalMachine Hive = 0
)

// ErrUnsupported 表示当前平台没有注册表。
var ErrUnsupported = errors.New("winreg: only available on Windows")

// ReadString 在非 Windows 上总是失败。
func ReadString(Hive, string, string) (string, error) {
	return "", ErrUnsupported
}

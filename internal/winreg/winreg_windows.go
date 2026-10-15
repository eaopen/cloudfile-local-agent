//go:build windows

// Package winreg reads a few registry values in-process.
//
// 为什么不用 `reg.exe query`：那是**控制台程序**。本程序通常由 Chrome 作为
// native messaging host 启动，宿主没有控制台，于是每次 CreateProcess 都会给
// 子进程新开一个控制台黑框一闪而过。App Paths 探测一共要查 16 个应用 × 3 个键
// = 48 次，用户看到的就是「啪啪啪弹一堆黑窗口」，而且每条 native message 都会
// 重来一遍（宿主是每条消息一个新进程），单次耗时 0.7~1.6 秒。
//
// 直接调 advapi32 读，既没有窗口也没有进程开销。刻意不引 golang.org/x/sys：
// 本仓库保持零第三方依赖，用 syscall.LazyDLL 就够。
package winreg

import (
	"errors"
	"syscall"
	"unsafe"
)

// Hive 是注册表根键的句柄值（Windows 预定义键，不需要打开/关闭）。
type Hive uintptr

const (
	CurrentUser  Hive = 0x80000001 // HKEY_CURRENT_USER
	LocalMachine Hive = 0x80000002 // HKEY_LOCAL_MACHINE
)

const (
	keyRead       = 0x20019 // KEY_READ
	regSZ         = 1
	regExpandSZ   = 2
	errorSuccess  = 0
	errorMoreData = 234
	maxValueBytes = 64 << 10 // 单个值最多 64KB，异常数据直接拒绝
)

var (
	advapi32             = syscall.NewLazyDLL("advapi32.dll")
	kernel32             = syscall.NewLazyDLL("kernel32.dll")
	procRegOpenKeyExW    = advapi32.NewProc("RegOpenKeyExW")
	procRegQueryValueExW = advapi32.NewProc("RegQueryValueExW")
	procRegCloseKey      = advapi32.NewProc("RegCloseKey")
	procExpandEnvStrings = kernel32.NewProc("ExpandEnvironmentStringsW")
)

// ErrUnsupportedType 表示该值不是字符串（REG_SZ / REG_EXPAND_SZ）。
var ErrUnsupportedType = errors.New("winreg: value is not a string")

// ReadString 读取 hive\subkey 下名为 name 的字符串值（name 传空串表示默认值）。
// REG_EXPAND_SZ 会就地展开 %VAR%（这正是它被标记为 EXPAND 的含义）；
// 键或值不存在、类型不是字符串，都返回错误。
func ReadString(hive Hive, subkey, name string) (string, error) {
	subkeyPtr, err := syscall.UTF16PtrFromString(subkey)
	if err != nil {
		return "", err
	}
	var key uintptr
	status, _, _ := procRegOpenKeyExW.Call(uintptr(hive), uintptr(unsafe.Pointer(subkeyPtr)),
		0, keyRead, uintptr(unsafe.Pointer(&key)))
	if status != errorSuccess {
		return "", syscall.Errno(status)
	}
	defer procRegCloseKey.Call(key)

	namePtr, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return "", err
	}
	var kind, size uint32
	// 第一次调用只问长度：lpData 传 0、lpcbData 传一个变量。
	status, _, _ = procRegQueryValueExW.Call(key, uintptr(unsafe.Pointer(namePtr)), 0,
		uintptr(unsafe.Pointer(&kind)), 0, uintptr(unsafe.Pointer(&size)))
	if status != errorSuccess && status != errorMoreData {
		return "", syscall.Errno(status)
	}
	if kind != regSZ && kind != regExpandSZ {
		return "", ErrUnsupportedType
	}
	if size == 0 || size > maxValueBytes {
		return "", errors.New("winreg: implausible value size")
	}

	// 长度按字节给，转成 UTF-16 元素个数要上取整，并多留一个位置给结尾的 0。
	buffer := make([]uint16, size/2+1)
	status, _, _ = procRegQueryValueExW.Call(key, uintptr(unsafe.Pointer(namePtr)), 0,
		uintptr(unsafe.Pointer(&kind)), uintptr(unsafe.Pointer(&buffer[0])),
		uintptr(unsafe.Pointer(&size)))
	if status != errorSuccess {
		return "", syscall.Errno(status)
	}

	value := syscall.UTF16ToString(buffer)
	if kind == regExpandSZ {
		value = expand(value)
	}
	return value, nil
}

// expand 调 ExpandEnvironmentStringsW 展开 %VAR%。失败时原样返回，
// 让调用方按「路径不存在」处理，而不是拿到一个坏路径。
func expand(value string) string {
	source, err := syscall.UTF16PtrFromString(value)
	if err != nil {
		return value
	}
	needed, _, _ := procExpandEnvStrings.Call(uintptr(unsafe.Pointer(source)), 0, 0)
	if needed == 0 || needed > maxValueBytes {
		return value
	}
	buffer := make([]uint16, needed)
	written, _, _ := procExpandEnvStrings.Call(uintptr(unsafe.Pointer(source)),
		uintptr(unsafe.Pointer(&buffer[0])), uintptr(needed))
	if written == 0 || written > needed {
		return value
	}
	return syscall.UTF16ToString(buffer)
}

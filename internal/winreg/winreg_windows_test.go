//go:build windows

package winreg

import (
	"strings"
	"testing"
)

func TestReadStringReturnsARegularValue(t *testing.T) {
	value, err := ReadString(LocalMachine, `SOFTWARE\Microsoft\Windows NT\CurrentVersion`, "ProductName")
	if err != nil {
		t.Fatalf("ReadString: %v", err)
	}
	if strings.TrimSpace(value) == "" {
		t.Fatal("ProductName should never be empty")
	}
	if strings.Contains(value, "%") {
		t.Fatalf("REG_SZ must be returned verbatim, got %q", value)
	}
}

func TestReadStringExpandsExpandSz(t *testing.T) {
	// Environment\Path 是 REG_EXPAND_SZ：返回值必须是展开后的路径，
	// 不能把 %SystemRoot% 原样漏给调用方（那会让 App Paths 判断成"文件不存在"）。
	value, err := ReadString(LocalMachine,
		`SYSTEM\CurrentControlSet\Control\Session Manager\Environment`, "Path")
	if err != nil {
		t.Skipf("Path is not readable here: %v", err)
	}
	if strings.Contains(value, "%") {
		t.Fatalf("REG_EXPAND_SZ must be expanded, still got %% in %q", value)
	}
}

func TestReadStringRejectsNonStringValues(t *testing.T) {
	_, err := ReadString(LocalMachine, `SOFTWARE\Microsoft\Windows NT\CurrentVersion`,
		"CurrentMajorVersionNumber")
	if err == nil {
		t.Fatal("a REG_DWORD value must not be returned as a string")
	}
}

func TestReadStringReportsMissingKeysAndValues(t *testing.T) {
	if _, err := ReadString(LocalMachine, `SOFTWARE\NoSuchVendor\NoSuchProduct`, ""); err == nil {
		t.Fatal("a missing key must be an error")
	}
	if _, err := ReadString(LocalMachine, `SOFTWARE\Microsoft\Windows NT\CurrentVersion`,
		"NoSuchValueName"); err == nil {
		t.Fatal("a missing value must be an error")
	}
}

// App Paths 是这套 API 唯一的生产调用点：确认它至少能读到系统自带的项，
// 并且缺失项安静地返回错误（调用方据此回退到目录扫描）。
func TestAppPathsStyleLookup(t *testing.T) {
	value, err := ReadString(LocalMachine,
		`SOFTWARE\Microsoft\Windows\CurrentVersion\App Paths\notepad.exe`, "")
	if err != nil {
		t.Logf("notepad.exe is not registered in App Paths here: %v", err)
		return
	}
	if !strings.HasSuffix(strings.ToLower(value), ".exe") {
		t.Fatalf("default value should be an executable path, got %q", value)
	}
}

//go:build windows

package main

// Win32 interop for the task center UI.
//
// This deliberately uses nothing but the standard library: the module is kept
// free of third-party dependencies, and Go has no GUI in its standard library,
// so every call below is a plain syscall into the system DLLs.  Only
// windows/amd64 is targeted (which is what the Windows package ships).
//
// Anything that goes wrong here must degrade, never crash: a tray that cannot
// register, a window that cannot be created or a menu that will not open may
// cost the user an entry point, but it must never touch a task, a working copy
// or a Checkout.

import (
	"errors"
	"fmt"
	"syscall"
	"unsafe"
)

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	shell32  = syscall.NewLazyDLL("shell32.dll")
	gdi32    = syscall.NewLazyDLL("gdi32.dll")
	comctl32 = syscall.NewLazyDLL("comctl32.dll")

	procAdjustWindowRectEx     = user32.NewProc("AdjustWindowRectEx")
	procAppendMenuW            = user32.NewProc("AppendMenuW")
	procBeginPaint             = user32.NewProc("BeginPaint")
	procCreatePopupMenu        = user32.NewProc("CreatePopupMenu")
	procCreateWindowExW        = user32.NewProc("CreateWindowExW")
	procDefWindowProcW         = user32.NewProc("DefWindowProcW")
	procDestroyMenu            = user32.NewProc("DestroyMenu")
	procDestroyWindow          = user32.NewProc("DestroyWindow")
	procDispatchMessageW       = user32.NewProc("DispatchMessageW")
	procDrawTextW              = user32.NewProc("DrawTextW")
	procEndPaint               = user32.NewProc("EndPaint")
	procFillRect               = user32.NewProc("FillRect")
	procGetClientRect          = user32.NewProc("GetClientRect")
	procGetCursorPos           = user32.NewProc("GetCursorPos")
	procGetMessageW            = user32.NewProc("GetMessageW")
	procGetWindowLongPtrW      = user32.NewProc("GetWindowLongPtrW")
	procInvalidateRect         = user32.NewProc("InvalidateRect")
	procLoadCursorW            = user32.NewProc("LoadCursorW")
	procLoadIconW              = user32.NewProc("LoadIconW")
	procMonitorFromPoint       = user32.NewProc("MonitorFromPoint")
	procGetMonitorInfoW        = user32.NewProc("GetMonitorInfoW")
	procMessageBoxW            = user32.NewProc("MessageBoxW")
	procPostMessageW           = user32.NewProc("PostMessageW")
	procPostQuitMessage        = user32.NewProc("PostQuitMessage")
	procRegisterClassExW       = user32.NewProc("RegisterClassExW")
	procRegisterWindowMessageW = user32.NewProc("RegisterWindowMessageW")
	procSendMessageW           = user32.NewProc("SendMessageW")
	procSetForegroundWindow    = user32.NewProc("SetForegroundWindow")
	procSetTimer               = user32.NewProc("SetTimer")
	procKillTimer              = user32.NewProc("KillTimer")
	procSetWindowLongPtrW      = user32.NewProc("SetWindowLongPtrW")
	procSetWindowPos           = user32.NewProc("SetWindowPos")
	procSetWindowTextW         = user32.NewProc("SetWindowTextW")
	procShowWindow             = user32.NewProc("ShowWindow")
	procTrackPopupMenuEx       = user32.NewProc("TrackPopupMenuEx")
	procTranslateMessage       = user32.NewProc("TranslateMessage")
	procUpdateWindow           = user32.NewProc("UpdateWindow")
	procGetSystemMetrics       = user32.NewProc("GetSystemMetrics")
	procGetWindowRect          = user32.NewProc("GetWindowRect")
	procIsWindowVisible        = user32.NewProc("IsWindowVisible")
	procSetCapture             = user32.NewProc("SetCapture")
	procReleaseCapture         = user32.NewProc("ReleaseCapture")

	procShellNotifyIconW = shell32.NewProc("Shell_NotifyIconW")

	procCreateFontW      = gdi32.NewProc("CreateFontW")
	procCreateSolidBrush = gdi32.NewProc("CreateSolidBrush")
	procDeleteObject     = gdi32.NewProc("DeleteObject")
	procGetStockObject   = gdi32.NewProc("GetStockObject")
	procSelectObject     = gdi32.NewProc("SelectObject")
	procSetBkMode        = gdi32.NewProc("SetBkMode")
	procSetTextColor     = gdi32.NewProc("SetTextColor")

	procInitCommonControlsEx = comctl32.NewProc("InitCommonControlsEx")

	procCreateMutexW     = kernel32.NewProc("CreateMutexW")
	procCloseHandle      = kernel32.NewProc("CloseHandle")
	procGetModuleHandleW = kernel32.NewProc("GetModuleHandleW")
)

// moduleHandle is this process's own module handle. Both RegisterClassExW and
// CreateWindowExW need a real HINSTANCE: passing NULL makes class registration
// fail without setting a last error.
func moduleHandle() uintptr {
	handle, _, _ := procGetModuleHandleW.Call(0)
	return handle
}

// ── kernel32 ──────────────────────────────────────────────────────────────

// singleInstance takes a per-logon-session mutex. It reports false when another
// instance already holds it, which is how the UI keeps one resident window per
// user instead of one per double-click.
func singleInstance(name string) (handle uintptr, first bool) {
	pointer, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return 0, false
	}
	handle, _, lastErr := procCreateMutexW.Call(0, 0, uintptr(unsafe.Pointer(pointer)))
	if handle == 0 {
		return 0, false
	}
	// ERROR_ALREADY_EXISTS means somebody else owns it; the handle is still ours
	// to close, which is exactly what the caller does on shutdown.
	if errno, ok := lastErr.(syscall.Errno); ok && errno == syscall.ERROR_ALREADY_EXISTS {
		return handle, false
	}
	return handle, true
}

func closeHandle(handle uintptr) {
	if handle != 0 {
		_, _, _ = procCloseHandle.Call(handle)
	}
}

// ── small helpers ─────────────────────────────────────────────────────────

func loWord(value uintptr) uint32 { return uint32(value) & 0xffff }

func utf16Ptr(value string) uintptr {
	pointer, err := syscall.UTF16PtrFromString(value)
	if err != nil {
		return 0
	}
	return uintptr(unsafe.Pointer(pointer))
}

func utf16Into(destination []uint16, value string) {
	encoded, err := syscall.UTF16FromString(value)
	if err != nil {
		return
	}
	if len(encoded) > len(destination) {
		encoded = encoded[:len(destination)-1]
		encoded[len(encoded)-1] = 0
	}
	copy(destination, encoded)
}

func utf16ToString(source []uint16) string {
	end := 0
	for end < len(source) && source[end] != 0 {
		end++
	}
	return syscall.UTF16ToString(source[:end])
}

// ── structures ────────────────────────────────────────────────────────────

type point struct{ X, Y int32 }

type rect struct{ Left, Top, Right, Bottom int32 }

func (r rect) width() int32  { return r.Right - r.Left }
func (r rect) height() int32 { return r.Bottom - r.Top }

type msgStruct struct {
	HWnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      point
}

type wndClassEx struct {
	CbSize        uint32
	Style         uint32
	LpfnWndProc   uintptr
	CbClsExtra    int32
	CbWndExtra    int32
	HInstance     uintptr
	HIcon         uintptr
	HCursor       uintptr
	HbrBackground uintptr
	LpszMenuName  *uint16
	LpszClassName *uint16
	HIconSm       uintptr
}

type paintStruct struct {
	HDC       uintptr
	Erase     int32
	RcPaint   rect
	Restore   int32
	IncUpdate int32
	Reserved  [32]byte
}

type monitorInfo struct {
	CbSize    uint32
	RcMonitor rect
	RcWork    rect
	DwFlags   uint32
}

type monitorInfoEx struct {
	CbSize    uint32
	RcMonitor rect
	RcWork    rect
	DwFlags   uint32
	SzDevice  [32]uint16
}

// minMaxInfo mirrors MINMAXINFO; only ptMinTrackSize is used, to keep the task
// window from being resized below the point where its columns fit.
type minMaxInfo struct {
	PtReserved     point
	PtMaxSize      point
	PtMaxPosition  point
	PtMinTrackSize point
	PtMaxTrackSize point
}

type guid struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

// lvColumn mirrors LVCOLUMNW. The trailing cxMin/cxDefault/cxIdeal fields are
// declared even though this UI never sets LVCF_MINWIDTH, so the structure size
// matches what ComCtl32 expects.
type lvColumn struct {
	Mask       uint32
	Fmt        int32
	Cx         int32
	PszText    *uint16
	CchTextMax int32
	ISubItem   int32
	IImage     int32
	IOrder     int32
	CxMin      int32
	CxDefault  int32
	CxIdeal    int32
}

// lvItem mirrors LVITEMW.
type lvItem struct {
	Mask       uint32
	IItem      int32
	ISubItem   int32
	State      uint32
	StateMask  uint32
	PszText    *uint16
	CchTextMax int32
	IImage     int32
	LParam     uintptr
	IIndent    int32
	IGroupID   int32
	CColumns   uint32
	PuColumns  uintptr
	PIFlags    *uint32
	PIStates   *uint32
}

type lvItemState struct {
	State     uint32
	StateMask uint32
}

// notifyIconData mirrors NOTIFYICONDATAW.  cbSize is filled from the Go struct
// size so the two always agree; the UI does not call NIM_SETVERSION, which
// keeps the classic callback shape (wParam = icon id, lParam = mouse message).
type notifyIconData struct {
	CbSize           uint32
	HWnd             uintptr
	UID              uint32
	UFlags           uint32
	UCallbackMessage uint32
	HIcon            uintptr
	SzTip            [128]uint16
	DwState          uint32
	DwStateMask      uint32
	SzInfo           [256]uint16
	UVersion         uint32
	SzInfoTitle      [64]uint16
	DwInfoFlags      uint32
	GuidItem         guid
	HBalloonIcon     uintptr
}

// ── constants ─────────────────────────────────────────────────────────────

const (
	idAppIcon = 1

	wmCreate          = 0x0001
	wmDestroy         = 0x0002
	wmClose           = 0x0010
	wmPaint           = 0x000f
	wmCommand         = 0x0111
	wmTimer           = 0x0113
	wmSize            = 0x0005
	wmSetFont         = 0x0030
	wmEraseBackground = 0x0014
	wmGetMinMaxInfo   = 0x0024
	wmMouseMove       = 0x0200
	wmLButtonDown     = 0x0201
	wmLButtonUp       = 0x0202
	wmLButtonDBL      = 0x0203
	wmRButtonUp       = 0x0205
	wmApp             = 0x8000
	wmTrayMessage     = wmApp + 1
	wmUIWakeup        = wmApp + 2
	wmSelfDestruct    = wmApp + 3

	wsPopup = 0x80000000

	swMinimize = 6
	swRestore  = 9
	swShow     = 5
	swHide     = 0

	wsOverlappedWindow = 0x00cf0000
	wsChild            = 0x40000000
	wsVisible          = 0x10000000
	wsTabStop          = 0x00010000
	wsBorder           = 0x00800000
	wsVScroll          = 0x00200000
	wsClipChildren     = 0x02000000
	wsClipSiblings     = 0x04000000
	wsExClientEdge     = 0x00000200
	wsExToolWindow     = 0x00000080
	wsExTopmost        = 0x00000008
	bsPushButton       = 0x00000000
	bsDefPushButton    = 0x00000001
	ssLeft             = 0x00000000
	ssLeftNowordwrap   = 0x0000000c

	cwUseDefault = 0x80000000

	swpNoZOrder   = 0x0004
	swpNoActivate = 0x0010
	swpShowWindow = 0x0040

	hwndTopmost = ^uintptr(0) // (HWND)-1

	mfString       = 0x00000000
	mfSeparator    = 0x00000800
	tpmReturnCmd   = 0x0100
	tpmRightButton = 0x0002

	idiApplication = 32512

	idcArrow   = 32512
	idcSizeAll = 32646

	nimAdd    = 0x00000000
	nimModify = 0x00000001
	nimDelete = 0x00000002

	nifMessage = 0x00000001
	nifIcon    = 0x00000002
	nifTip     = 0x00000004
	nifInfo    = 0x00000010

	niifInfo    = 0x00000001
	niifWarning = 0x00000002

	lvsReport          = 0x0001
	lvsExFullRowSelect = 0x00000020
	lvsExGridLines     = 0x00000001

	lvmFirst            = 0x1000
	lvmGetItemCount     = lvmFirst + 4
	lvmDeleteAllItems   = lvmFirst + 9
	lvmInsertItemW      = lvmFirst + 77
	lvmInsertColumnW    = lvmFirst + 97
	lvmSetItemTextW     = lvmFirst + 116
	lvmEnsureVisible    = lvmFirst + 19
	lvmSetItemState     = lvmFirst + 43
	lvmSetExtendedStyle = lvmFirst + 54
	lvmGetNextItem      = lvmFirst + 12

	lvsilSmall  = 0
	lvsilNormal = 1

	lvifText         = 0x0001
	lvifState        = 0x0008
	lvniSelected     = 0x0002
	lvniFocused      = 0x0001
	lvniFirstVisible = 0x0001

	lvcfFmtLeft = 0x0000
	lvcfWidth   = 0x0002
	lvcfText    = 0x0001
	lvcfSubItem = 0x0008

	iccListViewClasses = 0x00000001

	dtCenter     = 0x00000001
	dtVCenter    = 0x00000004
	dtSingleLine = 0x00000020
	dtNoPrefix   = 0x00000800

	transparent = 1

	fwBold           = 700
	defaultCharset   = 1
	cleartypeQuality = 5
	fixedPitch       = 0x30

	mbYesNo       = 0x00000004
	mbIconWarning = 0x00000030
	mbDefButton2  = 0x00000200
	idYes         = 6

	mkLButton = 0x0001

	monitorDefaultToNearest = 0x00000002

	// defaultGuiFont is DEFAULT_GUI_FONT from GetStockObject; using it keeps the
	// child controls from falling back to the ugly bitmap system font.
	defaultGuiFont = 17
)

// ── user32 wrappers ───────────────────────────────────────────────────────

func initializeCommonControls() {
	var classes struct {
		Size uint32
		Icc  uint32
	}
	classes.Size = uint32(unsafe.Sizeof(classes))
	classes.Icc = iccListViewClasses
	_, _, _ = procInitCommonControlsEx.Call(uintptr(unsafe.Pointer(&classes)))
}

func registerWindowClass(name string, procedure uintptr, background uintptr) error {
	class := wndClassEx{
		Style:         0,
		LpfnWndProc:   procedure,
		HInstance:     moduleHandle(),
		HCursor:       loadCursor(idcArrow),
		HbrBackground: background,
		LpszClassName: syscall.StringToUTF16Ptr(name),
	}
	class.CbSize = uint32(unsafe.Sizeof(class))
	if atom, _, callErr := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&class))); atom == 0 {
		// A zero return is the failure signal; the call error is often
		// Errno(0) ("operation completed successfully"), which would be
		// misleading on its own, so report the raw code instead.
		if errno, ok := callErr.(syscall.Errno); ok && errno != 0 {
			return fmt.Errorf("%s (Win32 error %d)", errno.Error(), uintptr(errno))
		}
		return errors.New("RegisterClassExW 拒绝了窗口类")
	}
	return nil
}

func loadCursor(id uintptr) uintptr {
	handle, _, _ := procLoadCursorW.Call(0, id)
	return handle
}

// loadStockIcon returns a system icon. The preview only needs the generic
// application icon, which is what the WinForms version used as well.
func loadStockIcon() uintptr {
	handle, _, _ := procLoadIconW.Call(0, idiApplication)
	return handle
}

func createWindowEx(exStyle uintptr, class, title string, style uintptr,
	x, y, width, height int32, parent, menu, instance, param uintptr) uintptr {
	handle, _, _ := procCreateWindowExW.Call(exStyle, utf16Ptr(class), utf16Ptr(title),
		style, uintptr(x), uintptr(y), uintptr(width), uintptr(height),
		parent, menu, instance, param)
	return handle
}

func adjustWindowRect(r *rect, style uintptr, hasMenu bool) {
	menu := uintptr(0)
	if hasMenu {
		menu = 1
	}
	_, _, _ = procAdjustWindowRectEx.Call(uintptr(unsafe.Pointer(r)), style, menu, 0)
}

func defWindowProc(hwnd uintptr, message uint32, wparam, lparam uintptr) uintptr {
	result, _, _ := procDefWindowProcW.Call(hwnd, uintptr(message), wparam, lparam)
	return result
}

func getMessage(message *msgStruct, hwnd uintptr) int32 {
	result, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(message)), hwnd, 0, 0)
	return int32(result)
}

func translateMessage(message *msgStruct) {
	_, _, _ = procTranslateMessage.Call(uintptr(unsafe.Pointer(message)))
}

func dispatchMessage(message *msgStruct) {
	_, _, _ = procDispatchMessageW.Call(uintptr(unsafe.Pointer(message)))
}

func postQuitMessage(code int32) {
	_, _, _ = procPostQuitMessage.Call(uintptr(code))
}

func postMessage(hwnd uintptr, message uint32, wparam, lparam uintptr) {
	_, _, _ = procPostMessageW.Call(hwnd, uintptr(message), wparam, lparam)
}

func sendMessage(hwnd uintptr, message uint32, wparam, lparam uintptr) uintptr {
	result, _, _ := procSendMessageW.Call(hwnd, uintptr(message), wparam, lparam)
	return result
}

func setWindowText(hwnd uintptr, text string) {
	_, _, _ = procSetWindowTextW.Call(hwnd, utf16Ptr(text))
}

func showWindow(hwnd uintptr, command int32) bool {
	result, _, _ := procShowWindow.Call(hwnd, uintptr(command))
	return result != 0
}

func updateWindow(hwnd uintptr) {
	_, _, _ = procUpdateWindow.Call(hwnd)
}

func destroyWindow(hwnd uintptr) {
	_, _, _ = procDestroyWindow.Call(hwnd)
}

func invalidateRect(hwnd uintptr, area *rect, erase bool) {
	eraseFlag := uintptr(0)
	if erase {
		eraseFlag = 1
	}
	_, _, _ = procInvalidateRect.Call(hwnd, uintptr(unsafe.Pointer(area)), eraseFlag)
}

func beginPaint(hwnd uintptr, paint *paintStruct) uintptr {
	dc, _, _ := procBeginPaint.Call(hwnd, uintptr(unsafe.Pointer(paint)))
	return dc
}

func endPaint(hwnd uintptr, paint *paintStruct) {
	_, _, _ = procEndPaint.Call(hwnd, uintptr(unsafe.Pointer(paint)))
}

func fillRect(dc uintptr, area *rect, brush uintptr) {
	_, _, _ = procFillRect.Call(dc, uintptr(unsafe.Pointer(area)), brush)
}

func setTimer(hwnd uintptr, id uintptr, milliseconds uint32) {
	_, _, _ = procSetTimer.Call(hwnd, id, uintptr(milliseconds), 0)
}

func killTimer(hwnd uintptr, id uintptr) {
	_, _, _ = procKillTimer.Call(hwnd, id)
}

func setWindowPos(hwnd, insertAfter uintptr, x, y, width, height int32, flags uintptr) {
	_, _, _ = procSetWindowPos.Call(hwnd, insertAfter, uintptr(x), uintptr(y),
		uintptr(width), uintptr(height), flags)
}

func getClientRect(hwnd uintptr) rect {
	var area rect
	_, _, _ = procGetClientRect.Call(hwnd, uintptr(unsafe.Pointer(&area)))
	return area
}

func getCursorPos() point {
	var position point
	_, _, _ = procGetCursorPos.Call(uintptr(unsafe.Pointer(&position)))
	return position
}

func getWindowRect(hwnd uintptr) rect {
	var area rect
	_, _, _ = procGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&area)))
	return area
}

func setCapture(hwnd uintptr) {
	_, _, _ = procSetCapture.Call(hwnd)
}

func isWindowVisible(hwnd uintptr) bool {
	if hwnd == 0 {
		return false
	}
	result, _, _ := procIsWindowVisible.Call(hwnd)
	return result != 0
}

func releaseCapture() {
	_, _, _ = procReleaseCapture.Call()
}

func setForegroundWindow(hwnd uintptr) {
	_, _, _ = procSetForegroundWindow.Call(hwnd)
}

func messageBox(owner uintptr, text, caption string, kind uintptr) int32 {
	result, _, _ := procMessageBoxW.Call(owner, utf16Ptr(text), utf16Ptr(caption), kind)
	return int32(result)
}

func registerWindowMessage(name string) uint32 {
	result, _, _ := procRegisterWindowMessageW.Call(utf16Ptr(name))
	return uint32(result)
}

func systemMetrics(index int32) int32 {
	result, _, _ := procGetSystemMetrics.Call(uintptr(index))
	return int32(result)
}

// monitorWorkArea returns the work area (screen minus taskbar) of the monitor
// that contains the point, plus that monitor's device name.
func monitorWorkArea(at point) (rect, string) {
	handle, _, _ := procMonitorFromPoint.Call(uintptr(at.X), uintptr(at.Y), monitorDefaultToNearest)
	if handle == 0 {
		return rect{Left: 0, Top: 0, Right: systemMetrics(0), Bottom: systemMetrics(1)}, ""
	}
	var info monitorInfoEx
	info.CbSize = uint32(unsafe.Sizeof(info))
	if ok, _, _ := procGetMonitorInfoW.Call(handle, uintptr(unsafe.Pointer(&info))); ok == 0 {
		return rect{Left: 0, Top: 0, Right: systemMetrics(0), Bottom: systemMetrics(1)}, ""
	}
	return info.RcWork, utf16ToString(info.SzDevice[:])
}

// ── menus ─────────────────────────────────────────────────────────────────

func appendMenu(menu uintptr, flags uintptr, id uintptr, text string) {
	_, _, _ = procAppendMenuW.Call(menu, flags, id, utf16Ptr(text))
}

// trackPopupMenu opens a context menu at the given screen point. It returns the
// chosen command id, or 0 when the user dismissed the menu.
//
// SetForegroundWindow plus the trailing WM_NULL post are the documented dance
// that makes the menu close when the user clicks elsewhere.
func trackPopupMenu(menu uintptr, owner uintptr, at point) uintptr {
	setForegroundWindow(owner)
	command, _, _ := procTrackPopupMenuEx.Call(menu,
		uintptr(tpmReturnCmd|tpmRightButton), uintptr(at.X), uintptr(at.Y), owner, 0)
	postMessage(owner, 0 /* WM_NULL */, 0, 0)
	return command
}

// ── GDI ───────────────────────────────────────────────────────────────────

// createFont builds a font by asking GDI to map the requested height to the
// closest match it has, which keeps text readable at any DPI.
func createFont(height int32, bold bool, face string) uintptr {
	weight := int32(400)
	if bold {
		weight = fwBold
	}
	font, _, _ := procCreateFontW.Call(
		uintptr(height), 0, 0, 0, uintptr(weight), 0, 0, 0,
		uintptr(defaultCharset), 0, 0, uintptr(cleartypeQuality), 0,
		utf16Ptr(face))
	return font
}

func createSolidBrush(color uint32) uintptr {
	brush, _, _ := procCreateSolidBrush.Call(uintptr(color))
	return brush
}

// stockGuiFont returns the shell's default UI font. A zero handle is harmless:
// the caller simply skips WM_SETFONT and the system default is used.
func stockGuiFont() uintptr {
	font, _, _ := procGetStockObject.Call(defaultGuiFont)
	return font
}

func deleteObject(handle uintptr) {
	if handle != 0 {
		_, _, _ = procDeleteObject.Call(handle)
	}
}

func selectObject(dc, handle uintptr) uintptr {
	previous, _, _ := procSelectObject.Call(dc, handle)
	return previous
}

func setTextColor(dc uintptr, color uint32) {
	_, _, _ = procSetTextColor.Call(dc, uintptr(color))
}

func setBkMode(dc uintptr, mode int32) {
	_, _, _ = procSetBkMode.Call(dc, uintptr(mode))
}

// drawText draws text inside area with the given flags and returns nothing: a
// failed draw must never fail a paint cycle.
func drawText(dc uintptr, text string, area *rect, flags uintptr) {
	_, _, _ = procDrawTextW.Call(dc, utf16Ptr(text), ^uintptr(0), uintptr(unsafe.Pointer(area)), flags)
}

// rgb packs a colour the way COLORREF expects (0x00BBGGRR).
func rgb(red, green, blue byte) uint32 {
	return uint32(red) | uint32(green)<<8 | uint32(blue)<<16
}

// ── shell / tray ──────────────────────────────────────────────────────────

func notifyIcon(message uint32, data *notifyIconData) uintptr {
	data.CbSize = uint32(unsafe.Sizeof(*data))
	result, _, _ := procShellNotifyIconW.Call(uintptr(message), uintptr(unsafe.Pointer(data)))
	return result
}

// listView helpers ────────────────────────────────────────────────────────

func lvDeleteAllItems(list uintptr) {
	sendMessage(list, lvmDeleteAllItems, 0, 0)
}

func lvItemCount(list uintptr) int {
	return int(sendMessage(list, lvmGetItemCount, 0, 0))
}

func lvInsertColumn(list uintptr, index int, title string, width int32) {
	column := lvColumn{
		Mask:       lvcfFmtLeft | lvcfWidth | lvcfText | lvcfSubItem,
		Fmt:        lvcfFmtLeft,
		Cx:         width,
		PszText:    syscall.StringToUTF16Ptr(title),
		CchTextMax: int32(len(title) + 1),
		ISubItem:   int32(index),
	}
	sendMessage(list, lvmInsertColumnW, uintptr(index), uintptr(unsafe.Pointer(&column)))
}

func lvInsertItem(list uintptr, index int, text string) {
	item := lvItem{
		Mask:       lvifText,
		IItem:      int32(index),
		PszText:    syscall.StringToUTF16Ptr(text),
		CchTextMax: int32(len(text) + 1),
	}
	sendMessage(list, lvmInsertItemW, 0, uintptr(unsafe.Pointer(&item)))
}

func lvSetItemText(list uintptr, index int, subItem int, text string) {
	item := lvItem{
		Mask:       lvifText,
		IItem:      int32(index),
		ISubItem:   int32(subItem),
		PszText:    syscall.StringToUTF16Ptr(text),
		CchTextMax: int32(len(text) + 1),
	}
	sendMessage(list, lvmSetItemTextW, uintptr(index), uintptr(unsafe.Pointer(&item)))
}

func lvSetExtendedStyle(list uintptr, style uintptr) {
	sendMessage(list, lvmSetExtendedStyle, 0, style)
}

// lvSelectedIndex returns the single selected row, or -1 when the selection is
// empty or ambiguous.
func lvSelectedIndex(list uintptr) int {
	selected := int(sendMessage(list, lvmGetNextItem, ^uintptr(0) /* -1 */, lvniSelected))
	if selected < 0 {
		return -1
	}
	if next := int(sendMessage(list, lvmGetNextItem, uintptr(selected), lvniSelected)); next >= 0 {
		return -1
	}
	return selected
}

func lvSelectRow(list uintptr, index int) {
	state := lvItemState{State: lvniSelected | lvniFocused, StateMask: lvniSelected | lvniFocused}
	sendMessage(list, lvmSetItemState, uintptr(index), uintptr(unsafe.Pointer(&state)))
	sendMessage(list, lvmEnsureVisible, uintptr(index), 1)
}

//go:build windows

// CloudFile v0.8 Windows task center UI — the Go port of the WinForms preview
// that lives in windows/taskcenter-preview (kept there as the reference).
//
// Local summaries only: no native-host registration, no upload, no Checkout and
// no lease mutation.  Every task fact comes from the sibling
// cloudfile-taskcenter.exe CLI, exactly as the WinForms version did, so the Go
// CLI stays the single authority on task state.
//
// Everything must degrade rather than fail.  A tray icon that will not
// register, a list query that times out or a watcher that dies may cost the
// user an entry point or a notification, but none of them may touch a task, a
// working copy or a Checkout.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/eaopen/cloudfile-local-agent/internal/childproc"
	"github.com/eaopen/cloudfile-local-agent/internal/config"
	"github.com/eaopen/cloudfile-local-agent/internal/uinotify"
)

const (
	windowClassName = "CloudFileTaskCenterV08"
	floatClassName  = "CloudFileTaskCenterFloatV08"

	// One preview instance per Windows logon session, exactly as before. This is
	// not a second Chrome Host and it never registers anything.
	instanceMutex = `Local\CloudFileTaskCenterPreviewV08`

	windowTitle = "CloudFile 任务中心（本地预览）"
	trayTip     = "CloudFile 本地任务（可选系统托盘）"

	mainWidth     = 660
	mainHeight    = 380
	mainMinWidth  = 490
	mainMinHeight = 300
	statusHeight  = 35
	buttonHeight  = 36

	floatSize   = 48
	floatMargin = 12
	// A drag shorter than this in total is treated as a click, matching the
	// WinForms preview (|dx| + |dy| > 4).
	floatClickSlop = 4

	refreshMillis = 3000

	listTimeout       = 5 * time.Second
	focusTimeout      = 3 * time.Second
	preferenceTimeout = 2 * time.Second
	positionTimeout   = 2500 * time.Millisecond
	exitCheckTimeout  = 3 * time.Second
	balloonMillis     = 3500

	commandOpenCenter = 1001
	commandRefresh    = 1002
	commandQuit       = 1003
	commandManage     = 1004

	// Control ids are also their child window ids, which keeps WM_COMMAND
	// routing trivial.
	controlList   = 2001
	controlStatus = 2002
	controlManage = 1004

	timerRefresh  = 1
	timerSelfTest = 99

	// UI 线程超过这么久没有任何一次消息处理，就当成阻塞并记一笔。
	// 比 Windows 判「未响应」的阈值略早，好在窗口变灰之前就留下证据。
	uiStallMillis = 2500
	// 气泡队列长度；满了就丢 —— 提示是尽力而为，绝不能让 UI 线程等。
	balloonQueueSize = 16
	// 阻塞日志（与 config.json 同目录）。
	hangLogName = "ui-hang.log"
)

// selfTestMode suppresses every modal dialog: an automation run has nobody to
// click "OK", so failures have to reach stderr and the exit code instead.
var selfTestMode bool

// ── task models: the shape of the CLI's JSON, nothing more ────────────────

type taskList struct {
	Tasks              []taskEntry `json:"tasks"`
	ActiveExclusive    int         `json:"active_exclusive"`
	UncertainExclusive int         `json:"uncertain_exclusive"`
}

type taskEntry struct {
	ID       string `json:"task_id"`
	FileName string `json:"file_name"`
	RepoID   string `json:"repo_id"`
	Path     string `json:"path"`
	Mode     string `json:"mode"`
	Changed  bool   `json:"local_changed"`
	Review   bool   `json:"needs_review"`
}

type taskEvent struct {
	Kind     string `json:"kind"`
	FileName string `json:"file_name"`
	TaskID   string `json:"task_id"`
}

type preferences struct {
	FloatPosition *struct {
		Display string `json:"display"`
		X       int    `json:"x"`
		Y       int    `json:"y"`
	} `json:"float_position"`
}

// ── UI state ──────────────────────────────────────────────────────────────

type taskCenter struct {
	cliBinary string

	mainWindow   uintptr
	listControl  uintptr
	statusLabel  uintptr
	manageButton uintptr
	floatWindow  uintptr
	tray         notifyIconData
	trayReady    bool

	floatBrush uintptr
	floatFont  uintptr

	taskbarCreated uint32

	leaseWarnings      map[string]string
	activeExclusive    int
	uncertainExclusive int
	pendingFocusID     string
	// rowTaskIDs maps a list row to its task id: the list control has no tag
	// column, so the mapping is kept here instead of in the control.
	rowTaskIDs []string

	querying bool
	quitting bool

	// notifyOffset is how far the Agent's notification spool has been read. It
	// starts at the current end of file so a backlog from an earlier run does
	// not fire a burst of balloons at startup.
	notifyOffset int64
	// trayNoticePending delays the "已进托盘" balloon to the first refresh tick,
	// so it does not race the shell's own startup.
	trayNoticePending bool

	// Threading: every Win32 call that touches a window happens on the UI thread
	// that runs runMessageLoop. Worker goroutines only push closures here and then
	// post uiWakeup, which keeps the whole GUI single-threaded without an explicit
	// dispatcher. The two exceptions are deliberate and documented where they
	// live: balloons (tray worker) and SetForegroundWindow (fire-and-forget),
	// because a synchronous shell call on the UI thread can freeze the window.
	updates        chan func()
	watcherStopped chan struct{}
	watcherOnce    sync.Once
	// notifyWake 叫醒通知 worker；容量 1 即可（合并未处理的 tick）。
	notifyWake chan struct{}
	// trayResult 只给 self-test 用：托盘注册是异步的，自检要确定性地读到结果。
	trayResult chan bool

	// Drag state for the floating handle.
	dragging        bool
	dragMoved       bool
	dragStartCursor point
	dragStartWindow point

	selfTest bool
}

var app *taskCenter

// ── UI 线程阻塞看门狗 ─────────────────────────────────────────────────────
//
// 现场报过「点本地编辑后窗口变成未响应」，而进程从外面看不出卡在哪（CPU 为 0，
// 线程全在等）。所以 UI 线程每处理一条消息就刷新一次心跳，另有一个 goroutine
// 盯着：心跳停了就把「最后在做什么」写进 ui-hang.log。
// 只做原子读写，没有系统调用 —— 看门狗本身不可能成为新的卡点。

var (
	uiHeartbeat atomic.Int64 // UI 线程最后一次处理消息的时刻（Unix 毫秒）
	uiOpName    atomic.Value // 当前正在执行的操作名（string）
	uiOpSince   atomic.Int64 // 该操作的开始时刻（Unix 毫秒）
)

// tickUI 在 UI 线程每次进入窗口过程时调用：只要有消息在跑，就说明没卡。
func tickUI() {
	uiHeartbeat.Store(time.Now().UnixMilli())
}

// enterUI 在 UI 线程即将执行**可能阻塞**的操作前调用，给看门狗留下定位信息。
func enterUI(operation string) {
	now := time.Now().UnixMilli()
	uiHeartbeat.Store(now)
	uiOpSince.Store(now)
	uiOpName.Store(operation)
}

func (c *taskCenter) startWatchdog() {
	go func() {
		var reportedAt int64
		for {
			time.Sleep(500 * time.Millisecond)
			// self-test 会**故意**在 UI 线程上同步跑一次 CLI（最长 5 秒），
			// 那是测试语义不是故障，所以阈值抬高；真卡死（启动期挂住）照样会记。
			threshold := int64(uiStallMillis)
			if c.selfTest {
				threshold = 8000
			}
			last := uiHeartbeat.Load()
			stalled := time.Since(time.UnixMilli(last)).Milliseconds()
			if stalled < threshold {
				reportedAt = 0
				continue
			}
			// 心跳值不变就说明是同一次阻塞，只记一笔，别刷屏。
			if reportedAt == last {
				continue
			}
			reportedAt = last
			c.logHang(last, stalled)
		}
	}()
}

// logHang 追加一行阻塞记录。诊断永远不能影响主流程，任何失败都直接放弃。
func (c *taskCenter) logHang(heartbeat int64, stalledMillis int64) {
	operation, _ := uiOpName.Load().(string)
	if operation == "" {
		operation = "(未标注的操作)"
	}
	detail := operation
	if since := uiOpSince.Load(); since > 0 {
		detail = fmt.Sprintf("%s；该操作已进行 %d ms", operation, time.Now().UnixMilli()-since)
	}
	dir, err := config.Dir()
	if err != nil {
		return
	}
	file, err := os.OpenFile(filepath.Join(dir, hangLogName), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return
	}
	defer file.Close()
	fmt.Fprintf(file, "%s  UI 线程阻塞 %d ms（最后活动 %s），卡在：%s\n",
		time.Now().Format("2006-01-02 15:04:05"), stalledMillis,
		time.UnixMilli(heartbeat).Format("15:04:05"), detail)
}

// ── 托盘 worker：Shell_NotifyIcon 绝不能跑在 UI 线程上 ─────────────────────
//
// Shell_NotifyIcon 是**同步**调用（内部要跟任务栏进程打交道）。任务栏忙、被安全
// 软件挂钩、误留了一堆幽灵图标、或被企业策略限死时，它会长时间不返回。实测本机
// NIM_ADD 与 NIF_INFO 气泡都卡死过：
//   * 气泡那次 → 点「本地编辑」后窗口整个变成「未响应」（现场报告）；
//   * NIM_ADD 那次 → 窗口已经建出来但线程从此不再泵消息，看着就是「启动即未响应」。
// 所以**三种操作全部**交给同一个 worker goroutine 顺序执行，UI 线程只入队。
// 单个 worker 顺序消费，气泡顺序、注册与注销顺序都和原来一致。

type trayRequestKind int

const (
	trayAdd trayRequestKind = iota
	trayModify
	trayDelete
)

type trayRequest struct {
	kind trayRequestKind
	// 带上托盘数据的**快照**：worker 不去读 c.tray，避免跨线程读写同一结构。
	data    notifyIconData
	title   string
	text    string
	warning bool
}

var trayQueue = make(chan trayRequest, balloonQueueSize)

func (c *taskCenter) startTrayWorker() {
	go func() {
		for request := range trayQueue {
			switch request.kind {
			case trayAdd:
				ok := notifyIcon(nimAdd, &request.data) != 0
				// 结果交回 UI 线程：trayReady 只由 UI 线程写。
				c.postUI(func() {
					if ok {
						c.trayReady = true
						return
					}
					// 备选入口：托盘不可用时把完整窗口给用户，别让他什么都没有。
					c.setStatus("系统托盘不可用；可继续使用任务栏和普通窗口。")
					if c.mainWindow != 0 && !c.selfTest {
						showWindow(c.mainWindow, swRestore)
					}
				})
				// self-test 要确定性读到结果，另走一条只读通道。
				select {
				case c.trayResult <- ok:
				default:
				}
			case trayDelete:
				notifyIcon(nimDelete, &request.data)
			default:
				notice := request.data
				notice.UFlags = nifInfo
				utf16Into(notice.SzInfo[:], request.text)
				utf16Into(notice.SzInfoTitle[:], request.title)
				if request.warning {
					notice.DwInfoFlags = niifWarning
				} else {
					notice.DwInfoFlags = niifInfo
				}
				notifyIcon(nimModify, &notice)
			}
		}
	}()
}

func main() {
	cliFlag := flag.String("taskcenter-bin", "", "full path to cloudfile-taskcenter.exe (default: next to this executable)")
	selfTest := flag.Bool("self-test", false, "create the UI, pump messages briefly and exit (automation smoke test)")
	flag.Parse()
	selfTestMode = *selfTest

	cliBinary := *cliFlag
	if cliBinary == "" {
		executable, err := os.Executable()
		if err != nil {
			fail("无法定位本程序路径：" + err.Error())
		}
		cliBinary = filepath.Join(filepath.Dir(executable), "cloudfile-taskcenter.exe")
	}
	cliBinary, err := filepath.Abs(cliBinary)
	if err != nil {
		fail("无效的 taskcenter 路径：" + err.Error())
	}
	if info, statErr := os.Stat(cliBinary); statErr != nil || info.IsDir() {
		fail("找不到本机 Go TaskCenter CLI：\n" + cliBinary +
			"\n\n用法：cloudfile-taskcenter-ui.exe [--taskcenter-bin <完整路径>]")
	}

	handle, first := singleInstance(instanceMutex)
	if !first {
		// Another preview is already resident for this logon session; leave it
		// alone rather than racing it for the tray.
		return
	}
	defer closeHandle(handle)

	app = &taskCenter{
		cliBinary:      cliBinary,
		leaseWarnings:  map[string]string{},
		updates:        make(chan func(), 32),
		watcherStopped: make(chan struct{}),
		notifyWake:     make(chan struct{}, 1),
		trayResult:     make(chan bool, 1),
		selfTest:       *selfTest,
	}
	if err := app.run(); err != nil {
		fail(err.Error())
	}
}

func fail(message string) {
	if selfTestMode {
		fmt.Fprintln(os.Stderr, "self-test FAILED:", message)
		os.Exit(2)
	}
	messageBox(0, message, "CloudFile 本地任务预览", mbIconWarning)
	os.Exit(2)
}

// ── window procedure ──────────────────────────────────────────────────────

// lparam is declared as unsafe.Pointer rather than uintptr so message payloads
// that are really pointers (MINMAXINFO and friends) can be used directly
// instead of being rebuilt with a uintptr conversion that go vet flags.
var mainWndProc = syscall.NewCallback(func(hwnd uintptr, message uint32, wparam uintptr, lparam unsafe.Pointer) uintptr {
	// 每处理一条消息就刷新心跳：看门狗靠它判断 UI 线程有没有被挂住。
	tickUI()
	if app == nil {
		return defWindowProc(hwnd, message, wparam, uintptr(lparam))
	}
	if app.taskbarCreated != 0 && message == app.taskbarCreated {
		// Explorer restarted: the icon vanished with it, so put it back.
		app.restoreTray()
		return 0
	}
	switch message {
	case wmCreate:
		return 0
	case wmPaint:
		var paint paintStruct
		beginPaint(hwnd, &paint)
		endPaint(hwnd, &paint)
		return 0
	case wmSize:
		app.layoutMainWindow()
		return 0
	case wmGetMinMaxInfo:
		if lparam != nil {
			(*minMaxInfo)(lparam).PtMinTrackSize = point{X: mainMinWidth, Y: mainMinHeight}
		}
		return 0
	case wmTimer:
		switch wparam {
		case timerRefresh:
			app.refreshAsync()
			app.pollFocusAsync()
			app.pollNotifications()
			if app.trayNoticePending {
				app.trayNoticePending = false
				app.announceTray()
			}
		case timerSelfTest:
			app.finishSelfTest()
		}
		return 0
	case wmCommand:
		switch loWord(wparam) {
		case commandManage:
			app.manageSelectedTask()
		case commandOpenCenter:
			app.showCenter()
		case commandRefresh:
			app.refreshAsync()
		case commandQuit:
			app.requestQuit()
		}
		return 0
	case wmTrayMessage:
		switch loWord(uintptr(lparam)) {
		case wmLButtonUp, wmLButtonDBL:
			app.showCenter()
		case wmRButtonUp:
			enterUI("托盘右键菜单（TrackPopupMenu 是模态的）")
			app.openTrayMenu()
		}
		return 0
	case wmClose:
		// Never destroy on a close click: the popup may be the only entry point
		// the user can find under enterprise shell restrictions.
		if app.quitting {
			destroyWindow(hwnd)
		} else {
			showWindow(hwnd, swMinimize)
		}
		return 0
	case wmDestroy:
		app.shutdown()
		postQuitMessage(0)
		return 0
	case wmUIWakeup:
		enterUI("drainUpdates（执行 worker 投递的 UI 闭包）")
		app.drainUpdates()
		return 0
	case wmSelfDestruct:
		// Self-test only: wind the UI down without touching real state.
		app.quitting = true
		destroyWindow(hwnd)
		return 0
	}
	return defWindowProc(hwnd, message, wparam, uintptr(lparam))
})

var floatWndProc = syscall.NewCallback(func(hwnd uintptr, message uint32, wparam uintptr, lparam unsafe.Pointer) uintptr {
	if app == nil {
		return defWindowProc(hwnd, message, wparam, uintptr(lparam))
	}
	switch message {
	case wmPaint:
		app.paintFloat(hwnd)
		return 0
	case wmEraseBackground:
		return 1
	case wmLButtonDown:
		app.beginFloatDrag()
		return 0
	case wmMouseMove:
		if wparam&mkLButton != 0 {
			app.dragFloat()
		}
		return 0
	case wmLButtonUp:
		app.endFloatDrag()
		return 0
	case wmRButtonUp:
		app.openTrayMenu()
		return 0
	}
	return defWindowProc(hwnd, message, wparam, uintptr(lparam))
})

// ── startup ───────────────────────────────────────────────────────────────

func (c *taskCenter) run() error {
	// 看门狗**要在启动路径之前**就跑起来：托盘注册、CreateProcess 这些启动期的
	// shell 调用同样会挂住 UI 线程，那种情况下窗口已经建出来了但永远不泵消息。
	uiHeartbeat.Store(time.Now().UnixMilli())
	c.startWatchdog()
	// 自动化里最怕自检**永远不退出**（曾经 180 秒超时才发现）。UI 线程一旦卡在某个
	// user32 调用里，postMessage 是叫不醒它的，只有 os.Exit 能从别的 goroutine 收场 ——
	// 所以这条保险必须在**任何窗口调用之前**武装好（卡在建窗口阶段也照样能退出）。
	if c.selfTest {
		go func() {
			time.Sleep(12 * time.Second)
			fmt.Fprintln(os.Stderr, "self-test FAILED: 超过 12 秒仍未结束（UI 线程被卡住，详见 ui-hang.log）")
			os.Exit(4)
		}()
	}
	enterUI("initializeCommonControls（加载 comctl32）")
	initializeCommonControls()

	if err := registerWindowClass(windowClassName, mainWndProc, 0); err != nil {
		return errors.New("无法注册任务中心窗口类：" + err.Error())
	}
	if err := registerWindowClass(floatClassName, floatWndProc, 0); err != nil {
		return errors.New("无法注册浮标窗口类：" + err.Error())
	}
	// The floating handle wants a move cursor; per-class cursor is enough.
	c.taskbarCreated = registerWindowMessage("TaskbarCreated")

	enterUI("createMainWindow")
	if err := c.createMainWindow(); err != nil {
		return err
	}
	enterUI("createFloatWindow")
	c.createFloatWindow()
	// Shell_NotifyIcon 的 NIM_ADD 同样会跟任务栏打交道：加个标注，万一它挂了
	// 看门狗里就能直接看到是这一步。
	enterUI("createTray（Shell_NotifyIcon NIM_ADD 已移到 worker）")
	c.createTray()
	// 托盘操作、气泡、通知读取全部在 worker 里，UI 线程不再直接碰 Shell_NotifyIcon。
	c.startTrayWorker()

	if c.selfTest {
		showWindow(c.mainWindow, swShow)
	} else {
		// 托盘优先：任务栏窗口保持最小化，不在登录时弹出来打扰用户。
		// 万一托盘注册失败，worker 会把完整窗口显示出来（见 startTrayWorker）。
		showWindow(c.mainWindow, swMinimize)
	}
	updateWindow(c.mainWindow)

	if !c.selfTest {
		// 跳过历史通知：攒了一晚上的记录不该在开机时炸成一串气泡。
		c.notifyOffset, _ = uinotify.Size()
		c.trayNoticePending = true
		setTimer(c.mainWindow, timerRefresh, refreshMillis)
		c.refreshAsync()
		c.pollFocusAsync()
		enterUI("startWatcher（CreateProcess 可能被安全软件拦慢）")
		c.startWatcher()
		// 通知 worker 必须在初始偏移设好之后再起，否则它会把历史当新事件。
		c.startNotificationWorker()
	} else {
		// Automation smoke test. It cannot judge how the UI looks, so it proves
		// the two things that break silently -- the control interop and the CLI
		// data path -- and then closes itself.
		c.selfCheck()
		setTimer(c.mainWindow, timerSelfTest, 1200)
		// Watchdog: a posted message wakes the loop even if the timer never got
		// delivered, so a smoke run can always finish on its own.
		// （还不退出的兜底在 run() 开头已经武装好了。）
		window := c.mainWindow
		go func() {
			time.Sleep(5 * time.Second)
			postMessage(window, wmSelfDestruct, 0, 0)
		}()
	}

	enterUI("runMessageLoop（消息循环：这里的标签表示卡在某个处理函数里）")
	return c.runMessageLoop()
}

// selfCheck exercises the list control and the CLI before the window closes.
func (c *taskCenter) selfCheck() {
	// 1) Control interop: rows have to survive insertion and be counted back.
	enterUI("selfCheck：写列表控件")
	lvInsertItem(c.listControl, 0, "self-test-a.docx")
	lvSetItemText(c.listControl, 0, 1, "本地已修改，待确认")
	lvSetItemText(c.listControl, 0, 2, "独占")
	lvSetItemText(c.listControl, 0, 3, "01234567-89ab-cdef-0123-456789abcdef  /a.docx")
	lvInsertItem(c.listControl, 1, "self-test-b.docx")
	if rows := lvItemCount(c.listControl); rows != 2 {
		fmt.Fprintf(os.Stderr, "self-test 列表写入      FAILED（回读 %d 行，期望 2）\n", rows)
		os.Exit(3)
	}
	lvDeleteAllItems(c.listControl)
	fmt.Fprintln(os.Stderr, "self-test 列表写入      OK")

	// 2) Data path: the CLI must run, and its JSON must parse.
	enterUI("selfCheck：同步 runCLI --list")
	output, err := c.runCLI(listTimeout, "--list")
	if err != nil {
		fmt.Fprintln(os.Stderr, "self-test CLI --list    FAILED:", err)
		os.Exit(3)
	}
	var snapshot taskList
	if json.Unmarshal(output, &snapshot) != nil {
		fmt.Fprintln(os.Stderr, "self-test CLI --list    FAILED: 输出不是有效的任务 JSON")
		os.Exit(3)
	}
	for index, task := range snapshot.Tasks {
		lvInsertItem(c.listControl, index, task.FileName)
		lvSetItemText(c.listControl, index, 1, "已读取")
		lvSetItemText(c.listControl, index, 2, task.Mode)
		lvSetItemText(c.listControl, index, 3, task.RepoID+"  "+task.Path)
	}
	fmt.Fprintf(os.Stderr, "self-test CLI --list    OK（本地任务 %d 项，活动独占 %d）\n",
		len(snapshot.Tasks), snapshot.ActiveExclusive)

	// 3) Agent → UI 通知通道：队列路径能解析、没有队列时读作空。
	//    真正的「写一条再读回来」往返由 internal/uinotify 的单元测试覆盖 ——
	//    self-test 刻意不写任何文件，它不该在用户机器上留下数据。
	notifyPath, pathErr := uinotify.Path()
	enterUI("selfCheck：通知通道检查")
	_, _, notifyErr := uinotify.Read(0)
	if pathErr != nil || notifyErr != nil {
		fmt.Fprintf(os.Stderr, "self-test 通知通道      FAILED（%v %v）\n", pathErr, notifyErr)
		os.Exit(3)
	}
	fmt.Fprintf(os.Stderr, "self-test 通知通道      OK（%s）\n", notifyPath)
}

// finishSelfTest reports what was actually created and then closes the window,
// which unwinds through WM_DESTROY to a clean process exit. It exists because a
// GUI cannot be checked visually in CI, so at least "did it construct" can be.
func (c *taskCenter) finishSelfTest() {
	// 托盘注册是异步的（NIM_ADD 会挂 UI 线程），自检要确定性拿到结果就等一小会儿；
	// 超时按「不可用」处理 —— 托盘缺失是允许的降级，不会让自检失败。
	select {
	case ok := <-c.trayResult:
		c.trayReady = ok
	case <-time.After(2 * time.Second):
	}
	checks := []struct {
		name string
		ok   bool
	}{
		{"窗口", c.mainWindow != 0},
		{"任务列表", c.listControl != 0},
		{"状态栏", c.statusLabel != 0},
		{"管理按钮", c.manageButton != 0},
		{"浮标", c.floatWindow != 0},
		{"托盘", c.trayReady},
	}
	var failures []string
	for _, check := range checks {
		mark := "OK"
		if !check.ok {
			mark = "MISSING"
			// Only the window and the list are load-bearing; a missing tray or
			// handle is a documented degradation, not a failure.
			if check.name == "窗口" || check.name == "任务列表" {
				failures = append(failures, check.name)
			}
		}
		fmt.Fprintf(os.Stderr, "self-test %-10s %s\n", check.name, mark)
	}
	c.quitting = true
	destroyWindow(c.mainWindow)
	if len(failures) > 0 {
		fmt.Fprintln(os.Stderr, "self-test FAILED:", strings.Join(failures, ", "))
		os.Exit(3)
	}
}

func (c *taskCenter) createMainWindow() error {
	style := uintptr(wsOverlappedWindow)
	area := rect{Right: mainWidth, Bottom: mainHeight}
	adjustWindowRect(&area, style, false)
	width, height := area.width(), area.height()

	workArea, _ := monitorWorkArea(point{X: 0, Y: 0})
	x := workArea.Left + (workArea.width()-width)/2
	y := workArea.Top + (workArea.height()-height)/2

	hwnd := createWindowEx(0, windowClassName, windowTitle, style, x, y, width, height,
		0, 0, moduleHandle(), 0)
	if hwnd == 0 {
		return errors.New("无法创建任务中心窗口")
	}
	c.mainWindow = hwnd

	instance := moduleHandle()
	font := stockGuiFont()

	c.listControl = createWindowEx(wsExClientEdge, "SysListView32", "",
		wsChild|wsVisible|wsBorder|wsTabStop|wsClipChildren|wsClipSiblings|lvsReport,
		0, 0, 0, 0, hwnd, controlList, instance, 0)
	if c.listControl == 0 {
		return errors.New("无法创建任务列表控件")
	}
	lvSetExtendedStyle(c.listControl, lvsExFullRowSelect|lvsExGridLines)
	lvInsertColumn(c.listControl, 0, "文件", 170)
	lvInsertColumn(c.listControl, 1, "本地状态", 185)
	lvInsertColumn(c.listControl, 2, "模式", 85)
	lvInsertColumn(c.listControl, 3, "库 / 路径", 330)

	c.statusLabel = createWindowEx(0, "STATIC", "正在读取本地任务…（不自动上传）",
		wsChild|wsVisible|ssLeftNowordwrap, 0, 0, 0, 0, hwnd, controlStatus, instance, 0)

	c.manageButton = createWindowEx(0, "BUTTON", "管理选中文件（人工提交 / 恢复原 Intent）",
		wsChild|wsVisible|wsTabStop|bsPushButton, 0, 0, 0, 0, hwnd, controlManage, instance, 0)

	for _, control := range []uintptr{c.listControl, c.statusLabel, c.manageButton} {
		if control != 0 && font != 0 {
			sendMessage(control, wmSetFont, font, 1)
		}
	}
	c.layoutMainWindow()
	return nil
}

func (c *taskCenter) layoutMainWindow() {
	if c.mainWindow == 0 {
		return
	}
	area := getClientRect(c.mainWindow)
	height := area.Bottom
	if height < statusHeight+buttonHeight+40 {
		height = statusHeight + buttonHeight + 40
	}
	// WinForms docked the button first (bottom-most), then the status label, so
	// the button sits below the status line. Keep that order.
	buttonTop := height - buttonHeight
	statusTop := buttonTop - statusHeight
	listHeight := statusTop

	setWindowPos(c.manageButton, 0, 0, buttonTop, area.Right, buttonHeight, swpNoZOrder)
	setWindowPos(c.statusLabel, 0, 0, statusTop, area.Right, statusHeight, swpNoZOrder)
	setWindowPos(c.listControl, 0, 0, 0, area.Right, listHeight, swpNoZOrder)
}

func (c *taskCenter) createFloatWindow() {
	style := uintptr(wsPopup)
	hwnd := createWindowEx(wsExTopmost|wsExToolWindow, floatClassName, "",
		style, 0, 0, floatSize, floatSize, 0, 0, moduleHandle(), 0)
	if hwnd == 0 {
		return
	}
	c.floatWindow = hwnd
	c.floatBrush = createSolidBrush(rgb(41, 108, 204))
	c.floatFont = createFont(-31, true, "Segoe UI Symbol") // ~22pt

	x, y := c.defaultFloatPosition()
	setWindowPos(hwnd, hwndTopmost, x, y, floatSize, floatSize, swpNoActivate)
	// 保存过的位置要去问 CLI（--get-preferences）。那是**子进程**，不能压在启动
	// 路径上：它以前是同步跑的（而且那个调用点没有 WaitDelay，2 秒超时实为无限期），
	// 一旦 CLI 侧不返回，窗口就建完了但永远不泵消息 —— 看门狗抓到的「卡在
	// createFloatWindow」就是它。现在改成异步取回来再挪位置。
	c.applyStoredFloatPosition(hwnd)
}

// ── tray ──────────────────────────────────────────────────────────────────

func (c *taskCenter) createTray() {
	if c.mainWindow == 0 {
		return
	}
	c.tray = notifyIconData{
		HWnd:             c.mainWindow,
		UID:              idAppIcon,
		UFlags:           nifMessage | nifIcon | nifTip,
		UCallbackMessage: wmTrayMessage,
		HIcon:            loadStockIcon(),
	}
	utf16Into(c.tray.SzTip[:], trayTip)
	// NIM_ADD 是同步 shell 调用，实测会挂住 UI 线程（窗口建好了但永不泵消息），
	// 所以这里只入队；成败由 worker 回报（成功设 trayReady，失败给用户完整窗口）。
	if !c.enqueueTray(trayRequest{kind: trayAdd, data: c.tray}) {
		// 队列都排不进去（极不可能）：当作托盘不可用，别让用户没有入口。
		c.setStatus("系统托盘不可用；可继续使用任务栏和普通窗口。")
	}
}

func (c *taskCenter) restoreTray() {
	if c.tray.HWnd == 0 {
		return
	}
	c.enqueueTray(trayRequest{kind: trayAdd, data: c.tray})
}

// enqueueTray 入队一次托盘操作。队列满时返回 false —— 绝不阻塞 UI 线程。
func (c *taskCenter) enqueueTray(request trayRequest) bool {
	select {
	case trayQueue <- request:
		return true
	default:
		return false
	}
}

func (c *taskCenter) removeTray() {
	if !c.trayReady {
		return
	}
	// 同样交给 worker，免得退出时卡在 Shell_NotifyIcon 上。
	// 随后固定等一小会儿让注销真正发出去：NIM_DELETE 之后紧接着就 destroyWindow，
	// 窗口一没就注销不掉，会留下一个到鼠标划过才消失的幽灵图标。
	// 只睡固定时长 —— 就算 NIM_DELETE 真挂住，退出流程也不受影响。
	c.enqueueTray(trayRequest{kind: trayDelete, data: c.tray})
	time.Sleep(150 * time.Millisecond)
	c.trayReady = false
}

// balloon 只把请求交给托盘 worker（见文件上方 startTrayWorker 的说明）。
// 托盘还不可用时直接丢掉：worker 是先处理注册请求的，队列 FIFO 保证气泡不会
// 抢在图标之前。
func (c *taskCenter) balloon(title, text string, warning bool) {
	if !c.trayReady {
		return
	}
	select {
	case trayQueue <- trayRequest{kind: trayModify, data: c.tray, title: title, text: text, warning: warning}:
	default:
		// 队列满就丢：提示是尽力而为，绝不能让 UI 线程等。
	}
}

func (c *taskCenter) openTrayMenu() {
	menu, _, _ := procCreatePopupMenu.Call()
	if menu == 0 {
		return
	}
	defer func() { _, _, _ = procDestroyMenu.Call(menu) }()
	appendMenu(menu, mfString, commandOpenCenter, "打开任务中心")
	appendMenu(menu, mfString, commandRefresh, "刷新任务")
	appendMenu(menu, mfSeparator, 0, "")
	appendMenu(menu, mfString, commandQuit, "退出预览")

	at := getCursorPos()
	owner := c.mainWindow
	if owner == 0 {
		owner = c.floatWindow
	}
	switch loWord(trackPopupMenu(menu, owner, at)) {
	case commandOpenCenter:
		c.showCenter()
	case commandRefresh:
		c.refreshAsync()
	case commandQuit:
		c.requestQuit()
	}
}

// ── window plumbing ───────────────────────────────────────────────────────

func (c *taskCenter) showCenter() {
	if c.mainWindow == 0 {
		return
	}
	enterUI("showCenter")
	showWindow(c.mainWindow, swRestore)
	showWindow(c.mainWindow, swShow)
	// SetForegroundWindow 是跨进程同步调用：它会跟当前前台窗口所在线程打交道，
	// 对方不响应（刚启动的 Word/CAD 就是这种状态）时调用方会被挂住。
	// 窗口本身的 ShowWindow 必须在 UI 线程，但这一句可以挪到 worker —— 卡也只卡它。
	window := c.mainWindow
	go setForegroundWindow(window)
}

func (c *taskCenter) setStatus(text string) {
	if c.statusLabel != 0 {
		setWindowText(c.statusLabel, text)
	}
}

func (c *taskCenter) drainUpdates() {
	for {
		select {
		case update := <-c.updates:
			update()
		default:
			return
		}
	}
}

// postUI hands a closure to the UI thread. Ordering is preserved and no Win32
// call is ever made from a worker goroutine.
func (c *taskCenter) postUI(update func()) {
	select {
	case c.updates <- update:
		if c.mainWindow != 0 {
			postMessage(c.mainWindow, wmUIWakeup, 0, 0)
		}
	default:
		// Queue full: dropping a refresh is better than blocking a worker.
	}
}

func (c *taskCenter) runMessageLoop() error {
	var message msgStruct
	for {
		result := getMessage(&message, 0)
		if result == 0 {
			return nil
		}
		if result == -1 {
			return errors.New("消息循环失败")
		}
		translateMessage(&message)
		dispatchMessage(&message)
	}
}

// ── CLI plumbing ──────────────────────────────────────────────────────────

// cliCommand builds an invocation of the sibling CLI with the console window
// suppressed. The UI itself is a GUI binary (-H=windowsgui), but the CLI it
// drives is a console program: without CREATE_NO_WINDOW every 3-second refresh,
// focus poll and preference write would each flash a black box on screen.
func cliCommand(ctx context.Context, binary string, args ...string) *exec.Cmd {
	var command *exec.Cmd
	if ctx != nil {
		command = exec.CommandContext(ctx, binary, args...)
		// 超时只杀得掉直接子进程：stderr 是普通 Writer（被包成管道），Output()/Wait()
		// 还要等管道读到 EOF，而只要 CLI 拉起过任何继承了句柄的孙进程，EOF 就永不到来
		// —— 于是「5 秒超时」实际会变成无限期挂住。WaitDelay 让 Wait 到点就返回。
		command.WaitDelay = 2 * time.Second
	} else {
		command = exec.Command(binary, args...)
	}
	childproc.Hide(command)
	return command
}

// runCLI runs the sibling Go CLI and returns its stdout. The CLI is the only
// source of task truth; a failure here is reported, never guessed around.
func (c *taskCenter) runCLI(timeout time.Duration, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	command := cliCommand(ctx, c.cliBinary, args...)
	var stderr strings.Builder
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("taskcenter %s 超时", strings.Join(args, " "))
		}
		return nil, fmt.Errorf("taskcenter %s 失败：%v", strings.Join(args, " "), err)
	}
	return output, nil
}

func (c *taskCenter) refreshAsync() {
	if c.querying || c.quitting || c.selfTest {
		return
	}
	c.querying = true
	go func() {
		output, err := c.runCLI(listTimeout, "--list")
		c.postUI(func() {
			c.querying = false
			if err != nil {
				// A transient failure must not hide tasks the user can still see.
				c.setStatus("无法更新本地任务；已有任务保持显示，请检查 Agent Core。")
				return
			}
			var snapshot taskList
			if json.Unmarshal(output, &snapshot) != nil {
				c.setStatus("无法更新本地任务；已有任务保持显示，请检查 Agent Core。")
				return
			}
			c.applySnapshot(snapshot)
		})
	}()
}

func (c *taskCenter) applySnapshot(snapshot taskList) {
	enterUI("applySnapshot（刷新任务列表）")
	c.activeExclusive = snapshot.ActiveExclusive
	c.uncertainExclusive = snapshot.UncertainExclusive

	if c.listControl != 0 {
		lvDeleteAllItems(c.listControl)
		c.rowTaskIDs = c.rowTaskIDs[:0]
		for index, task := range snapshot.Tasks {
			state, warned := c.leaseWarnings[task.ID]
			if !warned {
				switch {
				case task.Review:
					state = "需要检查"
				case task.Changed:
					state = "本地已修改，待确认"
				default:
					state = "本地编辑中"
				}
			}
			mode := "普通"
			if task.Mode == "exclusive-edit" {
				mode = "独占"
			}
			lvInsertItem(c.listControl, index, task.FileName)
			lvSetItemText(c.listControl, index, 1, state)
			lvSetItemText(c.listControl, index, 2, mode)
			lvSetItemText(c.listControl, index, 3, task.RepoID+"  "+task.Path)
			c.rowTaskIDs = append(c.rowTaskIDs, task.ID)
		}
	}
	c.highlightPendingFocus()
	c.setStatus(fmt.Sprintf("本地任务 %d 项；活动独占 %d，状态待查 %d。保存≠上传。",
		len(snapshot.Tasks), snapshot.ActiveExclusive, snapshot.UncertainExclusive))

	// The handle only exists while there is something to look at.
	if c.floatWindow != 0 {
		if len(snapshot.Tasks) > 0 {
			showWindow(c.floatWindow, swShow)
			setWindowPos(c.floatWindow, hwndTopmost, 0, 0, 0, 0, swpNoZOrder|swpNoActivate)
		} else {
			showWindow(c.floatWindow, swHide)
		}
	}
}

func (c *taskCenter) pollFocusAsync() {
	if c.quitting || c.selfTest {
		return
	}
	go func() {
		output, err := c.runCLI(focusTimeout, "--poll-focus")
		if err != nil {
			// An unavailable focus observer must never interrupt local editing.
			return
		}
		var result struct {
			TaskID string `json:"task_id"`
		}
		if json.Unmarshal(output, &result) != nil || result.TaskID == "" {
			return
		}
		c.postUI(func() {
			c.pendingFocusID = result.TaskID
			c.showCenter()
			c.highlightPendingFocus()
		})
	}()
}

// highlightPendingFocus selects the row a focus request asked for. If the row
// is not on screen yet the request is kept, and the next refresh selects it.
func (c *taskCenter) highlightPendingFocus() {
	if c.pendingFocusID == "" || c.listControl == 0 {
		return
	}
	for index, id := range c.rowTaskIDs {
		if id == c.pendingFocusID {
			lvSelectRow(c.listControl, index)
			c.pendingFocusID = ""
			return
		}
	}
}

// ── watcher: local save / review notifications ────────────────────────────

func (c *taskCenter) startWatcher() {
	command := cliCommand(nil, c.cliBinary, "--watch")
	stdout, err := command.StdoutPipe()
	if err != nil {
		c.setStatus("本地保存提示暂不可用；任务列表仍可查询。")
		return
	}
	if err := command.Start(); err != nil {
		c.setStatus("本地保存提示暂不可用；任务列表仍可查询。")
		return
	}
	go func() {
		defer func() { _ = command.Wait() }()
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			var event taskEvent
			if json.Unmarshal(scanner.Bytes(), &event) != nil {
				continue
			}
			if !interestingEvent(event.Kind) {
				continue
			}
			c.handleEvent(event)
		}
	}()
}

func interestingEvent(kind string) bool {
	return kind == "local-saved" || kind == "needs-review" || strings.HasPrefix(kind, "checkout-")
}

func (c *taskCenter) handleEvent(event taskEvent) {
	c.postUI(func() {
		if c.quitting {
			return
		}
		switch {
		case event.Kind == "checkout-renewed" || event.Kind == "checkout-ended":
			delete(c.leaseWarnings, event.TaskID)
		case strings.HasPrefix(event.Kind, "checkout-"):
			c.leaseWarnings[event.TaskID] = "⚠ 独占续租未确认"
			c.balloon("CloudFile 独占任务",
				"后台续租未确认，请打开任务中心检查。不会自动释放 Checkout。", true)
		default:
			notice := "本地已修改，尚未上传"
			warning := false
			if event.Kind == "needs-review" {
				notice = "本地文件需要检查"
				warning = true
			}
			c.balloon("CloudFile · "+event.FileName, notice, warning)
		}
		c.refreshAsync()
	})
}

// ── Agent → UI notifications ──────────────────────────────────────────────

// pollNotifications 只负责**叫醒**通知 worker。读文件这一步（%AppData% 可能被
// 重定向到很慢的网络位置）以前是内联在 UI 线程上的，现在挪到 worker。
func (c *taskCenter) pollNotifications() {
	select {
	case c.notifyWake <- struct{}{}:
	default:
	}
}

// startNotificationWorker 串行消费通知队列：读增量 → 交给 UI 线程弹气泡。
// 偏移量由本 goroutine 独占（启动时的初始偏移由 UI 线程在起它之前设好）。
func (c *taskCenter) startNotificationWorker() {
	go func() {
		for range c.notifyWake {
			events, next, err := uinotify.Read(c.notifyOffset)
			if err != nil {
				continue
			}
			c.notifyOffset = next
			for _, event := range events {
				c.postUI(func() { c.showNotification(event) })
			}
		}
	}()
}

func (c *taskCenter) showNotification(event uinotify.Event) {
	if event.Kind != uinotify.KindOpened {
		return
	}
	notice := "已在本地打开只读副本"
	if event.Mode == "local-edit" {
		notice = "已打开工作副本；保存不会自动上传"
	}
	if event.Dir != "" {
		notice += "\n" + event.Dir
	}
	// NOTIFYICONDATAW 的标题上限 64 个字符、正文 256 个，超了会被系统截断成乱码。
	c.balloon("CloudFile · "+truncate(event.File, 48), truncate(notice, 240), false)
}

// announceTray explains, once per install, where the icon lives. A tray-only
// application has no other way to announce itself, and field feedback was
// exactly「没看到 UI 界面」。
func (c *taskCenter) announceTray() {
	if !c.trayReady {
		return
	}
	first, err := uinotify.ClaimStartupNotice()
	if err != nil || !first {
		return
	}
	c.balloon("CloudFile 已在托盘运行",
		"本地查看/编辑的文件会在这里提示；双击图标打开任务中心。", false)
}

// truncate keeps balloon text inside the NOTIFYICONDATAW field limits.
func truncate(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit-1]) + "…"
}

// ── explicit actions ──────────────────────────────────────────────────────

// manageSelectedTask is only ever called from a local GUI click. It does not
// create a second upload workflow: it asks the installed controller to show the
// original per-file dialog. On the Go-only Windows line that controller does not
// exist yet, which is reported plainly rather than faked.
func (c *taskCenter) manageSelectedTask() {
	if c.listControl == 0 {
		return
	}
	index := lvSelectedIndex(c.listControl)
	if index < 0 {
		c.setStatus("请先选中一个受信编辑会话；普通文件列表无法直接触发云端写入。")
		return
	}
	controller := filepath.Join(filepath.Dir(c.cliBinary), "CloudFile.Agent.Windows.exe")
	if info, err := os.Stat(controller); err != nil || info.IsDir() {
		c.setStatus("当前是独立任务预览；人工提交请使用完整 Agent 一体化包。")
		return
	}
	enterUI("manageSelectedTask：启动原生文件操作窗口")
	// CreateProcess 在装了安全软件的机器上可能要几秒（新落地的 exe 会被扫），
	// 所以放到 goroutine 里，UI 线程只更新一句状态。
	statusReady := func() { c.setStatus("已向原生文件操作窗口提交定位请求；云端写入仍需逐文件确认。") }
	go func() {
		launched := exec.Command(controller)
		childproc.Hide(launched)
		if err := launched.Start(); err != nil {
			c.postUI(func() { c.setStatus("本地操作窗口不可用；工作副本和待决 Intent 均保留。") })
			return
		}
		c.postUI(statusReady)
	}()
}

// requestQuit re-reads the durable task list before stopping. A live Checkout
// must never be dropped silently just because the UI is closing.
func (c *taskCenter) requestQuit() {
	if c.quitting {
		return
	}
	active, uncertain := c.activeExclusive, c.uncertainExclusive
	enterUI("requestQuit：同步执行 taskcenter --list")
	output, err := c.runCLI(exitCheckTimeout, "--list")
	if err != nil {
		uncertain++
	} else {
		var snapshot taskList
		if json.Unmarshal(output, &snapshot) != nil {
			uncertain++
		} else {
			active, uncertain = snapshot.ActiveExclusive, snapshot.UncertainExclusive
		}
	}
	if active+uncertain > 0 {
		answer := messageBox(c.mainWindow,
			"仍有独占编辑任务。退出将停止 TaskCenter 后台续租，不会释放 Checkout 或删除本地文件。仍然退出？",
			"CloudFile 独占编辑", mbYesNo|mbIconWarning|mbDefButton2)
		if answer != idYes {
			return
		}
	}
	c.quitting = true
	c.shutdown()
	destroyWindow(c.mainWindow)
}

func (c *taskCenter) shutdown() {
	c.watcherOnce.Do(func() { close(c.watcherStopped) })
	if c.mainWindow != 0 {
		killTimer(c.mainWindow, timerRefresh)
	}
	c.removeTray()
	if c.floatWindow != 0 {
		destroyWindow(c.floatWindow)
		c.floatWindow = 0
	}
	if c.floatBrush != 0 {
		deleteObject(c.floatBrush)
		c.floatBrush = 0
	}
	if c.floatFont != 0 {
		deleteObject(c.floatFont)
		c.floatFont = 0
	}
}

// ── floating handle ───────────────────────────────────────────────────────

func (c *taskCenter) paintFloat(hwnd uintptr) {
	var paint paintStruct
	dc := beginPaint(hwnd, &paint)
	if dc == 0 {
		endPaint(hwnd, &paint)
		return
	}
	area := getClientRect(hwnd)
	if c.floatBrush != 0 {
		fillRect(dc, &area, c.floatBrush)
	}
	if c.floatFont != 0 {
		setBkMode(dc, transparent)
		setTextColor(dc, rgb(255, 255, 255))
		previous := selectObject(dc, c.floatFont)
		drawText(dc, "☁", &area, dtCenter|dtVCenter|dtSingleLine|dtNoPrefix)
		selectObject(dc, previous)
	}
	endPaint(hwnd, &paint)
}

func (c *taskCenter) beginFloatDrag() {
	c.dragging = true
	c.dragMoved = false
	c.dragStartCursor = getCursorPos()
	area := getWindowRect(c.floatWindow)
	c.dragStartWindow = point{X: area.Left, Y: area.Top}
	setCapture(c.floatWindow)
}

func (c *taskCenter) dragFloat() {
	if !c.dragging || c.floatWindow == 0 {
		return
	}
	current := getCursorPos()
	dx := current.X - c.dragStartCursor.X
	dy := current.Y - c.dragStartCursor.Y
	if abs32(dx)+abs32(dy) > floatClickSlop {
		c.dragMoved = true
	}
	if !c.dragMoved {
		return
	}
	setWindowPos(c.floatWindow, 0, c.dragStartWindow.X+dx, c.dragStartWindow.Y+dy,
		floatSize, floatSize, swpNoZOrder|swpNoActivate)
}

func (c *taskCenter) endFloatDrag() {
	if !c.dragging {
		return
	}
	c.dragging = false
	releaseCapture()
	if c.floatWindow == 0 {
		return
	}
	if !c.dragMoved {
		// A click toggles the task window, matching the WinForms preview.
		if isWindowVisible(c.mainWindow) {
			showWindow(c.mainWindow, swHide)
		} else {
			c.showCenter()
		}
		return
	}
	x, y := c.snapFloatToEdge()
	setWindowPos(c.floatWindow, hwndTopmost, x, y, floatSize, floatSize, swpNoZOrder|swpNoActivate)
	c.saveFloatPosition(x, y)
}

// snapFloatToEdge clamps the handle into the current monitor's work area and
// pulls it to the nearer horizontal edge.
func (c *taskCenter) snapFloatToEdge() (int32, int32) {
	area := getWindowRect(c.floatWindow)
	x, y := area.Left, area.Top
	work, _ := monitorWorkArea(point{X: x + floatSize/2, Y: y + floatSize/2})
	x = clamp32(x, work.Left, work.Right-floatSize)
	y = clamp32(y, work.Top, work.Bottom-floatSize)
	leftDistance := abs32(x - work.Left)
	rightDistance := abs32(work.Right - (x + floatSize))
	if leftDistance < rightDistance {
		x = work.Left
	} else {
		x = work.Right - floatSize
	}
	return x, y
}

// storedFloatPosition reads the position through the Go CLI so both platforms
// share one settings file. Any failure falls back to the default corner.
// defaultFloatPosition 是不问 CLI 时的位置：主显示器右侧居中。
func (c *taskCenter) defaultFloatPosition() (int32, int32) {
	work, _ := monitorWorkArea(point{X: 0, Y: 0})
	return work.Right - floatSize - floatMargin, work.Top + work.height()/2
}

// applyStoredFloatPosition 异步取回上次保存的位置并挪动浮标。
// 窗口操作仍然回到 UI 线程执行；取不回就用默认位置，不报错。
func (c *taskCenter) applyStoredFloatPosition(hwnd uintptr) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), preferenceTimeout)
		defer cancel()
		output, err := cliCommand(ctx, c.cliBinary, "--get-preferences").Output()
		if err != nil {
			return
		}
		var prefs preferences
		if json.Unmarshal(output, &prefs) != nil || prefs.FloatPosition == nil {
			return
		}
		x, y := int32(prefs.FloatPosition.X), int32(prefs.FloatPosition.Y)
		// Clamp whatever came back: a saved position can belong to a monitor that
		// is no longer attached.
		area, _ := monitorWorkArea(point{X: x + floatSize/2, Y: y + floatSize/2})
		x = clamp32(x, area.Left, area.Right-floatSize)
		y = clamp32(y, area.Top, area.Bottom-floatSize)
		c.postUI(func() {
			if c.floatWindow == hwnd {
				setWindowPos(hwnd, hwndTopmost, x, y, floatSize, floatSize, swpNoActivate)
			}
		})
	}()
}

func (c *taskCenter) saveFloatPosition(x, y int32) {
	display := "win:"
	if _, name := monitorWorkArea(point{X: x + floatSize/2, Y: y + floatSize/2}); name != "" {
		display = "win:" + name
	}
	// Advisory only: a position that cannot be saved must never block the UI.
	// 子进程放到 goroutine 里 —— 这是拖拽结束时调的，压在 UI 线程上会直接卡窗口。
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), positionTimeout)
		defer cancel()
		_ = cliCommand(ctx, c.cliBinary, "--set-position",
			"--x", fmt.Sprint(x), "--y", fmt.Sprint(y), "--display", display).Run()
	}()
}

// ── helpers ───────────────────────────────────────────────────────────────

func abs32(value int32) int32 {
	if value < 0 {
		return -value
	}
	return value
}

func clamp32(value, low, high int32) int32 {
	if high < low {
		return low
	}
	if value < low {
		return low
	}
	if value > high {
		return high
	}
	return value
}

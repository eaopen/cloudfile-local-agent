// CloudFile v0.8 macOS read-only UI preview.
// This is a local AppKit adapter, not a second Native Messaging Host.
// No OAuth, Checkout, Commit, upload, renewal, or browser authority lives here.
import AppKit
import Foundation
import Darwin

struct TaskPreferences: Decodable {
    let reminder: String
    let float_position: FloatPosition?
}
struct FloatPosition: Decodable {
    let display: String
    let x: Int
    let y: Int
}
struct TaskNotice: Decodable {
    let kind: String
    let file_name: String
}

struct TaskList: Decodable {
    let tasks: [TaskEntry]
    let active_exclusive: Int
    let uncertain_exclusive: Int
}
struct TaskEntry: Decodable {
    let task_id: String
    let file_name: String
    let repo_id: String
    let path: String
    let mode: String
    let local_changed: Bool
    let local_missing: Bool
    let needs_review: Bool
}

// Click to open one window. A drag moves the single floating icon.
final class FloatingHandle: NSButton {
    private var initialMouse = NSPoint.zero
    private var initialOrigin = NSPoint.zero
    private var moved = false
    var onDrop: (() -> Void)?

    override func mouseDown(with event: NSEvent) {
        initialMouse = NSEvent.mouseLocation
        initialOrigin = window?.frame.origin ?? .zero
        moved = false
    }
    override func mouseDragged(with event: NSEvent) {
        let p = NSEvent.mouseLocation
        let dx = p.x - initialMouse.x
        let dy = p.y - initialMouse.y
        if abs(dx) + abs(dy) > 4 { moved = true }
        if moved {
            window?.setFrameOrigin(NSPoint(x: initialOrigin.x + dx, y: initialOrigin.y + dy))
        }
    }
    override func mouseUp(with event: NSEvent) {
        if moved {
            onDrop?()
        } else if let action = self.action {
            NSApp.sendAction(action, to: self.target, from: self)
        }
    }
}

final class TaskCenterPreview: NSObject, NSApplicationDelegate {
    private let taskcenterBinary: URL
    private let goCoreBinary: String?
    private var lastLeaseWarning: String?
    private var activeExclusive = 0
    private var uncertainExclusive = 0
    private var status: NSStatusItem?
    private let listWindow = NSWindow(
        contentRect: NSRect(x: 0, y: 0, width: 480, height: 360),
        styleMask: [.titled, .closable, .miniaturizable, .resizable],
        backing: .buffered, defer: false)
    private let content = NSTextView()
    private var floating: NSPanel?
    private var refreshing = false
    private var tasks: [TaskEntry] = []
    private var pendingFocusID: String?
    private var timer: Timer?
    private var watcher: Process?
    private var noticePanel: NSPanel?
    private var noticeHide: DispatchWorkItem?

    init(binary: URL, core: String?) {
        self.taskcenterBinary = binary
        self.goCoreBinary = core
    }

    func applicationDidFinishLaunching(_ notification: Notification) {
        // Menu-bar entry remains the primary interaction; Dock provides a
        // secondary entry when the menu bar icon is concealed/managed.
        // Neither entry needs administrator permissions or an installer.
        NSApp.setActivationPolicy(.regular)
        listWindow.title = "CloudFile 任务中心（本地预览）"
        listWindow.minSize = NSSize(width: 370, height: 260)
        listWindow.center()
        content.isEditable = false
        content.isSelectable = true
        content.frame = NSRect(x: 0, y: 0, width: 480, height: 360)
        content.isVerticallyResizable = true
        content.isHorizontallyResizable = false
        content.autoresizingMask = [.width]
        content.textContainer?.widthTracksTextView = true
        content.font = NSFont.systemFont(ofSize: 13)
        content.textContainerInset = NSSize(width: 15, height: 15)
        content.string = "检查本地任务中…\n\n云端上传与独占续租未连接。"
        let scroll = NSScrollView(frame: listWindow.contentView!.bounds)
        scroll.autoresizingMask = [.width, .height]
        scroll.hasVerticalScroller = true
        scroll.documentView = content
        listWindow.contentView?.addSubview(scroll)

        let item = NSStatusBar.system.statusItem(withLength: NSStatusItem.squareLength)
        item.button?.title = "☁"
        item.button?.toolTip = "CloudFile 本地编辑任务（只读）"
        let menu = NSMenu()
        let show = NSMenuItem(title: "打开任务中心", action: #selector(showWindow), keyEquivalent: "")
        show.target = self
        menu.addItem(show)
        let refresh = NSMenuItem(title: "刷新任务", action: #selector(refreshMenu), keyEquivalent: "")
        refresh.target = self
        menu.addItem(refresh)
        let manage = NSMenuItem(title:"管理选中文件（人工提交 / 恢复）",
                                action:#selector(manageSelectedTask),keyEquivalent:"")
        manage.target = self
        menu.addItem(manage)
        menu.addItem(.separator())
        let exit = NSMenuItem(title: "退出预览", action: #selector(quit), keyEquivalent: "")
        exit.target = self
        menu.addItem(exit)
        item.menu = menu
        status = item
        listWindow.makeKeyAndOrderFront(nil)
        refreshTasks()
        startWatcher()
        timer = Timer.scheduledTimer(withTimeInterval: 3, repeats: true) { [weak self] _ in
            self?.refreshTasks()
            self?.pollFocus()
        }
        pollFocus()
    }

    func applicationShouldTerminate(_ sender: NSApplication) -> NSApplication.TerminateReply {
        // Re-query durable Journal status instead of trusting the last cached UI refresh.
        let proc = Process()
        proc.executableURL = taskcenterBinary
        proc.arguments = ["--list"]
        let output = Pipe()
        proc.standardOutput = output
        proc.standardError = Pipe()
        var active = activeExclusive
        var uncertain = uncertainExclusive
        do {
            try proc.run()
            let data = output.fileHandleForReading.readDataToEndOfFile()
            proc.waitUntilExit()
            if proc.terminationStatus != 0 { throw NSError(domain:"CloudFileTaskCenter",code:1) }
            let found = try JSONDecoder().decode(TaskList.self,from:data)
            active = found.active_exclusive
            uncertain = found.uncertain_exclusive
        } catch {
            uncertain += 1 // No false safe-to-exit conclusion on a failed journal query.
        }
        if active + uncertain == 0 { return .terminateNow }
        let alert = NSAlert()
        alert.messageText = "仍有独占编辑任务"
        alert.informativeText = "退出会停止 TaskCenter 后台续租，但不会释放 Checkout 或删除本地副本。建议保持 Agent 运行，并在任务中心完成编辑。"
        alert.addButton(withTitle: "继续运行")
        alert.addButton(withTitle: "仍然退出")
        return alert.runModal() == .alertSecondButtonReturn ? .terminateNow : .terminateCancel
    }

    func applicationWillTerminate(_ notification: Notification) {
        noticeHide?.cancel()
        if let proc = watcher, proc.isRunning { proc.terminate() }
    }

    // The watch command is a local-only observer. It cannot publish or Checkin.
    private func startWatcher() {
        let process = Process()
        process.executableURL = taskcenterBinary
        process.arguments = ["--watch"]
        if let core = goCoreBinary {
            process.arguments! += ["--go-core-bin", core]
        }
        let output = Pipe()
        process.standardOutput = output
        process.standardError = Pipe()
        do {
            try process.run()
            watcher = process
            DispatchQueue.global(qos: .utility).async { [weak self] in
                let handle = output.fileHandleForReading
                var buffer = Data()
                while true {
                    let chunk = handle.availableData
                    if chunk.isEmpty { break }
                    buffer.append(chunk)
                    while let newline = buffer.firstIndex(of: 10) {
                        let packet = buffer.prefix(upTo: newline)
                        buffer.removeSubrange(...newline)
                        guard let notice = try? JSONDecoder().decode(TaskNotice.self, from: Data(packet)),
                              notice.kind == "local-saved" || notice.kind == "needs-review" else { continue }
                        DispatchQueue.main.async { [weak self] in
                            if notice.kind.hasPrefix("checkout-") {
                                if notice.kind == "checkout-renewed" || notice.kind == "checkout-ended" {
                                    self?.lastLeaseWarning = nil
                                } else {
                                    self?.lastLeaseWarning = "独占任务续租状态需要检查（不自动释放锁）"
                                    self?.showNotice(TaskNotice(kind: notice.kind,
                                        file_name: notice.file_name))
                                }
                            } else {
                                self?.showNotice(notice)
                            }
                            self?.refreshTasks()
                        }
                    }
                    if buffer.count > 65536 { buffer.removeAll() }
                }
            }
        } catch {
            content.string = "本地保存提示暂不可用。任务列表仍可查询。"
        }
    }

    // Passive, short-lived tip: does not activate the application.
    private func showNotice(_ event: TaskNotice) {
        let message: String
        if event.kind == "local-saved" {
            message = "\(event.file_name)：本地已修改，尚未上传"
        } else if event.kind.hasPrefix("checkout-") {
            message = "\(event.file_name)：独占续租未确认，请检查任务状态"
        } else {
            message = "\(event.file_name)：本地文件需要检查"
        }
        let screen = NSScreen.main?.visibleFrame ?? NSRect(x: 0, y: 0, width: 1024, height: 768)
        let rect = NSRect(x: screen.maxX - 370, y: screen.maxY - 72, width: 350, height: 56)
        if noticePanel == nil {
            let panel = NSPanel(contentRect: rect,
                styleMask: [.borderless, .nonactivatingPanel], backing: .buffered, defer: false)
            panel.level = .floating
            panel.isOpaque = false
            panel.backgroundColor = NSColor.windowBackgroundColor
            panel.hasShadow = true
            panel.ignoresMouseEvents = true
            panel.collectionBehavior = [.canJoinAllSpaces]
            let field = NSTextField(labelWithString: "")
            field.frame = NSRect(x: 12, y: 10, width: 326, height: 36)
            field.maximumNumberOfLines = 2
            field.font = NSFont.systemFont(ofSize: 13)
            panel.contentView?.addSubview(field)
            noticePanel = panel
        }
        noticePanel?.setFrame(rect, display: true)
        if let field = noticePanel?.contentView?.subviews.first as? NSTextField {
            field.stringValue = message
        }
        noticePanel?.orderFrontRegardless()
        noticeHide?.cancel()
        let hide = DispatchWorkItem { [weak self] in self?.noticePanel?.orderOut(nil) }
        noticeHide = hide
        DispatchQueue.main.asyncAfter(deadline: .now() + 3.5, execute: hide)
    }

    private var checkingFocus = false
    // One GUI instance owns the short-lived focus mailbox. It heartbeats to
    // the private Go task store, not a localhost port or browser process.
    private func pollFocus() {
        if checkingFocus { return }
        checkingFocus = true
        let binary = taskcenterBinary
        DispatchQueue.global(qos: .utility).async { [weak self] in
            let proc = Process()
            proc.executableURL = binary
            proc.arguments = ["--poll-focus"]
            let output = Pipe()
            proc.standardOutput = output
            proc.standardError = Pipe()
            var requested = ""
            do {
                try proc.run()
                let data = output.fileHandleForReading.readDataToEndOfFile()
                proc.waitUntilExit()
                if proc.terminationStatus == 0,
                   let obj = try JSONSerialization.jsonObject(with:data) as? [String:Any] {
                    requested = obj["task_id"] as? String ?? ""
                }
            } catch {}
            DispatchQueue.main.async {
                self?.checkingFocus = false
                if !requested.isEmpty {
                    self?.pendingFocusID = requested
                    self?.showWindow()
                    self?.highlightFocusedTask()
                }
            }
        }
    }

    private func highlightFocusedTask() {
        guard let id = pendingFocusID,
              let task = tasks.first(where: {$0.task_id == id}) else { return }
        listWindow.title = "CloudFile · " + task.file_name
        pendingFocusID = nil
    }

    @objc private func manageSelectedTask() {
        guard let core = goCoreBinary,
              FileManager.default.isExecutableFile(atPath: core) else {
            let alert=NSAlert()
            alert.messageText="需要完整 CloudFile v0.8 一体化包"
            alert.informativeText="此便携任务预览没有原生编辑 Core。任务保留；不会替用户上传。"
            alert.runModal()
            return
        }
        let current = tasks.filter { $0.task_id.hasPrefix("session-") }
        guard !current.isEmpty else {
            let alert=NSAlert()
            alert.messageText="没有可恢复的受信编辑会话"
            alert.runModal()
            return
        }
        let alert=NSAlert()
        alert.messageText="选择要管理的单个本地文件"
        alert.informativeText="仅通过原始 Agent 打开人工操作对话框；不会因本地保存自动上传。"
        alert.addButton(withTitle:"打开原编辑操作")
        alert.addButton(withTitle:"取消")
        let picker=NSPopUpButton(frame:NSRect(x:0,y:0,width:350,height:28),pullsDown:false)
        for task in current {
            picker.addItem(withTitle:task.file_name +
                (task.mode=="exclusive-edit" ? "（独占）" : "（普通）"))
        }
        alert.accessoryView=picker
        if alert.runModal() != .alertFirstButtonReturn {return}
        let selected=current[max(0,picker.indexOfSelectedItem)]
        // Explicit UI action only. The Go core re-reads its original
        // current-user verified Session and shows its own consent dialog.
        DispatchQueue.global(qos:.userInitiated).async {
            let child=Process()
            child.executableURL=URL(fileURLWithPath:core)
            child.arguments=["--manage-local-task-stdin"]
            let stdin=Pipe()
            child.standardInput=stdin
            child.standardOutput=Pipe()
            child.standardError=Pipe()
            do {
                try child.run()
                let body=try JSONSerialization.data(withJSONObject:["task_id":selected.task_id])
                stdin.fileHandleForWriting.write(body)
                stdin.fileHandleForWriting.closeFile()
                child.waitUntilExit()
            } catch {
                DispatchQueue.main.async {
                    let warning=NSAlert()
                    warning.messageText="无法启动原编辑操作窗口"
                    warning.informativeText="本地文件、Journal 与未决 Intent 均保留。"
                    warning.runModal()
                }
            }
        }
    }

    @objc private func quit() { NSApp.terminate(nil) }
    @objc private func refreshMenu() { refreshTasks() }
    @objc private func showWindow() {
        listWindow.makeKeyAndOrderFront(nil)
        NSApp.activate(ignoringOtherApps: true)
    }
    @objc private func toggleWindow() {
        if listWindow.isVisible { listWindow.orderOut(nil) }
        else { showWindow() }
    }

    // Read the same per-user Go task preference used by Windows. There is no
    // second GUI-only settings database and no HTTP/Chrome registration.
    private func loadFloatPosition() -> NSPoint? {
        let proc = Process()
        proc.executableURL = taskcenterBinary
        proc.arguments = ["--get-preferences"]
        let pipe = Pipe()
        proc.standardOutput = pipe
        proc.standardError = Pipe()
        do {
            try proc.run()
            let data = pipe.fileHandleForReading.readDataToEndOfFile()
            proc.waitUntilExit()
            guard proc.terminationStatus == 0,
                  let point = try JSONDecoder().decode(TaskPreferences.self, from: data).float_position else {
                return nil
            }
            return NSPoint(x: point.x, y: point.y)
        } catch { return nil }
    }

    private func saveFloatPosition(_ point: NSPoint) {
        let executable = taskcenterBinary
        let screen = NSScreen.main?.localizedName ?? "main"
        DispatchQueue.global(qos: .utility).async {
            let proc = Process()
            proc.executableURL = executable
            proc.arguments = ["--set-position", "--x", String(Int(point.x)),
                              "--y", String(Int(point.y)), "--display", "mac:" + screen]
            proc.standardOutput = Pipe()
            proc.standardError = Pipe()
            do { try proc.run(); proc.waitUntilExit() } catch { }
        }
    }

    private func createFloat() {
        let size: CGFloat = 48
        let screen = NSScreen.main?.visibleFrame ?? NSRect(x: 0, y: 0, width: 1024, height: 768)
        let origin = loadFloatPosition() ??
            NSPoint(x: screen.maxX - size - 12, y: screen.midY)
        let frame = NSRect(x: max(screen.minX, min(screen.maxX - size, origin.x)),
                           y: max(screen.minY, min(screen.maxY - size, origin.y)),
                           width: size, height: size)
        let panel = NSPanel(contentRect: frame, styleMask: [.borderless, .nonactivatingPanel],
                            backing: .buffered, defer: false)
        panel.level = .floating
        panel.isOpaque = false
        panel.backgroundColor = .clear
        panel.hasShadow = true
        panel.collectionBehavior = [.canJoinAllSpaces]
        let handle = FloatingHandle(frame: NSRect(x: 0, y: 0, width: size, height: size))
        handle.title = "☁"
        handle.bezelStyle = .regularSquare
        handle.isBordered = false
        handle.wantsLayer = true
        handle.layer?.cornerRadius = size / 2
        handle.layer?.backgroundColor = NSColor.controlAccentColor.cgColor
        handle.contentTintColor = .white
        handle.font = NSFont.systemFont(ofSize: 22, weight: .medium)
        handle.toolTip = "CloudFile 文件任务（点击展开，拖动移动）"
        handle.target = self
        handle.action = #selector(toggleWindow)
        handle.onDrop = { [weak self, weak panel] in
            guard let panel = panel else { return }
            self?.saveFloatPosition(panel.frame.origin)
        }
        panel.contentView = handle
        floating = panel
    }

    private func refreshTasks() {
        if refreshing { return }
        refreshing = true
        let binary = taskcenterBinary
        DispatchQueue.global(qos: .utility).async { [weak self] in
            let p = Process()
            p.executableURL = binary
            p.arguments = ["--list"]
            let output = Pipe()
            p.standardOutput = output
            p.standardError = Pipe()
            var entries: [TaskEntry] = []
            var active = 0
            var uncertain = 0
            var failure: String?
            do {
                try p.run()
                let data = output.fileHandleForReading.readDataToEndOfFile()
                p.waitUntilExit()
                if p.terminationStatus != 0 { throw NSError(domain: "CloudFileTaskCenter", code: Int(p.terminationStatus)) }
                let response = try JSONDecoder().decode(TaskList.self, from: data)
                entries = response.tasks
                active = response.active_exclusive
                uncertain = response.uncertain_exclusive
            } catch {
                failure = "本地任务读取失败；请检查 Agent Core 与本地目录。"
            }
            DispatchQueue.main.async {
                self?.refreshing = false
                self?.activeExclusive = active
                self?.uncertainExclusive = uncertain
                self?.render(entries: entries, failure: failure)
            }
        }
    }

    private func render(entries: [TaskEntry], failure: String?) {
        if let failure = failure {
            content.string = failure + "\n\n保持本地修改，不执行自动上传。"
            return
        }
        tasks = entries
        highlightFocusedTask()
        var lines = ["已管理 \(entries.count) 个本地编辑任务", "本地保存不会自动上传，所有云端操作仍需明确确认。", ""]
        for (i, t) in entries.enumerated() {
            let state = t.needs_review ? "⚠ 需要检查" : (t.local_changed ? "● 本地已修改，尚未上传" : "本地编辑中")
            let mode = t.mode == "exclusive-edit" ? "独占编辑" : "普通编辑"
            lines.append("\(i + 1). \(t.file_name) — \(state)")
            lines.append("    \(mode) · \(t.path)")
            lines.append("    库：\(t.repo_id)")
            lines.append("")
        }
        if activeExclusive + uncertainExclusive > 0 {
            lines.append("独占任务：\(activeExclusive) 个活动，\(uncertainExclusive) 个状态待检查")
        }
        if let warning = lastLeaseWarning { lines.append("⚠ " + warning) }
        if entries.isEmpty { lines.append("当前没有本地编辑任务。") }
        content.string = lines.joined(separator: "\n")
        if !entries.isEmpty {
            if floating == nil { createFloat() }
            floating?.orderFrontRegardless()
        } else {
            floating?.orderOut(nil)
        }
    }
}

let arguments = CommandLine.arguments
// A portable unsigned .app can be opened directly in Finder without passing
// command-line arguments. Its companion Go CLI is inside Contents/MacOS.
// Explicit --taskcenter-bin remains available for development and recovery.
let embeddedBinary = Bundle.main.executableURL?.deletingLastPathComponent()
    .appendingPathComponent("cloudfile-taskcenter")
let defaultBinary = embeddedBinary?.path ?? ""
let targetBinary: String
let originalCore: String?
if arguments.count == 1 {
    targetBinary = defaultBinary
    // Full v0.8 green package ships the ORIGINAL device-signing Core next
    // to .app, not inside its writable task index. No browser input used.
    let companion = Bundle.main.bundleURL.deletingLastPathComponent()
        .appendingPathComponent("cloudfile-agent2").path
    originalCore = FileManager.default.isExecutableFile(atPath: companion) ? companion : nil
} else if (arguments.count == 3 || arguments.count == 5) &&
          arguments[1] == "--taskcenter-bin" && arguments[2].hasPrefix("/") &&
          (arguments.count == 3 || (arguments[3] == "--go-core-bin" &&
                                     arguments[4].hasPrefix("/"))) {
    targetBinary = arguments[2]
    originalCore = arguments.count == 5 ? arguments[4] : nil
} else {
    fputs("Usage: CloudFileTaskCenterPreview [--taskcenter-bin /absolute/path/to/cloudfile-taskcenter [--go-core-bin /absolute/path/to/installed-core]]\n", stderr)
    exit(2)
}
guard FileManager.default.isExecutableFile(atPath: targetBinary) else {
    fputs("CloudFile portable TaskCenter companion is unavailable; keep the .app bundle intact.\n", stderr)
    exit(2)
}
let lockDirectory = FileManager.default.homeDirectoryForCurrentUser
    .appendingPathComponent("Library/Application Support/CloudFileTaskCenterPreview")
try? FileManager.default.createDirectory(at: lockDirectory,
    withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
let lockFile = lockDirectory.appendingPathComponent("preview.lock")
let lockFD = open(lockFile.path, O_RDWR | O_CREAT | O_NOFOLLOW, 0o600)
guard lockFD >= 0, flock(lockFD, LOCK_EX | LOCK_NB) == 0 else {
    fputs("CloudFile TaskCenter preview is already running or lock is unavailable.\n", stderr)
    exit(1)
}
let app = NSApplication.shared
let delegate = TaskCenterPreview(binary: URL(fileURLWithPath: targetBinary),
    core: originalCore)
app.delegate = delegate
app.run()

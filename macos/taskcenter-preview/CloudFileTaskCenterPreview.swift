// CloudFile v0.8 macOS read-only UI preview.
// This is a local AppKit adapter, not a second Native Messaging Host.
// No OAuth, Checkout, Commit, upload, renewal, or browser authority lives here.
import AppKit
import Foundation

struct TaskList: Decodable {
    let tasks: [TaskEntry]
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
    private var status: NSStatusItem?
    private let listWindow = NSWindow(
        contentRect: NSRect(x: 0, y: 0, width: 480, height: 360),
        styleMask: [.titled, .closable, .miniaturizable, .resizable],
        backing: .buffered, defer: false)
    private let content = NSTextView()
    private var floating: NSPanel?
    private var refreshing = false
    private var tasks: [TaskEntry] = []
    private var timer: Timer?

    init(binary: URL) { self.taskcenterBinary = binary }

    func applicationDidFinishLaunching(_ notification: Notification) {
        NSApp.setActivationPolicy(.accessory)
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
        menu.addItem(.separator())
        let exit = NSMenuItem(title: "退出预览", action: #selector(quit), keyEquivalent: "")
        exit.target = self
        menu.addItem(exit)
        item.menu = menu
        status = item

        refreshTasks()
        timer = Timer.scheduledTimer(withTimeInterval: 3, repeats: true) { [weak self] _ in
            self?.refreshTasks()
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

    private func createFloat() {
        let size: CGFloat = 48
        let screen = NSScreen.main?.visibleFrame ?? NSRect(x: 0, y: 0, width: 1024, height: 768)
        let defaults = UserDefaults.standard
        let hasPosition = defaults.object(forKey: "CloudFileFloatX") != nil
        let origin = NSPoint(
            x: hasPosition ? defaults.double(forKey: "CloudFileFloatX") : screen.maxX - size - 12,
            y: hasPosition ? defaults.double(forKey: "CloudFileFloatY") : screen.midY)
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
        handle.onDrop = { [weak panel] in
            guard let panel = panel else { return }
            UserDefaults.standard.set(Double(panel.frame.origin.x), forKey: "CloudFileFloatX")
            UserDefaults.standard.set(Double(panel.frame.origin.y), forKey: "CloudFileFloatY")
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
            var failure: String?
            do {
                try p.run()
                let data = output.fileHandleForReading.readDataToEndOfFile()
                p.waitUntilExit()
                if p.terminationStatus != 0 { throw NSError(domain: "CloudFileTaskCenter", code: Int(p.terminationStatus)) }
                entries = try JSONDecoder().decode(TaskList.self, from: data).tasks
            } catch {
                failure = "本地任务读取失败；请检查 Agent Core 与本地目录。"
            }
            DispatchQueue.main.async {
                self?.refreshing = false
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
        var lines = ["已管理 \(entries.count) 个本地编辑任务", "本地保存不会自动上传，所有云端操作仍需明确确认。", ""]
        for (i, t) in entries.enumerated() {
            let state = t.needs_review ? "⚠ 需要检查" : (t.local_changed ? "● 本地已修改，尚未上传" : "本地编辑中")
            let mode = t.mode == "exclusive-edit" ? "独占编辑" : "普通编辑"
            lines.append("\(i + 1). \(t.file_name) — \(state)")
            lines.append("    \(mode) · \(t.path)")
            lines.append("    库：\(t.repo_id)")
            lines.append("")
        }
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
guard arguments.count == 3, arguments[1] == "--taskcenter-bin",
      arguments[2].hasPrefix("/") else {
    fputs("Usage: CloudFileTaskCenterPreview --taskcenter-bin /absolute/path/to/cloudfile-taskcenter\n", stderr)
    exit(2)
}
let app = NSApplication.shared
let delegate = TaskCenterPreview(binary: URL(fileURLWithPath: arguments[2]))
app.delegate = delegate
app.run()

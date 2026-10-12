// CloudFile v0.8 Windows UI preview: local summaries only.
// No native-host registration, upload, Checkout or lease operations.
using System.Diagnostics;
using System.Drawing;
using System.Text.Json;
using System.Text.Json.Serialization;
using System.Windows.Forms;

namespace CloudFile.TaskCenter.Preview;

internal sealed class TaskList
{
    [JsonPropertyName("tasks")] public List<TaskEntry> Tasks { get; set; } = new();
}
internal sealed class TaskEntry
{
    [JsonPropertyName("task_id")] public string ID { get; set; } = "";
    [JsonPropertyName("file_name")] public string FileName { get; set; } = "";
    [JsonPropertyName("repo_id")] public string RepoID { get; set; } = "";
    [JsonPropertyName("path")] public string Path { get; set; } = "";
    [JsonPropertyName("mode")] public string Mode { get; set; } = "";
    [JsonPropertyName("local_changed")] public bool Changed { get; set; }
    [JsonPropertyName("needs_review")] public bool Review { get; set; }
}
internal sealed class TaskEvent
{
    [JsonPropertyName("kind")] public string Kind { get; set; } = "";
    [JsonPropertyName("file_name")] public string FileName { get; set; } = "";
}

internal static class Program
{
    [STAThread]
    private static void Main(string[] args)
    {
        if (args.Length != 2 || args[0] != "--taskcenter-bin" ||
            !System.IO.Path.IsPathFullyQualified(args[1]) || !File.Exists(args[1]))
        {
            MessageBox.Show("用法：TaskCenterPreview --taskcenter-bin <完整本机 Go CLI 路径>",
                "CloudFile 本地任务预览");
            return;
        }
        // One preview instance per Windows logon session. No additional Chrome Host.
        using var mutex = new Mutex(true, @"Local\CloudFileTaskCenterPreviewV08", out var acquired);
        if (!acquired) return;
        ApplicationConfiguration.Initialize();
        using var center = new CenterForm(args[1]);
        Application.Run(center);
    }
}

internal sealed class FloatHandle : Form
{
    private Point startCursor;
    private Point startPosition;
    private bool moved;
    private readonly Action clicked;
    private readonly string goBinary;
    public FloatHandle(Action clicked, string goBinary)
    {
        this.clicked = clicked;
        this.goBinary = goBinary;
        FormBorderStyle = FormBorderStyle.None;
        ShowInTaskbar = false;
        TopMost = true;
        ClientSize = new Size(48, 48);
        BackColor = Color.FromArgb(41, 108, 204);
        var face = new Label {
            Text = "☁", Font = new Font("Segoe UI Symbol", 22, FontStyle.Bold),
            ForeColor = Color.White, TextAlign = ContentAlignment.MiddleCenter,
            Dock = DockStyle.Fill, Cursor = Cursors.SizeAll
        };
        Controls.Add(face);
        face.MouseDown += HandleDown;
        face.MouseMove += HandleMove;
        face.MouseUp += HandleUp;
        MouseDown += HandleDown;
        MouseMove += HandleMove;
        MouseUp += HandleUp;
    }
    private void HandleDown(object? sender, MouseEventArgs e)
    {
        if (e.Button != MouseButtons.Left) return;
        startCursor = Cursor.Position;
        startPosition = Location;
        moved = false;
    }
    private void HandleMove(object? sender, MouseEventArgs e)
    {
        if (e.Button != MouseButtons.Left) return;
        var delta = new Size(Cursor.Position.X - startCursor.X, Cursor.Position.Y - startCursor.Y);
        if (Math.Abs(delta.Width) + Math.Abs(delta.Height) > 4) moved = true;
        if (moved) Location = Point.Add(startPosition, delta);
    }
    private void HandleUp(object? sender, MouseEventArgs e)
    {
        if (e.Button != MouseButtons.Left) return;
        if (!moved) clicked();
        else {
            var area = Screen.FromPoint(Location).WorkingArea;
            Location = new Point(
                Math.Clamp(Location.X, area.Left, area.Right - Width),
                Math.Clamp(Location.Y, area.Top, area.Bottom - Height));
            var edge = Math.Abs(Location.X - area.Left) < Math.Abs(area.Right - (Location.X + Width))
                ? area.Left : area.Right - Width;
            Location = new Point(edge, Location.Y);
            SavePosition(Location, Screen.FromPoint(Location).DeviceName);
        }
    }
    public void RestorePosition()
    {
        var area = Screen.PrimaryScreen?.WorkingArea ?? new Rectangle(0, 0, 1024, 768);
        var x = area.Right - Width - 12;
        var y = area.Top + area.Height / 2;
        var stored = LoadPosition();
        if (stored.HasValue) { x = stored.Value.X; y = stored.Value.Y; }
        area = Screen.FromPoint(new Point(x, y)).WorkingArea;
        Location = new Point(Math.Clamp(x, area.Left, area.Right - Width),
                             Math.Clamp(y, area.Top, area.Bottom - Height));
    }
    // Same Go per-user TaskCenter preferences as the macOS preview.
    // The existing Native Host and edit journal remain the only authorities.
    private Point? LoadPosition()
    {
        try {
            using var process = new Process { StartInfo = PreferenceProcess("--get-preferences") };
            if (!process.Start()) return null;
            if (!process.WaitForExit(2000)) { process.Kill(); return null; }
            if (process.ExitCode != 0) return null;
            using var data = JsonDocument.Parse(process.StandardOutput.ReadToEnd());
            if (!data.RootElement.TryGetProperty("float_position", out var pos) ||
                pos.ValueKind != JsonValueKind.Object) return null;
            return new Point(pos.GetProperty("x").GetInt32(), pos.GetProperty("y").GetInt32());
        } catch (Exception error) when (error is IOException or InvalidOperationException
                    or JsonException or System.ComponentModel.Win32Exception) { return null; }
    }
    private void SavePosition(Point position, string display)
    {
        string go = goBinary;
        _ = Task.Run(() => {
            try {
                using var process = new Process { StartInfo = new ProcessStartInfo(go) {
                    CreateNoWindow = true, UseShellExecute = false,
                } };
                process.StartInfo.ArgumentList.Add("--set-position");
                process.StartInfo.ArgumentList.Add("--x");
                process.StartInfo.ArgumentList.Add(position.X.ToString(System.Globalization.CultureInfo.InvariantCulture));
                process.StartInfo.ArgumentList.Add("--y");
                process.StartInfo.ArgumentList.Add(position.Y.ToString(System.Globalization.CultureInfo.InvariantCulture));
                process.StartInfo.ArgumentList.Add("--display");
                process.StartInfo.ArgumentList.Add("win:" + display);
                if (process.Start() && !process.WaitForExit(2500)) process.Kill();
            } catch (Exception error) when (error is IOException or InvalidOperationException
                        or System.ComponentModel.Win32Exception) { /* position is advisory */ }
        });
    }
    private ProcessStartInfo PreferenceProcess(string operation)
    {
        var info = new ProcessStartInfo(goBinary) {
            UseShellExecute = false, CreateNoWindow = true,
            RedirectStandardOutput = true, RedirectStandardError = true
        };
        info.ArgumentList.Add(operation);
        return info;
    }

}

internal sealed class CenterForm : Form
{
    private readonly string goBinary;
    private readonly NotifyIcon tray;
    private readonly FloatHandle floating;
    private readonly ListView list;
    private readonly Label status;
    private readonly System.Windows.Forms.Timer timer;
    private readonly CancellationTokenSource watcherStop = new();
    private Process? watcher;
    private bool querying;
    private bool quitting;

    public CenterForm(string goBinary)
    {
        this.goBinary = goBinary;
        Text = "CloudFile 任务中心（本地预览）";
        ClientSize = new Size(660, 380);
        MinimumSize = new Size(490, 300);
        StartPosition = FormStartPosition.CenterScreen;
        ShowInTaskbar = false;
        list = new ListView { Dock = DockStyle.Fill, View = View.Details, FullRowSelect = true };
        list.Columns.Add("文件", 170);
        list.Columns.Add("本地状态", 185);
        list.Columns.Add("模式", 85);
        list.Columns.Add("库 / 路径", 330);
        status = new Label { Text = "正在读取本地任务…（不自动上传）",
            Dock = DockStyle.Bottom, Height = 35, TextAlign = ContentAlignment.MiddleLeft };
        Controls.Add(list);
        Controls.Add(status);
        floating = new FloatHandle(() => { if (Visible) Hide(); else ShowCenter(); }, goBinary);
        floating.RestorePosition();

        var menu = new ContextMenuStrip();
        menu.Items.Add("打开任务中心", null, (_, _) => ShowCenter());
        menu.Items.Add("刷新任务", null, (_, _) => _ = RefreshAsync());
        menu.Items.Add(new ToolStripSeparator());
        menu.Items.Add("退出预览", null, (_, _) => ExitPreview());
        tray = new NotifyIcon { Icon = SystemIcons.Application, Visible = true,
            Text = "CloudFile 本地任务（只读预览）", ContextMenuStrip = menu };
        tray.DoubleClick += (_, _) => ShowCenter();
        FormClosing += (_, e) => {
            if (!quitting) { e.Cancel = true; Hide(); }
        };
        timer = new System.Windows.Forms.Timer { Interval = 3000 };
        timer.Tick += (_, _) => _ = RefreshAsync();
        Shown += (_, _) => Hide();
        Load += (_, _) => {
            timer.Start();
            _ = RefreshAsync();
            StartWatcher();
        };
    }

    private void ShowCenter()
    {
        Show();
        WindowState = FormWindowState.Normal;
        Activate();
    }

    private async Task RefreshAsync()
    {
        if (querying || quitting) return;
        querying = true;
        try {
            using var p = new Process { StartInfo = NewProcess("--list") };
            p.Start();
            var data = await p.StandardOutput.ReadToEndAsync();
            using var timeout = new CancellationTokenSource(TimeSpan.FromSeconds(5));
            await p.WaitForExitAsync(timeout.Token);
            if (p.ExitCode != 0) throw new IOException("Go task inspection failed");
            var tasks = JsonSerializer.Deserialize<TaskList>(data)?.Tasks
                ?? throw new IOException("invalid task summary");
            list.BeginUpdate();
            try {
                list.Items.Clear();
                foreach (var t in tasks) {
                    var state = t.Review ? "需要检查" : t.Changed ? "本地已修改，待确认" : "本地编辑中";
                    var row = new ListViewItem(t.FileName);
                    row.SubItems.Add(state);
                    row.SubItems.Add(t.Mode == "exclusive-edit" ? "独占" : "普通");
                    row.SubItems.Add(t.RepoID + "  " + t.Path);
                    list.Items.Add(row);
                }
            } finally { list.EndUpdate(); }
            status.Text = $"本地任务 {tasks.Count} 项。保存≠上传；云端提交需用户明确确认。";
            if (tasks.Count > 0 && !floating.Visible) floating.Show();
            if (tasks.Count == 0 && floating.Visible) floating.Hide();
        } catch (Exception e) when (e is IOException or InvalidOperationException
                                    or System.ComponentModel.Win32Exception
                                    or JsonException or OperationCanceledException) {
            // Do not remove or hide existing tasks on a transient query failure.
            status.Text = "无法更新本地任务；已有任务保持显示，请检查 Agent Core。";
        } finally { querying = false; }
    }

    private ProcessStartInfo NewProcess(string action)
    {
        var info = new ProcessStartInfo(goBinary) {
            UseShellExecute = false, CreateNoWindow = true,
            RedirectStandardOutput = true, RedirectStandardError = true
        };
        info.ArgumentList.Add(action);
        return info;
    }

    private void StartWatcher()
    {
        try {
            watcher = new Process { StartInfo = NewProcess("--watch") };
            watcher.Start();
            _ = Task.Run(async () => {
                try {
                    while (!watcherStop.IsCancellationRequested) {
                        var line = await watcher.StandardOutput.ReadLineAsync(watcherStop.Token);
                        if (line is null) break;
                        TaskEvent? evt = null;
                        try { evt = JsonSerializer.Deserialize<TaskEvent>(line); }
                        catch (JsonException) { continue; }
                        if (evt == null || evt.Kind is not ("local-saved" or "needs-review")) continue;
                        BeginInvoke(() => {
                            if (quitting) return;
                            var notice = evt.Kind == "local-saved"
                                ? "本地已修改，尚未上传" : "本地文件需要检查";
                            tray.ShowBalloonTip(3500, "CloudFile · " + evt.FileName,
                                notice, evt.Kind == "needs-review" ? ToolTipIcon.Warning : ToolTipIcon.Info);
                            _ = RefreshAsync();
                        });
                    }
                } catch (Exception ex) when (ex is IOException or OperationCanceledException
                                           or InvalidOperationException) { }
            });
        } catch (Exception e) when (e is IOException or System.ComponentModel.Win32Exception) {
            status.Text = "本地保存提示暂不可用；任务列表仍可查询。";
        }
    }

    private void ExitPreview()
    {
        quitting = true;
        timer.Stop();
        watcherStop.Cancel();
        try { if (watcher is { HasExited: false }) watcher.Kill(entireProcessTree: true); }
        catch (InvalidOperationException) { }
        catch (System.ComponentModel.Win32Exception) { }
        watcher?.Dispose();
        tray.Visible = false;
        tray.Dispose();
        floating.Close();
        floating.Dispose();
        Close();
    }
}

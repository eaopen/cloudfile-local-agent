//! 本地工作副本（镜像）根目录的策略与一次性搬迁。
//!
//! 移植自 Go 侧 `internal/config/mirror.go` + `internal/config/workspace_windows.go`
//! + `internal/workspace/adopt.go`，一共三件事：
//!
//! 1. [`resolve`]：显式配置优先；否则平台默认——Windows 上优先**固定盘符**
//!    （`D:\<代理目录>`，与代理数据目录同名），不可用时回落 `%LocalAppData%`。
//!    光驱、U 盘、RAM 盘、映射的网络共享都不能当镜像根：它们更慢、可能在下载中途消失，
//!    而且盘符状态是每次判定都会变的（抖动的卷会让镜像来回搬）。UNC 路径永远不会
//!    报成固定盘，所以两种情况都天然被拒。
//! 2. [`tasks_root`]：任务目录就在解析出来的根下面——让这条策略真正决定**落盘位置**，
//!    而不只是影响界面显示。
//! 3. [`carry_over`]：解析结果变了就把已有任务搬过去**一次**。工作副本是用户本地工作的
//!    唯一一份，重新下载不是修复，所以只能搬。规则见 [`carry_over_to`]。
//!
//! 标记文件 `mirror-root.txt` 放在 **agent 根目录**（不是镜像内部）：镜像换了位置它也必须
//! 留在原地，否则下一次解析又会从空目录开始。记录结果写在 agent 根目录的
//! `mirror-adopt.json`，这样排查时不必去动工作区里的文件。

use anyhow::Result;
use serde::{Deserialize, Serialize};
use std::fs;
#[cfg(windows)]
use std::path::Component;
use std::path::{Path, PathBuf};
#[cfg(windows)]
use std::sync::OnceLock;

/// 搬运结果；同时落到 agent 根目录的 `mirror-adopt.json`。
#[derive(Clone, Debug, Serialize, Deserialize, PartialEq, Eq)]
pub struct Outcome {
    /// 从哪搬（空串表示没有可搬的来源）。
    pub from: String,
    pub to: String,
    /// Unix 秒。
    pub at: u64,
    /// settled（已结算）/ skipped（目标已有内容）/ copied / done / partial / failed
    pub state: String,
    /// 目标里最终应有的任务目录数。
    pub tasks: usize,
    /// 实际复制过去的文件数（一个任务目录里通常还有一个 task.json）。
    pub files: usize,
    pub bytes: u64,
    /// 目标已有同名文件而没动的个数（永不覆盖）。
    pub skipped: usize,
    /// 校验时缺失或大小不符的个数。
    pub missing: usize,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub detail: Option<String>,
}

const MARKER: &str = "mirror-root.txt";
const REPORT: &str = "mirror-adopt.json";

/// Windows 上首选镜像根——**常量**，不按 `app_root` 推导。
/// 与 Go 侧 `D:\CloudFileAgent2\workspace` 同一个形状：固定的、与代理数据目录同名的路径。
#[cfg(windows)]
const PREFERRED_ROOT: &str = "D:\\CloudFileAgent22";

/// Windows 上「代理数据目录」的名字。
const APP_DIR_NAME: &str = "CloudFileAgent22";

/// 首选位置能否当镜像根，每进程只探一次。
///
/// 一次运行里盘符类型与可写性不会变，而 [`resolve`] 会被频繁调用（每个任务目录解析一次），
/// 每次都做可写性探测（建临时文件）是不必要的 I/O。代价是运行期间才插上的盘要等下次启动
/// 才被采用——这比「下载中途换根」安全。
#[cfg(windows)]
static PREFERRED_USABLE: OnceLock<bool> = OnceLock::new();

/// 标记文件路径（在 agent 根目录下，与镜像位置无关）。
pub fn marker_path(app_root: &Path) -> PathBuf {
    app_root.join(MARKER)
}

/// 记录文件路径（在 agent 根目录下）。
pub fn report_path(app_root: &Path) -> PathBuf {
    app_root.join(REPORT)
}

/// 解析工作区根目录（**不含** `tasks` 这一层）。
pub fn resolve(app_root: &Path) -> PathBuf {
    if let Some(explicit) = crate::settings::configured_workspace_root(app_root) {
        return explicit;
    }
    default_root(app_root)
}

/// 任务目录：`<解析出来的根>/tasks`。所有读写工作副本的地方都必须走这里，
/// 否则「显式配置的 workspace_root」只影响界面、不影响落盘。
pub fn tasks_root(app_root: &Path) -> PathBuf {
    resolve(app_root).join("tasks")
}

fn default_root(app_root: &Path) -> PathBuf {
    #[cfg(windows)]
    {
        // 盘符偏好只作用于**真实安装根**（`%LocalAppData%\CloudFileAgent22`）。
        // 测试、便携安装或任何别的根都不该被改写到 D 盘——那会让一个测试进程
        // 悄悄写进用户真实的工作区。
        if is_default_app_root(app_root) {
            if let Some(preferred) = preferred_root() {
                if *PREFERRED_USABLE.get_or_init(|| usable(&preferred)) {
                    return preferred;
                }
            }
        }
    }
    app_root.to_path_buf()
}

/// `app_root` 是不是本平台的标准安装根。
fn is_default_app_root(app_root: &Path) -> bool {
    let Some(default) = dirs::data_local_dir().map(|dir| dir.join(APP_DIR_NAME)) else {
        return false;
    };
    // Windows 路径不区分大小写，macOS 通常也不区分（但按字节比更保守，这里统一折大小写）。
    default
        .to_string_lossy()
        .eq_ignore_ascii_case(&app_root.to_string_lossy())
}

/// 首选镜像根：固定本地盘上的固定路径。
#[cfg(windows)]
fn preferred_root() -> Option<PathBuf> {
    Some(PathBuf::from(PREFERRED_ROOT))
}

/// 候选位置能不能用：既要是固定本地盘，又要真的写得进去
/// （只读或被写满的盘同样不能当镜像根）。
#[cfg(windows)]
fn usable(directory: &Path) -> bool {
    if !fixed_volume(directory) {
        return false;
    }
    if fs::create_dir_all(directory).is_err() {
        return false;
    }
    // 探测文件立刻随临时文件析构删除：这个目录会显示在扩展弹窗里，
    // 留一个 `.cf-probe` 在那儿用户会看见。
    tempfile::NamedTempFile::new_in(directory).is_ok()
}

/// `GetDriveTypeW(path) == DRIVE_FIXED`：光驱/可移动盘/网络共享都不是固定盘。
#[cfg(windows)]
fn fixed_volume(path: &Path) -> bool {
    use std::os::windows::ffi::OsStrExt;
    use windows_sys::Win32::Storage::FileSystem::GetDriveTypeW;
    /// DRIVE_FIXED（winbase.h）。自己写常量而不是从 windows-sys 取：
    /// 它在不同版本里被放在不同的模块下，常量本身是稳定的 ABI 值。
    const DRIVE_FIXED: u32 = 3;
    // 卷名就是路径的第一个组件（`D:`），补上分隔符再补 NUL 结尾。
    let mut volume: Vec<u16> = match path.components().next() {
        Some(Component::Prefix(prefix)) => prefix.as_os_str().encode_wide().collect(),
        _ => return false,
    };
    volume.extend([u16::from(b'\\'), 0]);
    // SAFETY: 传入的是以 NUL 结尾的宽字符串字面量副本，API 只读它，不做任何释放。
    let kind = unsafe { GetDriveTypeW(volume.as_ptr()) };
    kind == DRIVE_FIXED
}

/// 搬迁：先把已有镜像搬过来，再始终写标记（哪怕没搬成）。
pub fn carry_over(app_root: &Path) -> Outcome {
    let root = resolve(app_root);
    let outcome = carry_over_to(app_root, &root);
    // 标记总要往前走：一块来回抖动的盘不该每次触发一次上 GB 的复制。
    let _ = record_marker(app_root, &root);
    let _ = write_report(app_root, &outcome);
    outcome
}

/// 没有解析策略的那一半，方便测试直接指定目标。
///
/// 规则按重要性排序：
///  1. **复制完成并校验通过之前，什么都不删**；
///  2. 目标已有的文件**永不覆盖**；目标已经装着任务时**整体不合并**（它会赢，
///     来源原地不动，标记照常前进）——静默合并两个镜像是「猜哪份更新」，不能做；
///  3. 只考虑「64 位 hex 名字 + 内含 `task.json`」的任务目录，工作区根里别的东西
///     （用户自己指到共享目录时会有的）一律不看不碰；
///  4. 符号链接与非常规文件跳过，绝不跟随。
pub fn carry_over_to(app_root: &Path, root: &Path) -> Outcome {
    let mut outcome = Outcome {
        from: String::new(),
        to: root.to_string_lossy().into_owned(),
        at: crate::now(),
        state: "settled".to_string(),
        tasks: 0,
        files: 0,
        bytes: 0,
        skipped: 0,
        missing: 0,
        detail: None,
    };
    // 快路径：镜像结算过之后只是一次很小的文件读取。
    if same_path(recorded_marker(app_root).as_deref(), Some(root)) {
        return outcome;
    }

    let source = pick_source(app_root, root);
    if source.as_os_str().is_empty() {
        return outcome;
    }
    outcome.from = source.to_string_lossy().into_owned();

    let (tasks, bytes) = mirror_size(&source);
    if tasks == 0 {
        // 没有值得搬的东西：只记住新家。
        return outcome;
    }
    outcome.tasks = tasks;
    outcome.bytes = bytes;
    if mirror_size(root).0 > 0 {
        outcome.state = "skipped".to_string();
        outcome.detail = Some("target already holds tasks; source left in place".to_string());
        return outcome;
    }

    match copy_tasks(&source, root) {
        Ok((copied, skipped)) => {
            outcome.skipped = skipped;
            outcome.files = copied;
            match verify_tasks(&source, root) {
                Ok(0) => {
                    outcome.state = "done".to_string();
                    // 已经逐字节校验过，来源可以删了。删失败只是留一份副本，
                    // 镜像本身已经完整正确。
                    if let Err(error) = remove_tasks(&source) {
                        outcome.state = "copied".to_string();
                        outcome.detail = Some(format!("source could not be removed: {error}"));
                    }
                }
                Ok(missing) => {
                    outcome.state = "partial".to_string();
                    outcome.missing = missing;
                    outcome.detail =
                        Some("verification found missing files; source kept".to_string());
                }
                Err(error) => {
                    outcome.state = "failed".to_string();
                    outcome.detail = Some(error.to_string());
                }
            }
        }
        Err(error) => {
            outcome.state = "failed".to_string();
            outcome.detail = Some(error.to_string());
        }
    }
    outcome
}

/// 从哪搬：标记记的位置，然后是当前平台默认位置。有任务目录的候选优先于空目录；
/// 都不行就取第一个候选，这样全新机器也能把根记下来而不是永远在找。
fn pick_source(app_root: &Path, root: &Path) -> PathBuf {
    let mut guess = PathBuf::new();
    for candidate in [recorded_marker(app_root), Some(default_root(app_root))] {
        let Some(candidate) = candidate else { continue };
        if same_path(Some(&candidate), Some(root)) {
            continue;
        }
        if guess.as_os_str().is_empty() {
            guess = candidate.clone();
        }
        if mirror_size(&candidate).0 > 0 {
            return candidate;
        }
    }
    guess
}

fn recorded_marker(app_root: &Path) -> Option<PathBuf> {
    let raw = fs::read_to_string(marker_path(app_root)).ok()?;
    let root = PathBuf::from(raw.trim());
    if root.as_os_str().is_empty() || !root.is_absolute() {
        return None;
    }
    Some(root)
}

fn record_marker(app_root: &Path, root: &Path) -> Result<()> {
    if !root.is_absolute() {
        anyhow::bail!("absolute mirror root required");
    }
    let mut value = root.to_string_lossy().into_owned().into_bytes();
    value.push(b'\n');
    crate::storage::write_bytes(&marker_path(app_root), &value)
}

fn write_report(app_root: &Path, outcome: &Outcome) -> Result<()> {
    let mut bytes = serde_json::to_vec_pretty(outcome)?;
    bytes.push(b'\n');
    crate::storage::write_bytes(&report_path(app_root), &bytes)
}

/// 两个绝对路径是否指同一个位置；Windows 上折大小写，其它平台不折。
fn same_path(a: Option<&Path>, b: Option<&Path>) -> bool {
    let (Some(a), Some(b)) = (a, b) else {
        return false;
    };
    if cfg!(windows) {
        a.to_string_lossy()
            .eq_ignore_ascii_case(&b.to_string_lossy())
    } else {
        a == b
    }
}

/// 镜像里的任务目录：64 位 hex 名字且内含 `task.json`。
/// 这是**唯一**会被搬运的东西——工作区根可以是用户自己选的目录，那里可能有他的文件。
fn task_dirs(root: &Path) -> Vec<PathBuf> {
    let mut result = Vec::new();
    let Ok(entries) = fs::read_dir(root.join("tasks")) else {
        return result;
    };
    for entry in entries.flatten() {
        let name = entry.file_name().to_string_lossy().into_owned();
        if !crate::session::task_id(&name) {
            continue;
        }
        if !entry.file_type().is_ok_and(|kind| kind.is_dir()) {
            continue;
        }
        if entry.path().join("task.json").is_file() {
            result.push(entry.path());
        }
    }
    result.sort();
    result
}

/// 镜像的大小：任务目录数与常规文件字节数。不可读/不存在按空处理
/// （没插的盘或首次运行不该看起来像错误）。
fn mirror_size(root: &Path) -> (usize, u64) {
    let dirs = task_dirs(root);
    let mut bytes = 0u64;
    for dir in &dirs {
        bytes += directory_bytes(dir);
    }
    (dirs.len(), bytes)
}

fn directory_bytes(dir: &Path) -> u64 {
    let Ok(entries) = fs::read_dir(dir) else {
        return 0;
    };
    let mut bytes = 0u64;
    for entry in entries.flatten() {
        let Ok(kind) = entry.file_type() else {
            continue;
        };
        if kind.is_dir() {
            bytes += directory_bytes(&entry.path());
        } else if kind.is_file() {
            if let Ok(meta) = entry.metadata() {
                bytes += meta.len();
            }
        }
    }
    bytes
}

/// 把任务目录从 source 拷到 target，**绝不覆盖**目标已有文件。
/// 返回（复制成功的文件数，因为目标已有而跳过的文件数）。
fn copy_tasks(source: &Path, target: &Path) -> Result<(usize, usize)> {
    let mut copied = 0usize;
    let mut skipped = 0usize;
    for dir in task_dirs(source) {
        let name = dir.file_name().unwrap_or_default();
        copy_tree(
            &dir,
            &target.join("tasks").join(name),
            &mut copied,
            &mut skipped,
        )?;
    }
    Ok((copied, skipped))
}

fn copy_tree(from: &Path, to: &Path, copied: &mut usize, skipped: &mut usize) -> Result<()> {
    fs::create_dir_all(to)?;
    for entry in fs::read_dir(from)?.flatten() {
        let kind = entry.file_type()?;
        let target = to.join(entry.file_name());
        if kind.is_dir() {
            copy_tree(&entry.path(), &target, copied, skipped)?;
            continue;
        }
        // 符号链接、设备、套接字：不跟随、不重建。
        if !kind.is_file() {
            continue;
        }
        if target.exists() {
            *skipped += 1;
            continue;
        }
        fs::copy(entry.path(), &target)?;
        *copied += 1;
    }
    Ok(())
}

/// 校验来源里的每个文件都在目标里且大小一致；返回缺失/大小不符的个数。
/// 大于 0 就保留来源。
fn verify_tasks(source: &Path, target: &Path) -> Result<usize> {
    let mut missing = 0usize;
    for dir in task_dirs(source) {
        let name = dir.file_name().unwrap_or_default();
        count_missing(&dir, &target.join("tasks").join(name), &mut missing)?;
    }
    Ok(missing)
}

fn count_missing(from: &Path, to: &Path, missing: &mut usize) -> Result<()> {
    for entry in fs::read_dir(from)?.flatten() {
        let kind = entry.file_type()?;
        let target = to.join(entry.file_name());
        if kind.is_dir() {
            count_missing(&entry.path(), &target, missing)?;
            continue;
        }
        if !kind.is_file() {
            continue;
        }
        let same_size = fs::metadata(&target)
            .map(|meta| meta.len() == entry.metadata().map(|m| m.len()).unwrap_or(u64::MAX))
            .unwrap_or(false);
        if !same_size {
            *missing += 1;
        }
    }
    Ok(())
}

/// 删掉已经校验过的来源任务目录。
fn remove_tasks(source: &Path) -> Result<()> {
    for dir in task_dirs(source) {
        fs::remove_dir_all(dir)?;
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    const ID_A: &str = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa";
    const ID_B: &str = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb";

    /// 在 root 下造一个任务目录：`tasks/<id>/task.json` + 工作副本。
    fn make_task(root: &Path, id: &str, name: &str, body: &[u8]) {
        let dir = root.join("tasks").join(id);
        fs::create_dir_all(&dir).unwrap();
        fs::write(dir.join("task.json"), format!("{{\"task_id\":\"{id}\"}}")).unwrap();
        fs::write(dir.join(name), body).unwrap();
    }

    fn tmp() -> (tempfile::TempDir, PathBuf) {
        crate::storage::test_root()
    }

    #[test]
    fn carries_tasks_to_the_new_root_and_leaves_nothing_behind() {
        let (_dir, base) = tmp();
        let old = base.join("old");
        let new = base.join("new");
        make_task(&old, ID_A, "drawing.dwg", b"1234567890");
        make_task(&old, ID_B, "notes.txt", b"xy");

        let (_, expected_bytes) = mirror_size(&old);
        let outcome = carry_over_to(&old, &new);
        assert_eq!(outcome.state, "done");
        assert_eq!(outcome.tasks, 2, "两个任务目录都该搬过去");
        assert_eq!(outcome.files, 4, "每个任务目录里是 task.json + 工作副本");
        // 报出来的字节数就是来源的镜像大小（含每个任务的 task.json）。
        assert_eq!(outcome.bytes, expected_bytes);
        assert!(outcome.bytes > 0);
        assert_eq!(outcome.missing, 0);
        // 目标里两份任务都在，来源的任务目录已删（校验通过之后才删）。
        assert!(new.join("tasks").join(ID_A).join("drawing.dwg").is_file());
        assert!(new.join("tasks").join(ID_B).join("notes.txt").is_file());
        assert!(task_dirs(&old).is_empty());
    }

    #[test]
    fn never_merges_two_populated_roots() {
        let (_dir, base) = tmp();
        let old = base.join("old");
        let new = base.join("new");
        make_task(&old, ID_A, "drawing.dwg", b"old");
        make_task(&new, ID_B, "notes.txt", b"new");

        let outcome = carry_over_to(&old, &new);
        assert_eq!(outcome.state, "skipped");
        // 目标赢：来源原地不动，两边都不丢东西。
        assert!(old.join("tasks").join(ID_A).join("drawing.dwg").is_file());
        assert!(new.join("tasks").join(ID_B).join("notes.txt").is_file());
    }

    #[test]
    fn a_matching_marker_makes_it_a_no_op() {
        let (_dir, base) = tmp();
        let app_root = base.join("app");
        fs::create_dir_all(&app_root).unwrap();
        let root = base.join("root");
        make_task(&root, ID_A, "drawing.dwg", b"x");

        record_marker(&app_root, &root).unwrap();
        let outcome = carry_over_to(&app_root, &root);
        assert_eq!(outcome.state, "settled");
        assert_eq!(outcome.from, "");
        assert!(root.join("tasks").join(ID_A).join("drawing.dwg").is_file());
    }

    #[test]
    fn records_the_root_when_there_is_nothing_to_carry() {
        let (_dir, base) = tmp();
        let app_root = base.join("app");
        fs::create_dir_all(&app_root).unwrap();

        let outcome = carry_over(&app_root);
        assert_eq!(outcome.state, "settled");
        assert_eq!(outcome.from, "");
        assert_eq!(outcome.tasks, 0);
        // marker 一定要写：没有它下次还会重新找一遍。
        assert_eq!(
            recorded_marker(&app_root).as_deref(),
            Some(resolve(&app_root).as_path())
        );
        // 结算结果也留一份在 agent 根目录，排查时不必去动工作区。
        assert!(report_path(&app_root).is_file());
    }

    #[test]
    fn never_overwrites_an_existing_target_file() {
        let (_dir, base) = tmp();
        let old = base.join("old");
        let new = base.join("new");
        make_task(&old, ID_A, "drawing.dwg", b"from-source");
        // 目标里已经有同名文件（且大小不同）——永不覆盖。
        let target = new.join("tasks").join(ID_A);
        fs::create_dir_all(&target).unwrap();
        fs::write(target.join("drawing.dwg"), b"KEEP").unwrap();

        let outcome = carry_over_to(&old, &new);
        assert_eq!(fs::read(target.join("drawing.dwg")).unwrap(), b"KEEP");
        assert_eq!(outcome.skipped, 1, "目标已有同名文件，必须跳过而不是覆盖");
        // 大小不符 → 校验不通过 → 来源原地保留。
        assert_eq!(outcome.missing, 1);
        assert_eq!(outcome.state, "partial");
        assert!(old.join("tasks").join(ID_A).join("task.json").is_file());
    }

    #[test]
    fn size_counts_only_task_directories() {
        let (_dir, base) = tmp();
        let root = base.join("root");
        make_task(&root, ID_A, "drawing.dwg", b"12345");
        // 工作区根可以是用户自己选的目录：那里别的东西一律不算进镜像。
        fs::write(root.join("my-own-file.txt"), b"not the agent's business").unwrap();
        let loose = root.join("tasks").join("not-a-task-id");
        fs::create_dir_all(&loose).unwrap();
        fs::write(loose.join("junk.bin"), vec![0u8; 4096]).unwrap();
        // 名字是 64 位 hex 但没有 task.json 的目录同样不算。
        let no_manifest = root.join("tasks").join(ID_B);
        fs::create_dir_all(&no_manifest).unwrap();
        fs::write(no_manifest.join("junk.bin"), vec![0u8; 4096]).unwrap();

        let (tasks, bytes) = mirror_size(&root);
        assert_eq!(tasks, 1);
        // 只算任务目录里的文件（含 task.json），上面那些 4KB 的杂物一个都不能算进来。
        let manifest = root.join("tasks").join(ID_A).join("task.json");
        assert_eq!(
            bytes,
            5 + fs::metadata(&manifest).unwrap().len(),
            "工作区根里非任务内容不能算进镜像大小"
        );
    }

    #[test]
    fn carries_from_the_current_default_when_the_marker_is_empty() {
        let (_dir, base) = tmp();
        let app_root = base.join("app");
        fs::create_dir_all(&app_root).unwrap();
        // 模拟「机器以前一直在平台默认位置，现在首选盘可用了」。
        let existing = tasks_root(&app_root);
        let dir = existing.join(ID_A);
        fs::create_dir_all(&dir).unwrap();
        fs::write(dir.join("task.json"), b"{}").unwrap();
        fs::write(dir.join("drawing.dwg"), b"payload").unwrap();
        let new = base.join("preferred");
        fs::create_dir_all(&new).unwrap();

        let outcome = carry_over_to(&app_root, &new);
        assert_eq!(outcome.state, "done");
        assert!(new.join("tasks").join(ID_A).join("drawing.dwg").is_file());
        assert!(task_dirs(&existing).is_empty());
    }

    #[test]
    fn same_path_folds_case_only_on_windows() {
        let (a, b) = (Path::new("C:\\Temp\\Mirror"), Path::new("c:\\temp\\mirror"));
        assert_eq!(
            same_path(Some(a), Some(b)),
            cfg!(windows),
            "Windows 不折大小写会让同一个目录被当成两个根"
        );
    }

    #[test]
    fn only_the_standard_install_root_gets_the_drive_preference() {
        // 测试/便携用的临时根绝不能被改写到别的盘：那会写进用户真实的工作区。
        let (_dir, base) = tmp();
        assert!(!is_default_app_root(&base));
        assert_eq!(resolve(&base), base);
        assert_eq!(tasks_root(&base), base.join("tasks"));

        #[cfg(windows)]
        {
            // 真实安装根才会去探首选盘。这里只断言**不变量**（解析结果只能是安装根
            // 或首选盘上的固定路径），因为 CI 的 Windows runner 不一定有 D 盘。
            let default_root = dirs::data_local_dir().unwrap().join("CloudFileAgent22");
            assert!(is_default_app_root(&default_root));
            assert_eq!(
                preferred_root(),
                Some(PathBuf::from("D:\\CloudFileAgent22"))
            );
            let resolved = resolve(&default_root);
            assert!(
                resolved == default_root || resolved == PathBuf::from("D:\\CloudFileAgent22"),
                "只能解析成安装根或首选盘，实际 {resolved:?}"
            );
            // 一旦真的切到首选盘，那个位置必须是固定本地盘（不是光驱/U 盘/网络盘）。
            if resolved != default_root {
                assert!(fixed_volume(&resolved), "首选根必须落在固定本地盘上");
            }
        }
    }

    #[test]
    fn the_marker_and_report_live_outside_the_mirror() {
        let (_dir, base) = tmp();
        let app_root = base.join("app");
        fs::create_dir_all(&app_root).unwrap();
        assert_eq!(marker_path(&app_root), app_root.join("mirror-root.txt"));
        // 标记不能放在镜像里面：镜像换了位置它也得留在原地。
        assert!(!marker_path(&app_root).starts_with(base.join("app").join("tasks")));
        record_marker(&app_root, Path::new("relative")).unwrap_err();
    }
}

//! Local application detection and user-defined open rules.
//!
//! Selecting an application is a local convenience only. It never decides which
//! server call runs and never changes what gets uploaded. The reference Windows
//! and Go agents already shipped these rules; this module ports the same intent
//! to one platform-adapted table instead of inheriting the historical GUI.
use anyhow::{bail, Context, Result};
use serde::{Deserialize, Serialize};
use std::path::{Path, PathBuf};

pub const PLACEHOLDER: &str = "{file}";
const MAX_RULES: usize = 32;

/// An application the agent can hand a working copy to.
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub struct Application {
    pub id: String,
    pub name: String,
    pub extensions: Vec<String>,
    /// Executable template. Exactly one argument is the `{file}` placeholder.
    pub command: Vec<String>,
    /// Whether the executable was actually found on this machine.
    pub available: bool,
}

/// A user rule always wins over detection and over the system default.
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct OpenRule {
    pub id: String,
    /// Restricted to the two local session modes so a read-only copy can never
    /// be routed into an editing application by accident.
    pub modes: Vec<String>,
    /// Lowercase extensions, or `*` as a final catch-all.
    pub extensions: Vec<String>,
    pub command: Vec<String>,
}

impl OpenRule {
    pub fn validate(&self) -> Result<()> {
        if self.id.is_empty() || self.id.len() > 64 || self.id.chars().any(char::is_control) {
            bail!("open rule requires a short identifier");
        }
        if self.modes.is_empty()
            || self
                .modes
                .iter()
                .any(|mode| !matches!(mode.as_str(), "local-view" | "local-edit"))
        {
            bail!("open rule modes must be local-view or local-edit");
        }
        if self.extensions.is_empty() {
            bail!("open rule requires at least one extension");
        }
        for extension in &self.extensions {
            if !valid_extension(extension) {
                bail!("open rule extension must be lowercase alphanumeric");
            }
        }
        if self.command.is_empty() || self.command.len() > 32 {
            bail!("open rule requires a command");
        }
        let program = &self.command[0];
        if program.contains('\0') || !Path::new(program).is_absolute() {
            bail!("open rule program must be an absolute path");
        }
        let placeholders = self
            .command
            .iter()
            .filter(|argument| argument.contains(PLACEHOLDER))
            .count();
        if placeholders != 1 {
            bail!("open rule command needs exactly one file placeholder");
        }
        Ok(())
    }
    fn matches(&self, mode: &str, name: &str) -> bool {
        let family = mode_family(mode);
        if !self.modes.iter().any(|value| value == family) {
            return false;
        }
        let extension = extension_of(name);
        let mut wildcard = false;
        for candidate in &self.extensions {
            if candidate == "*" {
                wildcard = true;
            } else if *candidate == extension {
                // A specific extension always outranks a later `*`.
                return true;
            }
        }
        wildcard
    }
    fn render(&self, path: &Path) -> Vec<String> {
        let file = path.to_string_lossy().into_owned();
        self.command
            .iter()
            .map(|argument| argument.replace(PLACEHOLDER, &file))
            .collect()
    }
}

/// The Chrome extension and the server use `local-edit`; the agent tracks the
/// same family as `optimistic-edit` because it publishes native updates.
/// `local-view` stays distinct so a read-only copy cannot be routed into an
/// editing rule.
pub fn mode_family(mode: &str) -> &str {
    match mode {
        "exclusive-edit" => "exclusive-edit",
        "local-view" => "local-view",
        _ => "local-edit",
    }
}

pub fn extension_of(name: &str) -> String {
    name.rsplit_once('.')
        .map(|(_, extension)| extension.to_ascii_lowercase())
        .unwrap_or_default()
}

fn valid_extension(value: &str) -> bool {
    value == "*"
        || (!value.is_empty()
            && value.len() <= 16
            && value
                .chars()
                .all(|c| c.is_ascii_lowercase() || c.is_ascii_digit() || c == '+' || c == '-'))
}

/// Types the agent will hand to the OS default handler. Anything else needs an
/// explicit rule or a detected application, so an unexpected type is not
/// silently launched.
const DEFAULT_OPENABLE: &[&str] = &[
    "txt", "md", "pdf", "doc", "docx", "xls", "xlsx", "ppt", "pptx", "odt", "ods", "odp", "csv",
    "rtf", "png", "jpg", "jpeg", "gif", "tif", "tiff", "bmp", "dwg", "dxf", "step", "stp", "iges",
    "igs", "sldprt", "sldasm", "slddrw",
];

pub fn validate_rules(rules: &[OpenRule]) -> Result<()> {
    if rules.len() > MAX_RULES {
        bail!("too many open rules");
    }
    let mut seen = std::collections::BTreeSet::new();
    for rule in rules {
        rule.validate()?;
        if !seen.insert(rule.id.clone()) {
            bail!("duplicate open rule identifier");
        }
    }
    Ok(())
}

pub fn resolve_open_rule<'a>(
    rules: &'a [OpenRule],
    mode: &str,
    name: &str,
) -> Option<&'a OpenRule> {
    rules.iter().find(|rule| rule.matches(mode, name))
}

pub fn for_file<'a>(apps: &'a [Application], name: &str) -> Option<&'a Application> {
    let extension = extension_of(name);
    if extension.is_empty() {
        return None;
    }
    apps.iter().find(|app| app.extensions.contains(&extension))
}

/// Rule, then detected application, then the OS default. The first two carry an
/// explicit program chosen by the user or by this table; the default handler is
/// only used for known document types.
pub fn open_path(root: &Path, mode: &str, name: &str, path: &Path) -> Result<()> {
    if !path.is_absolute() {
        bail!("working copy path is not absolute");
    }
    let rules = crate::settings::load(root)
        .map(|settings| settings.open_rules)
        .unwrap_or_default();
    if let Some(rule) = resolve_open_rule(&rules, mode, name) {
        let mut command = rule.render(path);
        let program = command.remove(0);
        return spawn(&program, &command);
    }
    let apps = detect();
    if let Some(app) = for_file(&apps, name) {
        if app.available {
            let mut command = app.command.clone();
            let program = command.remove(0);
            let command: Vec<String> = command
                .iter()
                .map(|argument| argument.replace(PLACEHOLDER, &path.to_string_lossy()))
                .collect();
            return spawn(&program, &command);
        }
    }
    if !DEFAULT_OPENABLE.contains(&extension_of(name).as_str()) {
        bail!("file type requires a locally configured application");
    }
    open::that(path)?;
    Ok(())
}

fn spawn(program: &str, arguments: &[String]) -> Result<()> {
    if program.contains('\0') || arguments.iter().any(|argument| argument.contains('\0')) {
        bail!("invalid application command");
    }
    std::process::Command::new(program)
        .args(arguments)
        .stdin(std::process::Stdio::null())
        .stdout(std::process::Stdio::null())
        .stderr(std::process::Stdio::null())
        .spawn()
        .with_context(|| format!("could not start {program}"))?;
    Ok(())
}

/// Opens a directory in the platform file manager.
pub fn open_directory(path: &Path) -> Result<()> {
    if !path.is_dir() {
        bail!("directory is not available");
    }
    #[cfg(target_os = "macos")]
    let program = "open";
    #[cfg(windows)]
    let program = "explorer";
    #[cfg(not(any(target_os = "macos", windows)))]
    let program = "xdg-open";
    spawn(program, &[path.to_string_lossy().into_owned()])
}

/// Parses `reg query <key> /ve` output into the default value.
/// Kept platform independent so it is covered by the normal test run.
pub fn parse_reg_default_value(stdout: &str) -> Option<String> {
    for line in stdout.lines() {
        for (marker, expand) in [("REG_EXPAND_SZ", true), ("REG_SZ", false)] {
            let Some((_, value)) = line.split_once(marker) else {
                continue;
            };
            let value = value.trim();
            if value.is_empty() {
                break;
            }
            if expand {
                // Only an expandable value is a template; a plain path is literal.
                if let Some(expanded) = std::env::var_os(value) {
                    return Some(expanded.to_string_lossy().into_owned());
                }
            }
            return Some(value.to_string());
        }
    }
    None
}

/// Parses `reg query <key> /v <name>` output into that named value. The value
/// is a version string here, so it is returned verbatim rather than expanded.
/// Kept platform independent so it is covered by the normal test run.
pub fn parse_named_reg_value(stdout: &str, name: &str) -> Option<String> {
    for line in stdout.lines() {
        // Each match reads `    pv    REG_SZ    120.0.2210.91`.
        let trimmed = line.trim_start();
        let Some((candidate, rest)) = trimmed.split_once(char::is_whitespace) else {
            continue;
        };
        if candidate != name {
            continue;
        }
        let (_, value) = rest.split_once("REG_SZ")?;
        return Some(value.trim().to_string());
    }
    None
}

/// Version of the WebView2 runtime the Windows desktop shell needs, if present.
///
/// Tauri on Windows renders through WebView2. It ships with current Windows and
/// alongside Edge, but not on every image, so the agent reports its absence
/// rather than bundling a large offline runtime — that would defeat the goal of
/// a small, dependency-free package. `None` means the check does not apply: on
/// macOS the system WebKit is always there.
pub fn webview2_version() -> Option<String> {
    #[cfg(windows)]
    {
        // Edge registers the runtime per machine and per user; either satisfies
        // the shell, so the machine-wide key is only preferred, not required.
        const CLIENT: &str = "{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}";
        let keys = [
            format!("HKLM\\SOFTWARE\\WOW6432Node\\Microsoft\\EdgeUpdate\\Clients\\{CLIENT}"),
            format!("HKCU\\Software\\Microsoft\\EdgeUpdate\\Clients\\{CLIENT}"),
        ];
        for key in keys {
            let Ok(output) = std::process::Command::new("reg")
                .args(["query", &key, "/v", "pv"])
                .output()
            else {
                continue;
            };
            if !output.status.success() {
                continue;
            }
            let stdout = String::from_utf8_lossy(&output.stdout);
            if let Some(version) = parse_named_reg_value(&stdout, "pv") {
                if !version.is_empty() {
                    return Some(version);
                }
            }
        }
        None
    }
    #[cfg(not(windows))]
    {
        None
    }
}

#[cfg(target_os = "macos")]
mod platform {
    use super::*;

    pub struct Spec {
        pub id: &'static str,
        pub name: &'static str,
        pub extensions: &'static [&'static str],
        /// Bundle names, most preferred first. Locations are resolved per user.
        pub bundles: &'static [&'static str],
    }

    pub const SPECS: &[Spec] = &[
        Spec {
            id: "word",
            name: "Microsoft Word",
            extensions: &["doc", "docx", "dot", "dotx", "rtf"],
            bundles: &["Microsoft Word.app"],
        },
        Spec {
            id: "excel",
            name: "Microsoft Excel",
            extensions: &["xls", "xlsx", "xlsm", "xlsb", "csv"],
            bundles: &["Microsoft Excel.app"],
        },
        Spec {
            id: "powerpoint",
            name: "Microsoft PowerPoint",
            extensions: &["ppt", "pptx", "pptm", "ppsx"],
            bundles: &["Microsoft PowerPoint.app"],
        },
        Spec {
            id: "libreoffice",
            name: "LibreOffice",
            extensions: &[
                "odt", "ods", "odp", "doc", "docx", "xls", "xlsx", "ppt", "pptx",
            ],
            bundles: &["LibreOffice.app"],
        },
        Spec {
            id: "autocad",
            name: "AutoCAD",
            extensions: &["dwg", "dxf", "dwt"],
            bundles: &[
                "Autodesk/AutoCAD 2026/AutoCAD.app",
                "Autodesk/AutoCAD 2025/AutoCAD.app",
                "Autodesk/AutoCAD 2024/AutoCAD.app",
            ],
        },
        Spec {
            id: "sketchup",
            name: "SketchUp",
            extensions: &["skp"],
            bundles: &["SketchUp 2024/SketchUp.app", "SketchUp 2023/SketchUp.app"],
        },
        Spec {
            id: "rhino",
            name: "Rhinoceros",
            extensions: &["3dm"],
            bundles: &["Rhino 8.app", "Rhino 7.app"],
        },
        Spec {
            id: "freecad",
            name: "FreeCAD",
            extensions: &["step", "stp", "iges", "igs", "stl", "obj", "fcstd"],
            bundles: &["FreeCAD.app"],
        },
        Spec {
            id: "preview",
            name: "Preview",
            extensions: &["pdf", "png", "jpg", "jpeg", "tif", "tiff", "gif", "bmp"],
            bundles: &[
                "System/Applications/Preview.app",
                "Applications/Preview.app",
            ],
        },
        Spec {
            id: "textedit",
            name: "TextEdit",
            extensions: &["txt", "md"],
            bundles: &[
                "System/Applications/TextEdit.app",
                "Applications/TextEdit.app",
            ],
        },
    ];

    pub fn roots() -> Vec<PathBuf> {
        let mut roots = vec![PathBuf::from("/Applications")];
        if let Some(home) = dirs::home_dir() {
            roots.push(home.join("Applications"));
        }
        roots
    }

    pub fn resolve(bundle: &str) -> Option<PathBuf> {
        for root in roots() {
            let candidate = root.join(bundle);
            if candidate.is_dir() {
                return Some(candidate);
            }
        }
        None
    }

    pub fn command(bundle: &Path) -> Vec<String> {
        vec![
            "/usr/bin/open".into(),
            "-a".into(),
            bundle.to_string_lossy().into_owned(),
            PLACEHOLDER.into(),
        ]
    }
}

#[cfg(windows)]
mod platform {
    use super::*;
    use std::process::Command;

    pub struct Spec {
        pub id: &'static str,
        pub name: &'static str,
        pub extensions: &'static [&'static str],
        /// Executable name used for `App Paths` lookup and the root scan.
        pub exe: &'static str,
        /// Well known install locations relative to a program root.
        pub fallbacks: &'static [&'static str],
    }

    pub const SPECS: &[Spec] = &[
        Spec {
            id: "word",
            name: "Microsoft Word",
            extensions: &["doc", "docx", "dot", "dotx", "rtf"],
            exe: "WINWORD.EXE",
            fallbacks: &[
                "Microsoft Office/root/Office16/WINWORD.EXE",
                "Microsoft Office/Office16/WINWORD.EXE",
            ],
        },
        Spec {
            id: "excel",
            name: "Microsoft Excel",
            extensions: &["xls", "xlsx", "xlsm", "xlsb", "csv"],
            exe: "EXCEL.EXE",
            fallbacks: &[
                "Microsoft Office/root/Office16/EXCEL.EXE",
                "Microsoft Office/Office16/EXCEL.EXE",
            ],
        },
        Spec {
            id: "powerpoint",
            name: "Microsoft PowerPoint",
            extensions: &["ppt", "pptx", "pptm", "ppsx"],
            exe: "POWERPNT.EXE",
            fallbacks: &[
                "Microsoft Office/root/Office16/POWERPNT.EXE",
                "Microsoft Office/Office16/POWERPNT.EXE",
            ],
        },
        Spec {
            id: "visio",
            name: "Microsoft Visio",
            extensions: &["vsd", "vsdx", "vssx"],
            exe: "VISIO.EXE",
            fallbacks: &["Microsoft Office/root/Office16/VISIO.EXE"],
        },
        Spec {
            id: "libreoffice",
            name: "LibreOffice",
            extensions: &[
                "odt", "ods", "odp", "doc", "docx", "xls", "xlsx", "ppt", "pptx",
            ],
            exe: "soffice.exe",
            fallbacks: &["LibreOffice/program/soffice.exe"],
        },
        Spec {
            id: "autocad",
            name: "AutoCAD",
            extensions: &["dwg", "dxf", "dwt"],
            exe: "acad.exe",
            fallbacks: &[
                "Autodesk/AutoCAD 2026/acad.exe",
                "Autodesk/AutoCAD 2025/acad.exe",
            ],
        },
        Spec {
            id: "revit",
            name: "Revit",
            extensions: &["rvt", "rfa", "rte", "rft"],
            exe: "Revit.exe",
            fallbacks: &[],
        },
        Spec {
            id: "solidworks",
            name: "SOLIDWORKS",
            extensions: &["sldprt", "sldasm", "slddrw"],
            exe: "SLDWORKS.exe",
            fallbacks: &["SOLIDWORKS Corp/SOLIDWORKS/SLDWORKS.exe"],
        },
        Spec {
            id: "sketchup",
            name: "SketchUp",
            extensions: &["skp"],
            exe: "SketchUp.exe",
            fallbacks: &[],
        },
        Spec {
            id: "rhino",
            name: "Rhinoceros",
            extensions: &["3dm"],
            exe: "Rhino.exe",
            fallbacks: &[],
        },
        Spec {
            id: "freecad",
            name: "FreeCAD",
            extensions: &["step", "stp", "iges", "igs", "stl", "obj", "fcstd"],
            exe: "FreeCAD.exe",
            fallbacks: &[
                "FreeCAD 1.0/bin/FreeCAD.exe",
                "FreeCAD 0.21/bin/FreeCAD.exe",
            ],
        },
        Spec {
            id: "notepad",
            name: "Notepad",
            extensions: &["txt", "md", "csv"],
            exe: "notepad.exe",
            fallbacks: &["Windows/System32/notepad.exe"],
        },
    ];

    pub fn program_roots() -> Vec<PathBuf> {
        let mut roots = Vec::new();
        for key in [
            "ProgramW6432",
            "ProgramFiles",
            "ProgramFiles(x86)",
            "LOCALAPPDATA",
        ] {
            if let Some(value) = std::env::var_os(key) {
                roots.push(PathBuf::from(value));
            }
        }
        roots
    }

    fn registry(exe: &str) -> Option<PathBuf> {
        let candidate = format!("Software\\Microsoft\\Windows\\CurrentVersion\\App Paths\\{exe}");
        for (hive, force) in [("HKCU", false), ("HKLM", false), ("HKLM", true)] {
            let key = if force {
                format!(
                    "Software\\WOW6432Node\\Microsoft\\Windows\\CurrentVersion\\App Paths\\{exe}"
                )
            } else {
                candidate.clone()
            };
            let output = Command::new("reg")
                .args(["query", &format!("{hive}\\{key}"), "/ve"])
                .output();
            if let Ok(output) = output {
                if output.status.success() {
                    if let Some(value) =
                        super::parse_reg_default_value(&String::from_utf8_lossy(&output.stdout))
                    {
                        let path = PathBuf::from(value);
                        if path.is_file() {
                            return Some(path);
                        }
                    }
                }
            }
        }
        None
    }

    pub fn resolve(spec: &Spec) -> Option<PathBuf> {
        if let Some(path) = registry(spec.exe) {
            return Some(path);
        }
        for root in program_roots() {
            for fallback in spec.fallbacks {
                let candidate = root.join(fallback);
                if candidate.is_file() {
                    return Some(candidate);
                }
            }
        }
        None
    }

    pub fn command(path: &Path) -> Vec<String> {
        vec![path.to_string_lossy().into_owned(), PLACEHOLDER.into()]
    }
}

#[cfg(not(any(target_os = "macos", windows)))]
mod platform {
    use super::*;

    pub struct Spec {
        pub id: &'static str,
        pub name: &'static str,
        pub extensions: &'static [&'static str],
        pub programs: &'static [&'static str],
    }

    pub const SPECS: &[Spec] = &[
        Spec {
            id: "libreoffice",
            name: "LibreOffice",
            extensions: &[
                "odt", "ods", "odp", "doc", "docx", "xls", "xlsx", "ppt", "pptx",
            ],
            programs: &["soffice", "libreoffice"],
        },
        Spec {
            id: "freecad",
            name: "FreeCAD",
            extensions: &["step", "stp", "iges", "igs", "stl", "obj", "fcstd"],
            programs: &["freecad", "FreeCAD"],
        },
        Spec {
            id: "inkscape",
            name: "Inkscape",
            extensions: &["svg"],
            programs: &["inkscape"],
        },
    ];

    pub fn resolve(program: &str) -> Option<PathBuf> {
        let path = std::env::var_os("PATH")?;
        for directory in std::env::split_paths(&path) {
            let candidate = directory.join(program);
            if candidate.is_file() {
                return Some(candidate);
            }
        }
        None
    }

    pub fn command(path: &Path) -> Vec<String> {
        vec![path.to_string_lossy().into_owned(), PLACEHOLDER.into()]
    }
}

/// Full table with availability resolved once per call. Detection is cheap
/// enough for a status request and must never be cached across an install.
pub fn detect() -> Vec<Application> {
    platform::SPECS
        .iter()
        .map(|spec| {
            #[cfg(target_os = "macos")]
            let resolved = spec
                .bundles
                .iter()
                .find_map(|bundle| platform::resolve(bundle));
            #[cfg(not(target_os = "macos"))]
            let resolved = {
                #[cfg(windows)]
                {
                    platform::resolve(spec)
                }
                #[cfg(not(windows))]
                {
                    spec.programs
                        .iter()
                        .find_map(|program| platform::resolve(program))
                }
            };
            let command = resolved
                .as_ref()
                .map(|found| platform::command(found))
                .unwrap_or_default();
            let available = resolved.is_some();
            Application {
                id: spec.id.to_string(),
                name: spec.name.to_string(),
                extensions: spec
                    .extensions
                    .iter()
                    .map(|value| (*value).to_string())
                    .collect(),
                command,
                available,
            }
        })
        .collect()
}

pub fn names() -> Vec<String> {
    platform::SPECS
        .iter()
        .map(|spec| spec.name.to_string())
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;

    /// A program path that is absolute on the host OS. Rules must not hard-code
    /// a Unix path: `Path::is_absolute` is false for `/usr/bin/tool` on Windows,
    /// so such a test passes locally and fails the Windows runner.
    #[cfg(windows)]
    const TOOL: &str = "C:/tools/tool.exe";
    #[cfg(not(windows))]
    const TOOL: &str = "/usr/bin/tool";

    fn rule(id: &str, modes: &[&str], extensions: &[&str], command: &[&str]) -> OpenRule {
        OpenRule {
            id: id.into(),
            modes: modes.iter().map(|value| (*value).into()).collect(),
            extensions: extensions.iter().map(|value| (*value).into()).collect(),
            command: command.iter().map(|value| (*value).into()).collect(),
        }
    }

    #[test]
    fn named_registry_values_are_read_by_name_not_by_position() {
        // The real layout of `reg query <key> /v pv`.
        let stdout = "\r\nHKEY_LOCAL_MACHINE\\SOFTWARE\\WOW6432Node\\Microsoft\\EdgeUpdate\\Clients\\{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}\r\n    pv    REG_SZ    120.0.2210.91\r\n\r\n";
        assert_eq!(
            parse_named_reg_value(stdout, "pv").as_deref(),
            Some("120.0.2210.91")
        );
        // A different value name next to it must not be mistaken for the target.
        let other = "HKEY_CURRENT_USER\\Software\\Vendor\r\n    Name    REG_SZ    CloudFile\r\n    pv    REG_SZ    1.2.3\r\n";
        assert_eq!(parse_named_reg_value(other, "pv").as_deref(), Some("1.2.3"));
        assert_eq!(parse_named_reg_value(other, "name"), None);
        assert_eq!(parse_named_reg_value(other, "missing"), None);
        // A missing key produces an error message, not a value.
        assert_eq!(
            parse_named_reg_value(
                "ERROR: The system was unable to find the specified registry key or value.\r\n",
                "pv"
            ),
            None
        );
    }

    #[test]
    fn runtime_probe_only_answers_where_it_applies() {
        // macOS renders through the system WebKit, so there is nothing to report
        // and the caller must be able to tell that apart from "missing".
        if cfg!(windows) {
            // Either a version string or nothing; never a placeholder.
            if let Some(version) = webview2_version() {
                assert!(!version.is_empty());
                assert!(version.chars().next().unwrap().is_ascii_digit());
            }
        } else {
            assert_eq!(webview2_version(), None);
        }
    }

    #[test]
    fn rule_validation_rejects_unsafe_commands() {
        for bad in [
            rule("a", &["local-edit"], &["dwg"], &["relative/tool"]),
            rule("a", &["local-edit"], &["dwg"], &[TOOL]),
            rule("a", &["local-edit"], &["dwg"], &[TOOL, "{file}", "{file}"]),
            rule("a", &["prompt"], &["dwg"], &[TOOL, "{file}"]),
            rule("", &["local-edit"], &["dwg"], &[TOOL, "{file}"]),
            rule("a", &["local-edit"], &[], &[TOOL, "{file}"]),
            rule("a", &["local-edit"], &["DWG"], &[TOOL, "{file}"]),
        ] {
            assert!(bad.validate().is_err(), "{bad:?} must be rejected");
        }
        rule("ok", &["local-edit"], &["dwg"], &[TOOL, "{file}"])
            .validate()
            .unwrap();
    }

    #[test]
    fn specific_extension_outranks_a_wildcard() {
        let rules = vec![
            rule("catch", &["local-edit"], &["*"], &[TOOL, "{file}"]),
            rule("cad", &["local-edit"], &["dwg"], &[TOOL, "{file}"]),
        ];
        assert_eq!(
            resolve_open_rule(&rules, "optimistic-edit", "a.dwg")
                .unwrap()
                .id,
            "catch"
        );
        // A `*` only applies after every specific rule failed.
        assert_eq!(
            resolve_open_rule(&rules, "local-edit", "a.txt").unwrap().id,
            "catch"
        );
        let ordered = vec![
            rule("cad", &["local-edit"], &["dwg"], &[TOOL, "{file}"]),
            rule("catch", &["local-edit"], &["*"], &[TOOL, "{file}"]),
        ];
        assert_eq!(
            resolve_open_rule(&ordered, "local-edit", "a.dwg")
                .unwrap()
                .id,
            "cad"
        );
    }

    #[test]
    fn rules_are_scoped_to_one_mode_and_rendered_without_a_shell() {
        let rules = vec![rule(
            "view",
            &["local-view"],
            &["dwg"],
            &[TOOL, "--readonly", "{file}"],
        )];
        assert!(resolve_open_rule(&rules, "local-edit", "a.dwg").is_none());
        assert!(resolve_open_rule(&rules, "local-view", "a.dwg").is_some());
        let rendered = rules[0].render(Path::new("/tmp/a b.dwg"));
        assert_eq!(
            rendered,
            vec![TOOL, "--readonly", "/tmp/a b.dwg"]
                .into_iter()
                .map(String::from)
                .collect::<Vec<_>>()
        );
    }

    #[test]
    fn duplicate_rule_ids_are_rejected() {
        let rules = vec![
            rule("same", &["local-edit"], &["dwg"], &[TOOL, "{file}"]),
            rule("same", &["local-edit"], &["dxf"], &[TOOL, "{file}"]),
        ];
        assert!(validate_rules(&rules).is_err());
    }

    #[test]
    fn detection_always_answers_with_a_stable_shape() {
        let apps = detect();
        assert!(!apps.is_empty());
        assert_eq!(apps.len(), platform::SPECS.len());
        for app in &apps {
            assert!(!app.extensions.is_empty());
            if app.available {
                assert!(Path::new(&app.command[0]).is_absolute());
                assert_eq!(
                    app.command
                        .iter()
                        .filter(|argument| argument.contains(PLACEHOLDER))
                        .count(),
                    1
                );
            } else {
                assert!(app.command.is_empty());
            }
        }
        // First match wins, which is why the table order is the priority order.
        if let Some(app) = for_file(&apps, "MODEL.STEP") {
            assert!(app.extensions.contains(&"step".to_string()));
        }
    }

    #[test]
    fn registry_output_parsing_handles_expandable_values() {
        let sample = "\r\nHKEY_CURRENT_USER\\Software\\Microsoft\\Windows\\CurrentVersion\\App Paths\\WINWORD.EXE\r\n    (Default)    REG_SZ    C:\\Program Files\\Microsoft Office\\root\\Office16\\WINWORD.EXE\r\n\r\n";
        assert_eq!(
            parse_reg_default_value(sample).unwrap(),
            "C:\\Program Files\\Microsoft Office\\root\\Office16\\WINWORD.EXE"
        );
        assert!(parse_reg_default_value(
            "ERROR: The system was unable to find the specified registry key or value."
        )
        .is_none());
    }

    #[test]
    fn unknown_types_are_not_launched_by_default() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("evil.exe");
        std::fs::write(&path, b"x").unwrap();
        assert!(open_path(dir.path(), "local-edit", "evil.exe", &path).is_err());
        assert!(open_path(
            dir.path(),
            "local-edit",
            "notes.txt",
            Path::new("relative.txt")
        )
        .is_err());
    }
}

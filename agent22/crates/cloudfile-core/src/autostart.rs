//! Optional login item for the resident desktop process.
//!
//! Starting at login must never be required for correctness: the agent works
//! when launched by hand, and a refused or impossible login item is reported
//! rather than silently ignored. macOS uses a per-user LaunchAgent so nothing
//! needs administrator rights; Windows uses the current-user `Run` key, the
//! same mechanism the shipped preview used.
use crate::settings;
use anyhow::{bail, Result};
use std::path::{Path, PathBuf};

#[cfg(target_os = "macos")]
use crate::storage;
// Both login-item back ends report a failed spawn through `context`, so the
// trait must be in scope on either platform, not only macOS.
#[cfg(any(target_os = "macos", windows))]
use anyhow::Context;

pub const MACOS_LABEL: &str = "com.cloudfile.agent22.autostart";
pub const WINDOWS_VALUE: &str = "CloudFileAgent22";

fn xml_escape(value: &str) -> String {
    value
        .replace('&', "&amp;")
        .replace('<', "&lt;")
        .replace('>', "&gt;")
        .replace('"', "&quot;")
        .replace('\'', "&apos;")
}

/// LaunchAgent property list. Kept hand-written so the agent needs no plist
/// library; the shape is fixed and covered by tests.
pub fn plist(label: &str, program: &Path, arguments: &[String]) -> Result<String> {
    if label.is_empty()
        || !label
            .chars()
            .all(|c| c.is_ascii_alphanumeric() || c == '.' || c == '-' || c == '_')
    {
        bail!("invalid login item label");
    }
    if !program.is_absolute() {
        bail!("login item program must be an absolute path");
    }
    let mut items = vec![program.to_string_lossy().into_owned()];
    items.extend(arguments.iter().cloned());
    let rendered: String = items
        .iter()
        .map(|item| format!("    <string>{}</string>\n", xml_escape(item)))
        .collect();
    Ok(format!(
        "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n\
         <!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n\
         <plist version=\"1.0\">\n\
         <dict>\n\
         \x20 <key>Label</key>\n\
         \x20 <string>{label}</string>\n\
         \x20 <key>ProgramArguments</key>\n\
         \x20 <array>\n{rendered}\x20 </array>\n\
         \x20 <key>RunAtLoad</key>\n\
         \x20 <true/>\n\
         \x20 <key>KeepAlive</key>\n\
         \x20 <dict>\n\
         \x20   <key>SuccessfulExit</key>\n\
         \x20   <false/>\n\
         \x20 </dict>\n\
         \x20 <key>LimitLoadToSessionType</key>\n\
         \x20 <string>Aqua</string>\n\
         </dict>\n\
         </plist>\n"
    ))
}

pub fn plist_path(home: &Path) -> PathBuf {
    home.join("Library/LaunchAgents")
        .join(format!("{MACOS_LABEL}.plist"))
}

/// `reg.exe` arguments for the current-user `Run` key. Pure for testing.
pub fn run_key_arguments(value: &str, program: Option<&Path>) -> Result<Vec<String>> {
    if value.is_empty()
        || value.len() > 64
        || !value
            .chars()
            .all(|c| c.is_ascii_alphanumeric() || c == '-' || c == '_')
    {
        bail!("invalid login item value name");
    }
    let key = "HKCU\\Software\\Microsoft\\Windows\\CurrentVersion\\Run".to_string();
    match program {
        Some(path) => {
            if !path.is_absolute() {
                bail!("login item program must be an absolute path");
            }
            Ok(vec![
                "add".into(),
                key,
                "/v".into(),
                value.into(),
                "/t".into(),
                "REG_SZ".into(),
                "/d".into(),
                path.to_string_lossy().into_owned(),
                "/f".into(),
            ])
        }
        None => Ok(vec![
            "delete".into(),
            key,
            "/v".into(),
            value.into(),
            "/f".into(),
        ]),
    }
}

#[cfg(target_os = "macos")]
fn launchctl(arguments: &[&str]) -> Result<()> {
    let output = std::process::Command::new("launchctl")
        .args(arguments)
        .output()
        .context("could not run launchctl")?;
    if !output.status.success() {
        bail!("launchctl refused the login item change");
    }
    Ok(())
}

/// Enables the login item. Records the intent in settings only after the OS
/// accepted it, so the UI never claims a state the system does not have.
pub fn install(root: &Path, program: &Path) -> Result<()> {
    if !program.is_absolute() {
        bail!("login item program must be an absolute path");
    }
    #[cfg(target_os = "macos")]
    {
        let home = dirs::home_dir().context("home directory unavailable")?;
        let path = plist_path(&home);
        storage::write_bytes(
            &path,
            plist(MACOS_LABEL, program, &["--autostart".to_string()])?.as_bytes(),
        )?;
        let domain = format!("gui/{}", uid()?);
        // Bootout first: a stale job would otherwise keep the old path alive.
        let _ = launchctl(&["bootout", &domain, &path.to_string_lossy()]);
        launchctl(&["bootstrap", &domain, &path.to_string_lossy()])?;
    }
    #[cfg(windows)]
    {
        let arguments = run_key_arguments(WINDOWS_VALUE, Some(program))?;
        let output = std::process::Command::new("reg")
            .args(&arguments)
            .output()
            .context("could not run reg.exe")?;
        if !output.status.success() {
            bail!("the login item was refused; start the agent manually");
        }
    }
    settings::update(root, |current| current.autostart = true)?;
    Ok(())
}

pub fn remove(root: &Path) -> Result<()> {
    #[cfg(target_os = "macos")]
    {
        let home = dirs::home_dir().context("home directory unavailable")?;
        let path = plist_path(&home);
        if path.exists() {
            let domain = format!("gui/{}", uid()?);
            let _ = launchctl(&["bootout", &domain, &path.to_string_lossy()]);
            std::fs::remove_file(&path)?;
        }
    }
    #[cfg(windows)]
    {
        let arguments = run_key_arguments(WINDOWS_VALUE, None)?;
        let _ = std::process::Command::new("reg").args(&arguments).output();
    }
    settings::update(root, |current| current.autostart = false)?;
    Ok(())
}

/// Reads the operating system, not the settings mirror: the settings file is
/// only ever a record of what the agent asked for.
pub fn enabled() -> bool {
    #[cfg(target_os = "macos")]
    {
        dirs::home_dir().is_some_and(|home| plist_path(&home).is_file())
    }
    #[cfg(windows)]
    {
        let key = "HKCU\\Software\\Microsoft\\Windows\\CurrentVersion\\Run";
        std::process::Command::new("reg")
            .args(["query", key, "/v", WINDOWS_VALUE])
            .output()
            .map(|output| output.status.success())
            .unwrap_or(false)
    }
    #[cfg(not(any(target_os = "macos", windows)))]
    {
        false
    }
}

#[cfg(target_os = "macos")]
fn uid() -> Result<u32> {
    let output = std::process::Command::new("id").arg("-u").output()?;
    let value = String::from_utf8_lossy(&output.stdout).trim().to_string();
    value.parse().context("could not determine the user id")
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Absolute paths for the host OS. `Path::is_absolute` is false for `/opt/x`
    /// on Windows, so a Unix literal passes on macOS and is refused by the
    /// login-item builders on the Windows runner.
    #[cfg(windows)]
    const PROGRAM: &str = "C:/Program Files/CloudFile Agent/cloudfile-desktop.exe";
    #[cfg(not(windows))]
    const PROGRAM: &str = "/opt/CloudFile Agent/bin/cloudfile-desktop";

    /// Same, for the escaping test, which needs an `&` inside the path.
    #[cfg(windows)]
    const AMPERSAND: &str = "C:/a&b/bin";
    #[cfg(not(windows))]
    const AMPERSAND: &str = "/opt/a&b/bin";

    /// Same, for the registry argument test, which needs an `.exe` suffix.
    #[cfg(windows)]
    const EXE: &str = "C:/CloudFileAgent22/cloudfile-desktop.exe";
    #[cfg(not(windows))]
    const EXE: &str = "/opt/CloudFileAgent22/cloudfile-desktop.exe";

    #[test]
    fn plist_is_a_well_formed_login_item() {
        let rendered = plist(
            MACOS_LABEL,
            Path::new(PROGRAM),
            &["--autostart".to_string()],
        )
        .unwrap();
        assert!(rendered.starts_with("<?xml version=\"1.0\""));
        assert!(rendered.contains("<string>com.cloudfile.agent22.autostart</string>"));
        assert!(rendered.contains(&format!("<string>{PROGRAM}</string>")));
        assert!(rendered.contains("<string>--autostart</string>"));
        assert!(rendered.contains("<key>RunAtLoad</key>"));
        assert!(rendered.contains("<key>SuccessfulExit</key>"));
        assert!(rendered.contains("<string>Aqua</string>"));
        assert!(rendered.contains("</plist>"));
        assert!(plist(DEV_LABEL_WITH_ESCAPE, Path::new("/a&b/c"), &[]).is_err());
        assert!(plist(MACOS_LABEL, Path::new("relative"), &[]).is_err());
    }

    const DEV_LABEL_WITH_ESCAPE: &str = "bad label";

    #[test]
    fn escaping_prevents_a_broken_property_list() {
        let plist = plist(MACOS_LABEL, Path::new(AMPERSAND), &["--x".into()]).unwrap();
        assert!(plist.contains(&AMPERSAND.replace('&', "&amp;")));
        assert!(!plist.contains(AMPERSAND));
    }

    #[test]
    fn login_item_path_is_per_user() {
        assert_eq!(
            plist_path(Path::new("/Users/user")),
            Path::new("/Users/user/Library/LaunchAgents/com.cloudfile.agent22.autostart.plist")
        );
    }

    #[test]
    fn run_key_arguments_are_exact_and_reversible() {
        let add = run_key_arguments(WINDOWS_VALUE, Some(Path::new(EXE))).unwrap();
        assert_eq!(add[0], "add");
        assert_eq!(
            add[1],
            "HKCU\\Software\\Microsoft\\Windows\\CurrentVersion\\Run"
        );
        assert_eq!(add[3], "CloudFileAgent22");
        assert_eq!(add[7], EXE);
        assert_eq!(add.len(), 9);
        let remove = run_key_arguments(WINDOWS_VALUE, None).unwrap();
        assert_eq!(
            remove,
            vec![
                "delete",
                "HKCU\\Software\\Microsoft\\Windows\\CurrentVersion\\Run",
                "/v",
                "CloudFileAgent22",
                "/f"
            ]
            .into_iter()
            .map(String::from)
            .collect::<Vec<_>>()
        );
        assert!(run_key_arguments("bad name", None).is_err());
        assert!(run_key_arguments(WINDOWS_VALUE, Some(Path::new("relative.exe"))).is_err());
    }
}

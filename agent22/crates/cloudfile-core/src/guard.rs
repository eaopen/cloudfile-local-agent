//! Exit risk assessment for the resident desktop process.
//!
//! The desktop process is the only holder of Checkout leases and the only
//! writer of publication intents. Terminating it silently stops background
//! renewal and can leave a user with an unconfirmed upload they do not know
//! about. This module answers one question for the tray menu: may the process
//! stop without a warning?
//!
//! Guarantee: an unreadable task or journal is never reported as "safe". Any
//! failure raises `uncertain`, so a broken store can only make the warning more
//! likely, never less.
use crate::{exclusive, journal, session};
use serde::Serialize;
use std::fs;
use std::path::Path;

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize)]
#[serde(rename_all = "kebab-case")]
pub enum Reason {
    /// The task or journal could not be read; state is unknown.
    UnreadableState,
    /// A valid exclusive Checkout is still held and being renewed.
    ActiveCheckout,
    /// A publication intent exists without a matching receipt.
    PendingIntent,
    /// The Checkout handoff was never acknowledged; the server still holds its
    /// ciphertext and the local session is not usable yet.
    UnacknowledgedLease,
}

impl Reason {
    /// Highest first: the warning headline should name the sharpest reason.
    /// A held Checkout being renewed is the expected steady state, so it ranks
    /// below anything that needs the user to act.
    fn rank(self) -> u8 {
        match self {
            Reason::UnreadableState => 3,
            Reason::PendingIntent => 2,
            Reason::UnacknowledgedLease => 1,
            Reason::ActiveCheckout => 0,
        }
    }
}

#[derive(Clone, Debug, Serialize)]
pub struct Risk {
    pub task_id: String,
    pub file_name: String,
    pub mode: String,
    pub reasons: Vec<Reason>,
}

impl Risk {
    pub fn headline(&self) -> Reason {
        // The list is built already ordered, so the first entry is the sharpest.
        self.reasons
            .first()
            .copied()
            .unwrap_or(Reason::UnreadableState)
    }
}

#[derive(Clone, Debug, Serialize)]
pub struct Assessment {
    /// True only when nothing at all is at risk. Callers must not widen this.
    pub safe_to_exit: bool,
    pub active_checkouts: usize,
    pub uncertain: usize,
    pub risks: Vec<Risk>,
}

impl Assessment {
    fn empty() -> Self {
        Assessment {
            safe_to_exit: true,
            active_checkouts: 0,
            uncertain: 0,
            risks: Vec::new(),
        }
    }
    fn finish(mut self) -> Self {
        self.safe_to_exit = self.risks.is_empty();
        for risk in &mut self.risks {
            risk.reasons
                .sort_by_key(|reason| std::cmp::Reverse(reason.rank()));
        }
        self.risks.sort_by(|a, b| a.file_name.cmp(&b.file_name));
        self
    }
}

/// Re-reads the durable store from disk. Never trusts a cached UI state: the
/// caller may have changed the journal since the last refresh.
pub fn assess(root: &Path) -> Assessment {
    let mut assessment = Assessment::empty();
    let dir = crate::workspace::tasks_root(root);
    let entries = match fs::read_dir(&dir) {
        Ok(entries) => entries,
        // No task directory yet is the only benign miss.
        Err(_) => return assessment.finish(),
    };
    for entry in entries.flatten() {
        if !entry.file_type().is_ok_and(|kind| kind.is_dir()) {
            continue;
        }
        let id = entry.file_name().to_string_lossy().into_owned();
        if !session::task_id(&id) || !entry.path().join("task.json").exists() {
            continue;
        }
        let (file_name, mode, mut reasons) = match session::load(root, &id) {
            Ok(task) => (task.file_name, task.mode, Vec::new()),
            Err(_) => (String::new(), String::new(), vec![Reason::UnreadableState]),
        };
        match journal::snapshot(root, &id) {
            Ok(state) => {
                if state.pending.is_some() {
                    reasons.push(Reason::PendingIntent);
                }
                if state.closed {
                    // A confirmed checkin released the Checkout; nothing is held.
                    reasons.retain(|reason| *reason != Reason::ActiveCheckout);
                    reasons.retain(|reason| *reason != Reason::UnacknowledgedLease);
                } else if let Some(lease) = &state.lease {
                    if exclusive::valid(lease) {
                        reasons.push(Reason::ActiveCheckout);
                        if !state.lease_ack {
                            reasons.push(Reason::UnacknowledgedLease);
                        }
                    } else {
                        // A malformed lease still occupies a guarded Checkout slot
                        // locally; treat it as unknown rather than as absent.
                        reasons.push(Reason::UnreadableState);
                    }
                }
            }
            Err(_) => reasons.push(Reason::UnreadableState),
        }
        if reasons.is_empty() {
            continue;
        }
        if reasons.contains(&Reason::ActiveCheckout) {
            assessment.active_checkouts += 1;
        }
        if reasons.iter().any(|reason| {
            matches!(
                reason,
                Reason::UnreadableState | Reason::UnacknowledgedLease | Reason::PendingIntent
            )
        }) {
            assessment.uncertain += 1;
        }
        assessment.risks.push(Risk {
            task_id: id,
            file_name,
            mode,
            reasons,
        });
    }
    assessment.finish()
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{journal::Action, storage};

    fn task(root: &Path, id: &str, mode: &str, name: &str) -> std::path::PathBuf {
        let dir = session::task_dir(root, id).unwrap();
        storage::private_dir(&dir).unwrap();
        std::fs::write(dir.join(name), b"original").unwrap();
        storage::write(
            &dir.join("task.json"),
            &session::Task {
                task_id: id.to_string(),
                device_id: Some("d".repeat(64)),
                file_name: name.to_string(),
                mode: mode.to_string(),
                server: "https://example.com".into(),
                repo_id: "b".repeat(36),
                remote_path: format!("/{name}"),
                session_id: "c".repeat(36),
                baseline_file_id: "e".repeat(40),
                baseline_digest: String::new(),
                size: 8,
            },
        )
        .unwrap();
        dir
    }

    fn lease() -> exclusive::Lease {
        exclusive::Lease {
            guard_id: "11111111-1111-4111-8111-111111111111".into(),
            generation: "1".into(),
            credential_epoch: "2".into(),
            base_file_id: "e".repeat(40),
            token: "f".repeat(64),
        }
    }

    #[test]
    fn empty_store_is_safe_to_exit() {
        let dir = tempfile::tempdir().unwrap();
        let assessment = assess(dir.path());
        assert!(assessment.safe_to_exit);
        assert_eq!((assessment.active_checkouts, assessment.uncertain), (0, 0));
    }

    #[test]
    fn pending_intent_blocks_a_quiet_exit() {
        let (_dir, base) = crate::storage::test_root();
        let root = base.as_path();
        let id = "a".repeat(64);
        task(root, &id, "optimistic-edit", "a.txt");
        journal::capture(root, &id, Action::OptimisticUpdate).unwrap();
        let assessment = assess(root);
        assert!(!assessment.safe_to_exit);
        assert_eq!(assessment.risks.len(), 1);
        assert_eq!(assessment.risks[0].headline(), Reason::PendingIntent);
        assert_eq!(assessment.uncertain, 1);
    }

    #[test]
    fn active_checkout_is_reported_and_unacknowledged_lease_escalates() {
        let (_dir, base) = crate::storage::test_root();
        let root = base.as_path();
        let id = "a".repeat(64);
        task(root, &id, "exclusive-edit", "a.txt");
        journal::set_lease(root, &id, &lease()).unwrap();
        let assessment = assess(root);
        assert_eq!(assessment.active_checkouts, 1);
        assert_eq!(assessment.risks[0].headline(), Reason::UnacknowledgedLease);
        journal::acknowledge_lease(root, &id).unwrap();
        let assessment = assess(root);
        assert_eq!(assessment.risks[0].headline(), Reason::ActiveCheckout);
        assert!(!assessment.safe_to_exit);
        // A known, held Checkout is not uncertainty; it is simply still open.
        assert_eq!((assessment.active_checkouts, assessment.uncertain), (1, 0));
    }

    #[test]
    fn closed_session_and_plain_tasks_do_not_block() {
        let (_dir, base) = crate::storage::test_root();
        let root = base.as_path();
        let id = "a".repeat(64);
        task(root, &id, "exclusive-edit", "a.txt");
        journal::set_lease(root, &id, &lease()).unwrap();
        journal::acknowledge_lease(root, &id).unwrap();
        journal::close(root, &id).unwrap();
        let other = "b".repeat(64);
        task(root, &other, "optimistic-edit", "b.txt");
        assert!(assess(root).safe_to_exit);
    }

    #[test]
    fn unreadable_state_is_never_safe() {
        let (_dir, base) = crate::storage::test_root();
        let root = base.as_path();
        let id = "a".repeat(64);
        let task_dir = task(root, &id, "exclusive-edit", "a.txt");
        std::fs::write(task_dir.join("journal.json"), b"{ not json").unwrap();
        let assessment = assess(root);
        assert!(!assessment.safe_to_exit);
        assert_eq!(assessment.risks[0].headline(), Reason::UnreadableState);
        std::fs::write(task_dir.join("journal.json"), b"{}").unwrap();
        std::fs::write(task_dir.join("task.json"), b"{ broken").unwrap();
        let assessment = assess(root);
        assert!(!assessment.safe_to_exit);
        assert!(assessment.risks[0]
            .reasons
            .contains(&Reason::UnreadableState));
    }
}

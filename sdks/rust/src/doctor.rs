//! The environment self-check: the guest agent's report on the pieces the
//! sandbox depends on.

use reqwest::Method;
use serde::{Deserialize, Serialize};

use crate::environments::Environments;
use crate::error::Error;
use crate::strenum::string_enum;
use crate::transport::require;

string_enum! {
    /// Outcomes carried in [`DoctorCheck::status`] and
    /// [`DoctorReport::status`].
    pub enum DoctorStatus {
        /// The check passed.
        Pass => "pass",
        /// Something is off but not broken, e.g. the display is up at a
        /// different geometry from the one declared.
        Warn => "warn",
        /// The piece is broken.
        Fail => "fail",
        /// The check does not apply, e.g. display checks on an image with no
        /// desktop. A report's status is never `skip`.
        Skip => "skip",
    }
}

/// One line of an environment's in-guest self-check. `name` is today one
/// of agent, machine-id, dbus, disk, memory, xdotool, scrot, display, wm,
/// vnc, healthcheck, dns, egress or clock; the guest agent owns the list, so
/// treat an unknown name as one more line.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct DoctorCheck {
    pub name: String,
    pub status: DoctorStatus,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub message: Option<String>,
}

/// The guest agent's self-check. `status` is the worst of the checks'
/// statuses, with skip counting as pass.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct DoctorReport {
    pub status: DoctorStatus,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub vm_id: Option<String>,
    #[serde(default)]
    pub version: String,
    #[serde(default)]
    pub checks: Vec<DoctorCheck>,
}

impl Environments {
    /// Runs the environment's in-guest self-check: the guest agent reports
    /// on itself, the display, window manager and vnc server, the
    /// healthcheck, disk, memory, dns, egress and the clock. A failing check is a
    /// [`DoctorStatus::Fail`] line in an `Ok` report, not an `Err`.
    pub async fn doctor(&self, vm_id: &str) -> Result<DoctorReport, Error> {
        require(vm_id, "vm id")?;
        let request = self
            .transport()
            .request(Method::GET, &["v1", "environments", vm_id, "doctor"]);
        self.transport().send_json(request).await
    }
}

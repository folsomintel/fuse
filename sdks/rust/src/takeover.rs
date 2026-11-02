//! Terminal takeover and the 423 lock (issue #312).
//!
//! While a human holds a takeover, or an ask-mode stall is pending, the
//! server answers the agent's exec and computer calls with 423 locked. By
//! default those calls wait for the intervention to resolve and retry, so
//! agent code needs no changes.

use std::fmt;

use reqwest::Method;
use serde::Serialize;

use crate::environments::Environments;
use crate::error::{ApiError, Error};
use crate::interventions::wait_intervention;
use crate::transport::{require, Transport};
use crate::types::{HitlKind, Intervention, TakeoverVia};

/// The body of [`Environments::takeover`].
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize)]
#[non_exhaustive]
pub struct TakeoverOptions {
    /// `None` means terminal.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub via: Option<TakeoverVia>,
    /// Recorded when no agent request is pending to claim.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub reason: Option<String>,
}

impl TakeoverOptions {
    pub fn new() -> Self {
        Self::default()
    }

    pub fn via(mut self, via: TakeoverVia) -> Self {
        self.via = Some(via);
        self
    }

    pub fn reason(mut self, reason: impl Into<String>) -> Self {
        self.reason = Some(reason.into());
        self
    }
}

/// The body of [`Environments::release`].
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize)]
#[non_exhaustive]
pub struct ReleaseOptions {
    /// Handed back to the agent as the takeover's answer.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub note: Option<String>,
}

impl ReleaseOptions {
    pub fn new() -> Self {
        Self::default()
    }

    pub fn note(mut self, note: impl Into<String>) -> Self {
        self.note = Some(note.into());
        self
    }
}

/// A 423 from exec or a computer action: a human holds the environment, or
/// an ask-mode stall is pending. Only returned when waiting is off
/// ([`ExecRequest::no_wait`](crate::ExecRequest::no_wait) or
/// [`ClientBuilder::hitl_no_wait`](crate::ClientBuilder::hitl_no_wait));
/// wait on the intervention with
/// [`Interventions::wait`](crate::Interventions::wait), then retry.
#[derive(Debug, Clone)]
#[non_exhaustive]
pub struct LockedError {
    /// The intervention holding the lock.
    pub intervention_id: String,
    /// [`HitlKind::Takeover`] or [`HitlKind::Stall`].
    pub kind: HitlKind,
    /// The server's message.
    pub message: String,
}

impl fmt::Display for LockedError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(
            f,
            "environment locked by {} intervention {}",
            self.kind, self.intervention_id
        )?;
        if !self.message.is_empty() {
            write!(f, ": {}", self.message)?;
        }
        Ok(())
    }
}

impl std::error::Error for LockedError {}

impl LockedError {
    fn from_api(err: &ApiError) -> Self {
        let detail = |key: &str| err.details.get(key).cloned().unwrap_or_default();
        Self {
            intervention_id: detail("intervention_id"),
            kind: HitlKind::from(detail("kind")),
            message: err.message.clone(),
        }
    }
}

impl Transport {
    /// Handles the error of a call the server may lock. A 423 waits until
    /// the intervention holding the lock resolves and returns `Ok`, so the
    /// caller retries; with waiting off it becomes [`Error::Locked`]. Any
    /// other error comes back unchanged. Drop the future to stop waiting.
    pub(crate) async fn wait_unlocked(&self, err: Error, no_wait: bool) -> Result<(), Error> {
        let locked = match &err {
            Error::Api(api) if api.status == 423 => LockedError::from_api(api),
            _ => return Err(err),
        };
        if no_wait || self.hitl.no_wait || locked.intervention_id.is_empty() {
            return Err(Error::Locked(Box::new(locked)));
        }
        wait_intervention(self, &locked.intervention_id).await?;
        Ok(())
    }
}

impl Environments {
    /// Takes over the environment's terminal as a human: it claims the
    /// agent's pending takeover request if there is one, else opens one, and
    /// returns it held. The agent's exec and computer calls wait until
    /// [`Environments::release`]. Requires the master token or a key with
    /// `hitl:takeover`; someone else already holding it is a conflict.
    pub async fn takeover(
        &self,
        vm_id: &str,
        options: TakeoverOptions,
    ) -> Result<Intervention, Error> {
        self.hitl_action(vm_id, "takeover", &options).await
    }

    /// Ends a takeover and hands `note` back to the agent as the answer. It
    /// is a conflict when nothing is held.
    pub async fn release(
        &self,
        vm_id: &str,
        options: ReleaseOptions,
    ) -> Result<Intervention, Error> {
        self.hitl_action(vm_id, "release", &options).await
    }

    async fn hitl_action<B: Serialize>(
        &self,
        vm_id: &str,
        action: &str,
        body: &B,
    ) -> Result<Intervention, Error> {
        require(vm_id, "vm id")?;
        let request = self
            .transport()
            .request(Method::POST, &["v1", "environments", vm_id])
            .query(&[("action", action)])
            .json(body);
        self.transport().send_json(request).await
    }
}

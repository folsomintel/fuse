use std::fmt;
use std::sync::Arc;

use reqwest::{RequestBuilder, Response, StatusCode};
use serde::Deserialize;

use crate::error::Error;
use crate::interventions::wait_intervention;
use crate::transport::Transport;
use crate::types::Intervention;

/// A callback told about the pending intervention whenever a hitl gate
/// holds a call, before the call waits on it. Set it per exec with
/// [`ExecRequest::on_held`](crate::ExecRequest::on_held) or client-wide with
/// [`ClientBuilder::on_held`](crate::ClientBuilder::on_held); the per-exec
/// one wins.
#[derive(Clone)]
pub struct OnHeld(Arc<dyn Fn(&Intervention) + Send + Sync>);

impl OnHeld {
    pub fn new(callback: impl Fn(&Intervention) + Send + Sync + 'static) -> Self {
        Self(Arc::new(callback))
    }
}

impl fmt::Debug for OnHeld {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str("OnHeld")
    }
}

// two callbacks are equal only when they are the same callback.
impl PartialEq for OnHeld {
    fn eq(&self, other: &Self) -> bool {
        Arc::ptr_eq(&self.0, &other.0)
    }
}

impl Eq for OnHeld {}

/// The client-wide hitl wait options.
#[derive(Clone, Default)]
pub(crate) struct HitlWait {
    pub(crate) no_wait: bool,
    pub(crate) on_held: Option<OnHeld>,
}

/// The answer to a gated call: its own response, or the intervention a gate
/// held it on.
pub(crate) enum Gated {
    Done(Response),
    Held(Box<Intervention>),
}

// the 202 body of a held call.
#[derive(Deserialize)]
struct HeldResponse {
    intervention: Intervention,
}

impl Transport {
    /// Sends a gated call. A 202 decodes as the held intervention; any other
    /// success is returned for the caller to read.
    pub(crate) async fn send_gated(&self, builder: RequestBuilder) -> Result<Gated, Error> {
        let response = Self::check(builder.send().await?).await?;
        if response.status() != StatusCode::ACCEPTED {
            return Ok(Gated::Done(response));
        }
        let body = response.bytes().await?;
        let held: HeldResponse = serde_json::from_slice(&body)?;
        Ok(Gated::Held(Box::new(held.intervention)))
    }

    /// Waits out a held call: it reports the hold, waits until the
    /// intervention settles, and turns anything but a successful run into
    /// an error. `on_held` and `no_wait` are the per-call overrides of the
    /// client's options. Every intervention has a deadline, so the wait
    /// always ends; drop the future to stop early.
    pub(crate) async fn settle_held(
        &self,
        held: Box<Intervention>,
        on_held: Option<&OnHeld>,
        no_wait: bool,
    ) -> Result<Intervention, Error> {
        if let Some(callback) = on_held.or(self.hitl.on_held.as_ref()) {
            (callback.0)(&held);
        }
        if no_wait || self.hitl.no_wait {
            return Err(Error::InterventionPending(held));
        }
        let intervention = wait_intervention(self, &held.id).await?;
        if !intervention.is_approved() {
            return Err(Error::InterventionRejected(Box::new(intervention)));
        }
        let failed = intervention
            .result
            .as_ref()
            .and_then(|r| r.error.as_deref())
            .is_some_and(|e| !e.is_empty());
        if failed {
            return Err(Error::HeldActionFailed(Box::new(intervention)));
        }
        Ok(intervention)
    }
}

/// Reads a JSON body off a successful response.
pub(crate) async fn decode<T: serde::de::DeserializeOwned>(response: Response) -> Result<T, Error> {
    let body = response.bytes().await?;
    Ok(serde_json::from_slice(&body)?)
}

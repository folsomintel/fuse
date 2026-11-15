use std::sync::Arc;
use std::time::{Duration, Instant};

use reqwest::Method;

use crate::error::Error;
use crate::interventions::{WAIT_HTTP_OVERHEAD, WAIT_POLL};
use crate::operations::CallOptions;
use crate::transport::{require, Transport};
use crate::types::{Build, CreateBuildRequest};

/// The builds service, from [`Client::builds`](crate::Client::builds).
///
/// A build runs a setup in a builder environment, caching each keyed step
/// as a layer, and files the result as a snapshot.
#[derive(Clone)]
pub struct Builds {
    t: Arc<Transport>,
}

impl Builds {
    pub(crate) fn new(t: Arc<Transport>) -> Self {
        Self { t }
    }

    /// Starts a build and returns it, still running. The same idempotency
    /// key with the same request returns the same build; with a different
    /// request it is a 409 conflict. `options` without a key sends a fresh
    /// random one.
    pub async fn create(
        &self,
        request: CreateBuildRequest,
        options: CallOptions,
    ) -> Result<Build, Error> {
        require(&request.name, "build name")?;
        let builder = self
            .t
            .request(Method::POST, &["v1", "builds"])
            .json(&request);
        self.t.send_json(options.apply(builder)).await
    }

    /// Returns one build. With `wait` the server holds the call until the
    /// build is no longer running or `wait` passes (capped by the server);
    /// without it the call answers at once.
    pub async fn get(&self, id: &str, wait: Option<Duration>) -> Result<Build, Error> {
        get_build(&self.t, id, wait, None).await
    }

    /// Long-polls until the build finishes and returns it. A failed build
    /// is returned, not an error. With `timeout` it returns the
    /// still-running build once `timeout` passes; without it, it waits
    /// until the build finishes. Drop the future to stop early.
    pub async fn wait(&self, id: &str, timeout: Option<Duration>) -> Result<Build, Error> {
        let deadline = timeout.map(|timeout| Instant::now() + timeout);
        loop {
            let remaining =
                deadline.map(|deadline| deadline.saturating_duration_since(Instant::now()));
            let poll = remaining.map_or(WAIT_POLL, |remaining| remaining.min(WAIT_POLL));
            let expired = || deadline.is_some_and(|deadline| Instant::now() >= deadline);
            let build = match get_build(&self.t, id, Some(poll), remaining).await {
                // the poll ran into the deadline: report where the build is now.
                Err(Error::Transport(e)) if e.is_timeout() && expired() => {
                    return get_build(&self.t, id, None, None).await;
                }
                other => other?,
            };
            let expired = expired();
            if build.is_terminal() || expired {
                return Ok(build);
            }
        }
    }
}

// get_build reads one build. a waiting read runs on the no-timeout stream
// client with its own deadline, capped by limit when given.
async fn get_build(
    t: &Transport,
    id: &str,
    wait: Option<Duration>,
    limit: Option<Duration>,
) -> Result<Build, Error> {
    require(id, "build id")?;
    let request = match wait {
        Some(wait) => t
            .stream_request(Method::GET, &["v1", "builds", id])
            .query(&[("wait", format!("{}ms", wait.as_millis()))])
            .timeout(limit.map_or(wait + WAIT_HTTP_OVERHEAD, |limit| {
                limit.min(wait + WAIT_HTTP_OVERHEAD)
            })),
        None => t.request(Method::GET, &["v1", "builds", id]),
    };
    t.send_json(request).await
}

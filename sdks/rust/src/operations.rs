use std::fmt;
use std::sync::Arc;
use std::time::{Duration, Instant};

use reqwest::{Method, RequestBuilder, StatusCode};
use serde::Deserialize;

use crate::error::{parse_api_error, Error};
use crate::interventions::{WAIT_HTTP_OVERHEAD, WAIT_POLL};
use crate::retry::{jitter, retry_delay};
use crate::transport::{require, Transport};
use crate::types::{Operation, OperationAccepted, OperationState};

pub(crate) const IDEMPOTENCY_KEY_HEADER: &str = "idempotency-key";

/// Per-call options for the mutating calls that run as operations:
/// environments create, destroy, fork, migrate and drain, snapshots create
/// and restore, and images convert.
///
/// ```
/// use fuse::CallOptions;
///
/// let options = CallOptions::new().idempotency_key("deploy-42");
/// ```
#[derive(Debug, Clone, Default)]
#[non_exhaustive]
pub struct CallOptions {
    /// Sent as `Idempotency-Key` (1-255 visible ascii). The same key with
    /// the same request attaches to, or replays, the first call's
    /// operation; with a different request it is a 409 conflict. `None`
    /// sends a fresh random key, generated once per call.
    pub idempotency_key: Option<String>,
}

impl CallOptions {
    pub fn new() -> Self {
        Self::default()
    }

    pub fn idempotency_key(mut self, key: impl Into<String>) -> Self {
        self.idempotency_key = Some(key.into());
        self
    }

    // apply sets the idempotency key header, generating one when the caller
    // gave none. a builder is built once per call, so the key is too.
    pub(crate) fn apply(&self, builder: RequestBuilder) -> RequestBuilder {
        let key = self
            .idempotency_key
            .clone()
            .filter(|key| !key.is_empty())
            .unwrap_or_else(|| uuid::Uuid::new_v4().to_string());
        builder.header(IDEMPOTENCY_KEY_HEADER, key)
    }
}

/// A callback told about the operation whenever a mutating call answers 202
/// with one, before the call starts following it. Set it client-wide with
/// [`ClientBuilder::on_operation`](crate::ClientBuilder::on_operation).
#[derive(Clone)]
pub struct OnOperation(pub(crate) Arc<dyn Fn(&OperationAccepted) + Send + Sync>);

impl OnOperation {
    pub fn new(callback: impl Fn(&OperationAccepted) + Send + Sync + 'static) -> Self {
        Self(Arc::new(callback))
    }
}

impl fmt::Debug for OnOperation {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str("OnOperation")
    }
}

/// The operations service, from
/// [`Client::operations`](crate::Client::operations).
///
/// Mutating calls follow their own operation transparently when the server
/// answers 202 with one, so this is only needed to look one up by the id in
/// a `Fuse-Operation-Id` header. Operations live in the orchestrator's
/// memory for 24h after they finish; unknown ones are not found.
#[derive(Clone)]
pub struct Operations {
    t: Arc<Transport>,
}

impl Operations {
    pub(crate) fn new(t: Arc<Transport>) -> Self {
        Self { t }
    }

    /// Returns one operation. With `wait` the server holds the call until
    /// the operation finishes or `wait` passes (capped by the server);
    /// without it the call answers at once.
    pub async fn get(&self, id: &str, wait: Option<Duration>) -> Result<Operation, Error> {
        get_operation(&self.t, id, wait, None).await
    }

    /// Long-polls until the operation finishes and returns it. With
    /// `timeout` it returns the still-running operation once `timeout`
    /// passes; without it, it waits until the operation finishes. Drop the
    /// future to stop early.
    pub async fn wait(&self, id: &str, timeout: Option<Duration>) -> Result<Operation, Error> {
        wait_operation(&self.t, id, timeout).await
    }
}

// get_operation reads one operation. a waiting read runs on the no-timeout
// stream client with its own deadline, so a short client timeout cannot cut
// a held poll short.
// limit, when given, caps a waiting read's http timeout.
pub(crate) async fn get_operation(
    t: &Transport,
    id: &str,
    wait: Option<Duration>,
    limit: Option<Duration>,
) -> Result<Operation, Error> {
    require(id, "operation id")?;
    let request = match wait {
        Some(wait) => t
            .stream_request(Method::GET, &["v1", "operations", id])
            .query(&[("wait", format!("{}ms", wait.as_millis()))])
            .timeout(limit.map_or(wait + WAIT_HTTP_OVERHEAD, |limit| {
                limit.min(wait + WAIT_HTTP_OVERHEAD)
            })),
        None => t.request(Method::GET, &["v1", "operations", id]),
    };
    t.send_json(request).await
}

// wait_operation long-polls until the operation finishes, or until timeout
// passes when one is given.
pub(crate) async fn wait_operation(
    t: &Transport,
    id: &str,
    timeout: Option<Duration>,
) -> Result<Operation, Error> {
    let deadline = timeout.map(|timeout| Instant::now() + timeout);
    loop {
        let remaining = deadline.map(|deadline| deadline.saturating_duration_since(Instant::now()));
        let poll = remaining.map_or(WAIT_POLL, |remaining| remaining.min(WAIT_POLL));
        let expired = || deadline.is_some_and(|deadline| Instant::now() >= deadline);
        let operation = match get_operation(t, id, Some(poll), remaining).await {
            // the poll ran into the deadline: report where the operation is now.
            Err(Error::Transport(e)) if e.is_timeout() && expired() => {
                return get_operation(t, id, None, None).await;
            }
            other => other?,
        };
        let expired = expired();
        if operation.is_terminal() || expired {
            return Ok(operation);
        }
    }
}

// the 202 body of a call whose operation outlasted the server's wait. it is
// told apart from a held response, or a 202 the call answers itself, by its
// operation_id.
#[derive(Deserialize)]
struct Accepted {
    #[serde(default)]
    operation_id: String,
}

impl Transport {
    /// Sends a mutating call and returns the status and body it answered
    /// with. A 202 naming an operation is followed until the operation
    /// finishes, and its answer is returned as if the call had answered
    /// with it; any other 202 is returned as it came. A keyed call answered
    /// 503 (no capacity, restate down) is sent again under the same
    /// idempotency key, up to the client's retries, waiting at least the
    /// server's Retry-After plus jitter between sends.
    pub(crate) async fn send_operation(
        &self,
        mut builder: RequestBuilder,
    ) -> Result<(u16, Vec<u8>), Error> {
        // only a keyed call is safe to send twice; exec and computer actions
        // come through here without a key and are never retried.
        let keyed = builder
            .try_clone()
            .and_then(|b| b.build().ok())
            .is_some_and(|r| r.headers().contains_key(IDEMPOTENCY_KEY_HEADER));
        let mut attempt = 0;
        loop {
            // a body that cannot be cloned cannot be sent twice.
            let next = if keyed && attempt < self.retries {
                builder.try_clone()
            } else {
                None
            };
            let mut retry_after = None;
            let result = self.send_operation_once(builder, &mut retry_after).await;
            match (result, next) {
                (Err(Error::Api(e)), Some(next)) if e.status == 503 => {
                    tokio::time::sleep(retry_delay(retry_after.as_deref(), attempt, jitter()))
                        .await;
                    builder = next;
                    attempt += 1;
                }
                (result, _) => return result,
            }
        }
    }

    // send_operation_once is one send of send_operation. retry_after is set
    // to the answer's Retry-After header.
    async fn send_operation_once(
        &self,
        builder: RequestBuilder,
        retry_after: &mut Option<String>,
    ) -> Result<(u16, Vec<u8>), Error> {
        let response = builder.send().await?;
        *retry_after = response
            .headers()
            .get(reqwest::header::RETRY_AFTER)
            .and_then(|value| value.to_str().ok())
            .map(str::to_owned);
        let response = Self::check(response).await?;
        let status = response.status().as_u16();
        let body = response.bytes().await?.to_vec();
        if status != StatusCode::ACCEPTED.as_u16() {
            return Ok((status, body));
        }
        let accepted: Accepted = serde_json::from_slice(&body)?;
        if accepted.operation_id.is_empty() {
            return Ok((status, body));
        }
        let accepted: OperationAccepted = serde_json::from_slice(&body)?;
        if let Some(callback) = &self.on_operation {
            (callback.0)(&accepted);
        }
        let operation = wait_operation(self, &accepted.operation_id, None).await?;
        // the 202 carried no Retry-After; the failed answer's rides on the
        // operation.
        if let Some(secs) = operation.retry_after {
            *retry_after = Some(secs.to_string());
        }
        operation_answer(operation)
    }
}

// operation_answer turns a finished operation into the status and body the
// call itself would have answered with, and a non-2xx one into the same
// error that answer would have been.
pub(crate) fn operation_answer(operation: Operation) -> Result<(u16, Vec<u8>), Error> {
    let fallback = if operation.state == OperationState::Succeeded {
        200
    } else {
        500
    };
    let status = operation.status_code.unwrap_or(fallback);
    if (200..300).contains(&status) {
        let body = match &operation.result {
            Some(result) => serde_json::to_vec(result)?,
            None => Vec::new(),
        };
        return Ok((status, body));
    }
    let body = match &operation.error {
        Some(error) => serde_json::to_vec(&serde_json::json!({ "error": error }))?,
        None => Vec::new(),
    };
    let status_text = StatusCode::from_u16(status)
        .ok()
        .and_then(|status| status.canonical_reason());
    Err(parse_api_error(status, status_text, None, &body).into())
}

use std::sync::Arc;
use std::time::Duration;

use reqwest::Method;
use serde::ser::SerializeStruct;
use serde::{Deserialize, Serialize, Serializer};

use crate::error::Error;
use crate::events::EventStream;
use crate::gates::{decode, Gated, OnHeld};
use crate::interventions::{wait_intervention, HumanRequest};
use crate::transport::{pagination, require, Transport, MAX_PAGE_LIMIT};
use crate::types::{
    CreateRequest, EnvironmentInfo, EnvironmentState, ForkOptions, Intervention, MigrateOptions,
};

// mirrors the orchestrator's own default guest-command bound. it is what the
// server applies when the timeout is omitted, so the client waits at least
// that long before giving up.
const DEFAULT_EXEC_SERVER_TIMEOUT: Duration = Duration::from_secs(60);

// headroom on top of the guest timeout for the hops the guest timeout does
// not cover: the ssh connect on the host and the network round trip. without
// it a command that runs for exactly its guest timeout would race the
// client's deadline.
const EXEC_HTTP_OVERHEAD: Duration = Duration::from_secs(30);

/// Filters and pagination for [`Environments::list`] and
/// [`Environments::list_page`].
#[derive(Debug, Clone, Default)]
#[non_exhaustive]
pub struct ListEnvironmentsOptions {
    pub task_id: Option<String>,
    pub state: Option<EnvironmentState>,
    pub host_id: Option<String>,
    /// Caps the page size (server default 50, max 200). Only consulted by
    /// [`Environments::list_page`] — [`Environments::list`] always requests
    /// the server's max page size internally since it walks every page.
    pub limit: Option<u32>,
    /// Resumes after a previous page's [`EnvironmentPage::next_cursor`]. It
    /// is an opaque token; treat it as such rather than parsing it.
    pub cursor: Option<String>,
}

impl ListEnvironmentsOptions {
    pub fn new() -> Self {
        Self::default()
    }

    pub fn task_id(mut self, task_id: impl Into<String>) -> Self {
        self.task_id = Some(task_id.into());
        self
    }

    pub fn state(mut self, state: EnvironmentState) -> Self {
        self.state = Some(state);
        self
    }

    pub fn host_id(mut self, host_id: impl Into<String>) -> Self {
        self.host_id = Some(host_id.into());
        self
    }

    pub fn limit(mut self, limit: u32) -> Self {
        self.limit = Some(limit);
        self
    }

    pub fn cursor(mut self, cursor: impl Into<String>) -> Self {
        self.cursor = Some(cursor.into());
        self
    }
}

/// One page of a list call, plus the cursor to fetch the next one.
#[derive(Debug, Clone, Default, Deserialize)]
pub struct EnvironmentPage {
    #[serde(default)]
    pub environments: Vec<EnvironmentInfo>,
    /// `None` once there are no more results.
    #[serde(default)]
    pub next_cursor: Option<String>,
}

/// The command half of an [`ExecRequest`]. Exactly one of argv and shell
/// goes on the wire, which this enum makes impossible to get wrong.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum ExecCommand {
    /// An argv run directly in the guest, e.g. `["ls", "-l"]`. Argv needs
    /// no quoting rules and cannot be turned into an injection by
    /// interpolating a value, so prefer it.
    Argv(Vec<String>),
    /// A string run under `sh -lc`. Use it only for what argv cannot
    /// express: pipelines, redirects, and globs.
    Shell(String),
}

/// The body of [`Environments::exec`].
///
/// ```
/// use std::time::Duration;
/// use fuse::ExecRequest;
///
/// let ls = ExecRequest::cmd(["ls", "-l", "/tmp"]);
/// let pipeline = ExecRequest::shell("dmesg | tail -n 5").timeout(Duration::from_secs(10));
/// ```
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ExecRequest {
    pub command: ExecCommand,
    /// Bounds the command inside the guest, in milliseconds. `None` takes
    /// the server default; the server clamps anything above its ceiling.
    pub timeout_ms: Option<u64>,
    /// Waits for a human when a hitl gate holds the exec. Default true;
    /// when false the call returns [`Error::InterventionPending`] at once.
    pub wait: bool,
    /// Called with the pending intervention when a gate holds the exec,
    /// before waiting on it. Overrides the client's
    /// [`ClientBuilder::on_held`](crate::ClientBuilder::on_held).
    pub on_held: Option<OnHeld>,
}

impl ExecRequest {
    /// Returns a request running `argv` directly in the guest.
    pub fn cmd<I, S>(argv: I) -> Self
    where
        I: IntoIterator<Item = S>,
        S: Into<String>,
    {
        Self {
            command: ExecCommand::Argv(argv.into_iter().map(Into::into).collect()),
            timeout_ms: None,
            wait: true,
            on_held: None,
        }
    }

    /// Returns a request running `script` under `sh -lc`.
    pub fn shell(script: impl Into<String>) -> Self {
        Self {
            command: ExecCommand::Shell(script.into()),
            timeout_ms: None,
            wait: true,
            on_held: None,
        }
    }

    /// Bounds the command inside the guest. Sub-millisecond precision is
    /// truncated.
    pub fn timeout(mut self, timeout: Duration) -> Self {
        self.timeout_ms = Some(timeout.as_millis().try_into().unwrap_or(u64::MAX));
        self
    }

    pub fn timeout_ms(mut self, timeout_ms: u64) -> Self {
        self.timeout_ms = Some(timeout_ms);
        self
    }

    /// Returns [`Error::InterventionPending`] at once when a hitl gate holds
    /// the exec, instead of waiting for a human to decide.
    pub fn no_wait(mut self) -> Self {
        self.wait = false;
        self
    }

    /// Calls `callback` with the pending intervention when a hitl gate holds
    /// the exec, before waiting on it.
    pub fn on_held(mut self, callback: impl Fn(&Intervention) + Send + Sync + 'static) -> Self {
        self.on_held = Some(OnHeld::new(callback));
        self
    }
}

impl Serialize for ExecRequest {
    fn serialize<S: Serializer>(&self, serializer: S) -> Result<S::Ok, S::Error> {
        let mut body = serializer.serialize_struct("ExecRequest", 2)?;
        match &self.command {
            ExecCommand::Argv(argv) => body.serialize_field("cmd", argv)?,
            ExecCommand::Shell(script) => body.serialize_field("shell", script)?,
        }
        match self.timeout_ms {
            Some(timeout_ms) => body.serialize_field("timeout_ms", &timeout_ms)?,
            None => body.skip_field("timeout_ms")?,
        }
        body.end()
    }
}

/// The outcome of a guest command.
///
/// A non-zero exit code is a successful call: the command ran and failed.
/// Only an `Err` from [`Environments::exec`] means the command could not be
/// run at all.
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(default)]
pub struct ExecResult {
    pub exit_code: i32,
    pub stdout: String,
    pub stderr: String,
}

impl ExecResult {
    /// Reports whether the command exited zero.
    pub fn success(&self) -> bool {
        self.exit_code == 0
    }
}

/// The environments service, from
/// [`Client::environments`](crate::Client::environments).
///
/// Exec, destroy, fork, migrate, drain and rotate-token can be held by a
/// hitl gate. A held call waits for the human transparently and returns what
/// it normally returns; see [`Error::InterventionPending`],
/// [`Error::InterventionRejected`] and [`Error::HeldActionFailed`] for the
/// other outcomes.
#[derive(Clone)]
pub struct Environments {
    t: Arc<Transport>,
}

impl Environments {
    pub(crate) fn new(t: Arc<Transport>) -> Self {
        Self { t }
    }

    pub(crate) fn transport(&self) -> &Transport {
        &self.t
    }

    /// Returns every environment matching `options`, transparently walking
    /// every result page. For explicit single-page control (e.g. a CLI
    /// `--cursor` flag), use [`Environments::list_page`].
    pub async fn list(
        &self,
        options: ListEnvironmentsOptions,
    ) -> Result<Vec<EnvironmentInfo>, Error> {
        let mut out = Vec::new();
        let mut page_options = options;
        page_options.limit = Some(MAX_PAGE_LIMIT);
        loop {
            let page = self.list_page(page_options.clone()).await?;
            out.extend(page.environments);
            match page.next_cursor {
                Some(cursor) if !cursor.is_empty() => page_options.cursor = Some(cursor),
                _ => return Ok(out),
            }
        }
    }

    /// Returns one page of environments matching `options`.
    pub async fn list_page(
        &self,
        options: ListEnvironmentsOptions,
    ) -> Result<EnvironmentPage, Error> {
        let mut query: Vec<(&str, String)> = Vec::new();
        if let Some(task_id) = &options.task_id {
            query.push(("task_id", task_id.clone()));
        }
        if let Some(state) = &options.state {
            query.push(("state", state.to_string()));
        }
        if let Some(host_id) = &options.host_id {
            query.push(("host_id", host_id.clone()));
        }
        pagination(&mut query, options.limit, &options.cursor);
        let request = self
            .t
            .request(Method::GET, &["v1", "environments"])
            .query(&query);
        self.t.send_json(request).await
    }

    /// Returns one environment by id.
    pub async fn get(&self, vm_id: &str) -> Result<EnvironmentInfo, Error> {
        require(vm_id, "vm id")?;
        let request = self.t.request(Method::GET, &["v1", "environments", vm_id]);
        self.t.send_json(request).await
    }

    /// Creates an environment. The call returns as soon as the VM is up;
    /// follow [`Environments::events`] to watch it settle.
    pub async fn create(&self, request: CreateRequest) -> Result<EnvironmentInfo, Error> {
        let request = self
            .t
            .request(Method::POST, &["v1", "environments"])
            .json(&request);
        self.t.send_json(request).await
    }

    /// Starts draining an environment.
    pub async fn drain(&self, vm_id: &str) -> Result<EnvironmentInfo, Error> {
        self.action(vm_id, "drain").await
    }

    /// Creates a new environment seeded from an existing one and returns
    /// the new environment. When `options.reuse_snapshot_id` is `None` the
    /// server snapshots the source first; otherwise it reuses the named
    /// snapshot.
    pub async fn fork(&self, vm_id: &str, options: ForkOptions) -> Result<EnvironmentInfo, Error> {
        require(vm_id, "vm id")?;
        let request = self
            .t
            .request(Method::POST, &["v1", "environments", vm_id])
            .query(&[("action", "fork")])
            .json(&options);
        self.send_env(request).await
    }

    /// Moves an environment to another host and returns the new environment.
    /// The source is drained and destroyed once the new one is running.
    pub async fn migrate(
        &self,
        vm_id: &str,
        options: MigrateOptions,
    ) -> Result<EnvironmentInfo, Error> {
        require(vm_id, "vm id")?;
        let request = self
            .t
            .request(Method::POST, &["v1", "environments", vm_id])
            .query(&[("action", "migrate")])
            .json(&options);
        self.send_env(request).await
    }

    /// Rotates the environment's own credentials.
    pub async fn rotate_token(&self, vm_id: &str) -> Result<(), Error> {
        require(vm_id, "vm id")?;
        let request = self
            .t
            .request(Method::POST, &["v1", "environments", vm_id])
            .query(&[("action", "rotate-token")]);
        self.send_unit_gated(request).await
    }

    /// Destroys an environment.
    pub async fn destroy(&self, vm_id: &str) -> Result<(), Error> {
        require(vm_id, "vm id")?;
        let request = self
            .t
            .request(Method::DELETE, &["v1", "environments", vm_id]);
        self.send_unit_gated(request).await
    }

    /// Runs a command inside a running environment's guest and returns its
    /// exit code with stdout and stderr kept separate.
    ///
    /// Exec requires the master token.
    ///
    /// When a hitl gate holds the exec, the call waits for the decision; the
    /// orchestrator runs the approved (possibly edited) argv and this
    /// returns its result. A rejection is [`Error::InterventionRejected`], a
    /// deny gate an error [`Error::is_hitl_denied`] matches.
    pub async fn exec(&self, vm_id: &str, request: ExecRequest) -> Result<ExecResult, Error> {
        require(vm_id, "vm id")?;
        match &request.command {
            ExecCommand::Argv(argv) if argv.is_empty() => {
                return Err(Error::InvalidRequest("cmd must not be empty".to_owned()));
            }
            ExecCommand::Shell(script) if script.is_empty() => {
                return Err(Error::InvalidRequest("shell must not be empty".to_owned()));
            }
            _ => {}
        }

        // a guest command can run far longer than the default client
        // timeout, so this uses the no-timeout stream client and bounds the
        // call by a per-request deadline derived from the requested guest
        // timeout instead. without this the 60s client default would cut a
        // longer command short awaiting headers.
        let guest = request
            .timeout_ms
            .filter(|&ms| ms > 0)
            .map(Duration::from_millis)
            .unwrap_or(DEFAULT_EXEC_SERVER_TIMEOUT);
        let http_request = self
            .t
            .stream_request(Method::POST, &["v1", "environments", vm_id])
            .query(&[("action", "exec")])
            .timeout(guest + EXEC_HTTP_OVERHEAD)
            .json(&request);
        // the guest-timeout deadline above bounds only this request. the
        // wait for a human polls with its own per-request deadlines.
        let held = match self.t.send_gated(http_request).await? {
            Gated::Done(response) => return decode(response).await,
            Gated::Held(held) => held,
        };
        let settled = self
            .t
            .settle_held(held, request.on_held.as_ref(), !request.wait)
            .await?;
        match settled.result.as_ref().and_then(|r| r.exec.clone()) {
            Some(result) => Ok(result),
            None => Err(Error::HeldActionFailed(Box::new(settled))),
        }
    }

    /// Asks a human for something the environment's hitl policy declares.
    /// By default it waits until the intervention resolves (answered,
    /// rejected, expired or cancelled) and returns it; read the outcome off
    /// [`Intervention::decision`]. With [`HumanRequest::no_wait`] it returns
    /// the pending intervention at once.
    pub async fn request_human(
        &self,
        vm_id: &str,
        request: HumanRequest,
    ) -> Result<Intervention, Error> {
        require(vm_id, "vm id")?;
        let http_request = self
            .t
            .request(Method::POST, &["v1", "environments", vm_id])
            .query(&[("action", "request")])
            .json(&request);
        let intervention: Intervention = self.t.send_json(http_request).await?;
        if !request.wait {
            return Ok(intervention);
        }
        wait_intervention(&self.t, &intervention.id).await
    }

    /// Opens the SSE event stream for an environment. The stream ends
    /// cleanly after a terminal state event (destroyed/failed) or on EOF;
    /// drop it to stop early.
    pub async fn events(&self, vm_id: &str) -> Result<EventStream, Error> {
        require(vm_id, "vm id")?;
        let response = self
            .t
            .stream_request(Method::GET, &["v1", "environments", vm_id, "events"])
            .header(reqwest::header::ACCEPT, "text/event-stream")
            .send()
            .await?;
        let response = Transport::check(response).await?;
        Ok(EventStream::new(response))
    }

    async fn action(&self, vm_id: &str, action: &str) -> Result<EnvironmentInfo, Error> {
        require(vm_id, "vm id")?;
        let request = self
            .t
            .request(Method::POST, &["v1", "environments", vm_id])
            .query(&[("action", action)]);
        match self.t.send_gated(request).await? {
            Gated::Done(response) => decode(response).await,
            Gated::Held(held) => {
                self.t.settle_held(held, None, false).await?;
                self.get(vm_id).await
            }
        }
    }

    // send_env sends a fork or migrate. a held one is waited out and the
    // environment it produced is fetched.
    async fn send_env(&self, request: reqwest::RequestBuilder) -> Result<EnvironmentInfo, Error> {
        let held = match self.t.send_gated(request).await? {
            Gated::Done(response) => return decode(response).await,
            Gated::Held(held) => held,
        };
        let settled = self.t.settle_held(held, None, false).await?;
        match settled.result.as_ref().and_then(|r| r.env_id.clone()) {
            Some(env_id) if !env_id.is_empty() => self.get(&env_id).await,
            _ => Err(Error::HeldActionFailed(Box::new(settled))),
        }
    }

    // send_unit_gated sends a call whose success has no body, waiting it out
    // when a gate holds it.
    async fn send_unit_gated(&self, request: reqwest::RequestBuilder) -> Result<(), Error> {
        if let Gated::Held(held) = self.t.send_gated(request).await? {
            self.t.settle_held(held, None, false).await?;
        }
        Ok(())
    }
}

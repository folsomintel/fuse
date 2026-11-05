use std::sync::Arc;
use std::time::Duration;

use reqwest::Method;
use serde::{Deserialize, Serialize};

use crate::error::Error;
use crate::transport::{pagination, require, Transport, MAX_PAGE_LIMIT};
use crate::types::{AuditEntry, HitlKind, Intervention, InterventionState};

// how long one long-poll asks the server to hold. the server caps it at 50s.
const WAIT_POLL: Duration = Duration::from_secs(30);

// headroom on top of the long-poll for the network round trip, so a poll
// that the server holds for its full duration does not race the client's
// deadline.
const WAIT_HTTP_OVERHEAD: Duration = Duration::from_secs(30);

/// The body of
/// [`Environments::request_human`](crate::Environments::request_human).
///
/// ```
/// use fuse::HumanRequest;
///
/// let request = HumanRequest::ask("deploy to prod?").choices(["yes", "no"]).timeout_seconds(300);
/// let handle_only = HumanRequest::ask("anything to add?").no_wait();
/// ```
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct HumanRequest {
    pub kind: HitlKind,
    pub question: String,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub choices: Vec<String>,
    /// Shortens the policy's deadline. It can never extend it.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub timeout_seconds: Option<u64>,
    /// Waits for a decision before returning. Default true; when false the
    /// pending intervention comes back at once, to wait on later with
    /// [`Interventions::wait`].
    #[serde(skip)]
    pub wait: bool,
}

impl HumanRequest {
    /// An ask request for `question` that waits for a decision.
    pub fn ask(question: impl Into<String>) -> Self {
        Self {
            kind: HitlKind::Ask,
            question: question.into(),
            choices: Vec::new(),
            timeout_seconds: None,
            wait: true,
        }
    }

    /// Restricts the answer to one of `choices`.
    pub fn choices<I, S>(mut self, choices: I) -> Self
    where
        I: IntoIterator<Item = S>,
        S: Into<String>,
    {
        self.choices = choices.into_iter().map(Into::into).collect();
        self
    }

    pub fn timeout_seconds(mut self, seconds: u64) -> Self {
        self.timeout_seconds = Some(seconds);
        self
    }

    /// Returns the pending intervention at once instead of waiting.
    pub fn no_wait(mut self) -> Self {
        self.wait = false;
        self
    }
}

/// The body of [`Interventions::approve_with`]: an approval of a held
/// action or an answer to an ask.
///
/// ```
/// use fuse::ApproveOptions;
///
/// let edit = ApproveOptions::new().edited_argv(["git", "push"]);
/// let repeat = ApproveOptions::new().grant("ttl:30m").reason("ok for the next half hour");
/// ```
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize)]
#[non_exhaustive]
pub struct ApproveOptions {
    /// Required to approve an ask.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub answer: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub reason: Option<String>,
    /// Replaces a held exec's argv. It is re-checked against the deny
    /// gates, and cannot be combined with `grant`.
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub edited_argv: Vec<String>,
    /// `once`, `env` or `ttl:<duration>`; `None` takes the gate's default.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub grant: Option<String>,
}

impl ApproveOptions {
    pub fn new() -> Self {
        Self::default()
    }

    pub fn answer(mut self, answer: impl Into<String>) -> Self {
        self.answer = Some(answer.into());
        self
    }

    pub fn reason(mut self, reason: impl Into<String>) -> Self {
        self.reason = Some(reason.into());
        self
    }

    pub fn edited_argv<I, S>(mut self, argv: I) -> Self
    where
        I: IntoIterator<Item = S>,
        S: Into<String>,
    {
        self.edited_argv = argv.into_iter().map(Into::into).collect();
        self
    }

    pub fn grant(mut self, grant: impl Into<String>) -> Self {
        self.grant = Some(grant.into());
        self
    }
}

// non_empty maps the empty string of the positional approve and reject
// arguments to an absent field.
fn non_empty(value: &str) -> Option<String> {
    (!value.is_empty()).then(|| value.to_owned())
}

/// Filters and pagination for [`Interventions::list`] and
/// [`Interventions::list_page`].
#[derive(Debug, Clone, Default)]
#[non_exhaustive]
pub struct ListInterventionsOptions {
    pub env_id: Option<String>,
    pub state: Option<InterventionState>,
    /// Filters by the actor id that raised or decided the intervention.
    pub actor: Option<String>,
    /// Caps the page size. Only consulted by [`Interventions::list_page`];
    /// [`Interventions::list`] always requests the server's max page size.
    pub limit: Option<u32>,
    /// Resumes after a previous page's [`InterventionPage::next_cursor`].
    pub cursor: Option<String>,
}

impl ListInterventionsOptions {
    pub fn new() -> Self {
        Self::default()
    }

    pub fn env_id(mut self, env_id: impl Into<String>) -> Self {
        self.env_id = Some(env_id.into());
        self
    }

    pub fn state(mut self, state: InterventionState) -> Self {
        self.state = Some(state);
        self
    }

    pub fn actor(mut self, actor: impl Into<String>) -> Self {
        self.actor = Some(actor.into());
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

/// One page of interventions, oldest first, plus the cursor to fetch the
/// next one.
#[derive(Debug, Clone, Default, Deserialize)]
pub struct InterventionPage {
    #[serde(default)]
    pub interventions: Vec<Intervention>,
    /// `None` once there are no more results.
    #[serde(default)]
    pub next_cursor: Option<String>,
}

/// Filters and pagination for [`Interventions::audit`] and
/// [`Interventions::audit_page`].
#[derive(Debug, Clone, Default)]
#[non_exhaustive]
pub struct AuditOptions {
    pub env_id: Option<String>,
    /// Filters by actor id.
    pub actor: Option<String>,
    /// Only rows at or after this time: RFC 3339, or a duration ago like
    /// `24h`.
    pub since: Option<String>,
    /// Caps the page size. Only consulted by [`Interventions::audit_page`];
    /// [`Interventions::audit`] always requests the server's max page size.
    pub limit: Option<u32>,
    /// Resumes after a previous page's [`AuditPage::next_cursor`].
    pub cursor: Option<String>,
}

impl AuditOptions {
    pub fn new() -> Self {
        Self::default()
    }

    pub fn env_id(mut self, env_id: impl Into<String>) -> Self {
        self.env_id = Some(env_id.into());
        self
    }

    pub fn actor(mut self, actor: impl Into<String>) -> Self {
        self.actor = Some(actor.into());
        self
    }

    pub fn since(mut self, since: impl Into<String>) -> Self {
        self.since = Some(since.into());
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

/// One page of the hitl audit log, oldest first, plus the cursor to fetch
/// the next one.
#[derive(Debug, Clone, Default, Deserialize)]
pub struct AuditPage {
    #[serde(default)]
    pub entries: Vec<AuditEntry>,
    /// `None` once there are no more results.
    #[serde(default)]
    pub next_cursor: Option<String>,
}

/// The interventions service, from
/// [`Client::interventions`](crate::Client::interventions). It reads and
/// decides human-in-the-loop interventions.
#[derive(Clone)]
pub struct Interventions {
    t: Arc<Transport>,
}

impl Interventions {
    pub(crate) fn new(t: Arc<Transport>) -> Self {
        Self { t }
    }

    /// Returns every intervention matching `options`, oldest first, walking
    /// every result page.
    pub async fn list(
        &self,
        options: ListInterventionsOptions,
    ) -> Result<Vec<Intervention>, Error> {
        let mut out = Vec::new();
        let mut page_options = options;
        page_options.limit = Some(MAX_PAGE_LIMIT);
        loop {
            let page = self.list_page(page_options.clone()).await?;
            out.extend(page.interventions);
            match page.next_cursor {
                Some(cursor) if !cursor.is_empty() => page_options.cursor = Some(cursor),
                _ => return Ok(out),
            }
        }
    }

    /// Returns one page of interventions matching `options`.
    pub async fn list_page(
        &self,
        options: ListInterventionsOptions,
    ) -> Result<InterventionPage, Error> {
        let mut query: Vec<(&str, String)> = Vec::new();
        if let Some(env_id) = &options.env_id {
            query.push(("env", env_id.clone()));
        }
        if let Some(state) = &options.state {
            query.push(("state", state.to_string()));
        }
        if let Some(actor) = &options.actor {
            query.push(("actor", actor.clone()));
        }
        pagination(&mut query, options.limit, &options.cursor);
        let request = self
            .t
            .request(Method::GET, &["v1", "interventions"])
            .query(&query);
        self.t.send_json(request).await
    }

    /// Returns every hitl audit row matching `options`, oldest first,
    /// walking every result page.
    pub async fn audit(&self, options: AuditOptions) -> Result<Vec<AuditEntry>, Error> {
        let mut out = Vec::new();
        let mut page_options = options;
        page_options.limit = Some(MAX_PAGE_LIMIT);
        loop {
            let page = self.audit_page(page_options.clone()).await?;
            out.extend(page.entries);
            match page.next_cursor {
                Some(cursor) if !cursor.is_empty() => page_options.cursor = Some(cursor),
                _ => return Ok(out),
            }
        }
    }

    /// Returns one page of the hitl audit log matching `options`.
    pub async fn audit_page(&self, options: AuditOptions) -> Result<AuditPage, Error> {
        let mut query: Vec<(&str, String)> = Vec::new();
        if let Some(env_id) = &options.env_id {
            query.push(("env", env_id.clone()));
        }
        if let Some(actor) = &options.actor {
            query.push(("actor", actor.clone()));
        }
        if let Some(since) = &options.since {
            query.push(("since", since.clone()));
        }
        pagination(&mut query, options.limit, &options.cursor);
        let request = self
            .t
            .request(Method::GET, &["v1", "hitl", "audit"])
            .query(&query);
        self.t.send_json(request).await
    }

    /// Returns one intervention without waiting.
    pub async fn get(&self, id: &str) -> Result<Intervention, Error> {
        require(id, "intervention id")?;
        let request = self.t.request(Method::GET, &["v1", "interventions", id]);
        self.t.send_json(request).await
    }

    /// Waits until the intervention settles: resolved, and for an approved
    /// gated action, performed, with the outcome on
    /// [`Intervention::result`]. Every intervention has a deadline, so this
    /// always returns eventually; drop the future to stop early. Each poll is independent, so a dropped connection or an
    /// orchestrator restart only costs one poll.
    pub async fn wait(&self, id: &str) -> Result<Intervention, Error> {
        wait_intervention(&self.t, id).await
    }

    /// Resolves a pending intervention as approved. An ask needs an
    /// `answer` (one of its choices, when it has any); an empty `reason` is
    /// omitted. Requires the master token or a key with `hitl:decide`.
    pub async fn approve(
        &self,
        id: &str,
        answer: &str,
        reason: &str,
    ) -> Result<Intervention, Error> {
        let options = ApproveOptions {
            answer: non_empty(answer),
            reason: non_empty(reason),
            ..ApproveOptions::default()
        };
        self.decide(id, "approve", &options).await
    }

    /// Approves a held exec or lifecycle action, optionally editing the
    /// exec's argv or granting repeats (`env`, `ttl:30m`). The orchestrator
    /// then performs the action; [`Interventions::wait`] on the id for its
    /// result.
    pub async fn approve_with(
        &self,
        id: &str,
        options: ApproveOptions,
    ) -> Result<Intervention, Error> {
        self.decide(id, "approve", &options).await
    }

    /// Resolves a pending intervention as rejected. The reason is fed back
    /// to the agent; the server fills one in when it is empty. Requires the
    /// master token or a key with `hitl:decide`.
    pub async fn reject(&self, id: &str, reason: &str) -> Result<Intervention, Error> {
        let options = ApproveOptions {
            reason: non_empty(reason),
            ..ApproveOptions::default()
        };
        self.decide(id, "reject", &options).await
    }

    async fn decide(
        &self,
        id: &str,
        action: &str,
        body: &ApproveOptions,
    ) -> Result<Intervention, Error> {
        require(id, "intervention id")?;
        let request = self
            .t
            .request(Method::POST, &["v1", "interventions", id])
            .query(&[("action", action)])
            .json(body);
        self.t.send_json(request).await
    }
}

// wait_intervention long-polls until the intervention is settled, not just
// terminal: an approved gated action keeps polling until its result lands. each poll
// runs on the no-timeout stream client with its own deadline, so a short
// client timeout cannot cut a held poll short.
pub(crate) async fn wait_intervention(t: &Transport, id: &str) -> Result<Intervention, Error> {
    require(id, "intervention id")?;
    let wait = format!("{}s", WAIT_POLL.as_secs());
    loop {
        let request = t
            .stream_request(Method::GET, &["v1", "interventions", id])
            .query(&[("wait", wait.as_str())])
            .timeout(WAIT_POLL + WAIT_HTTP_OVERHEAD);
        let intervention: Intervention = t.send_json(request).await?;
        if intervention.is_settled() {
            return Ok(intervention);
        }
    }
}

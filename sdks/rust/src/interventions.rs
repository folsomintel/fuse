use std::sync::Arc;
use std::time::Duration;

use reqwest::Method;
use serde::{Deserialize, Serialize};

use crate::error::Error;
use crate::transport::{pagination, require, Transport, MAX_PAGE_LIMIT};
use crate::types::{HitlKind, Intervention, InterventionState};

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

/// Filters and pagination for [`Interventions::list`] and
/// [`Interventions::list_page`].
#[derive(Debug, Clone, Default)]
#[non_exhaustive]
pub struct ListInterventionsOptions {
    pub env_id: Option<String>,
    pub state: Option<InterventionState>,
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
        pagination(&mut query, options.limit, &options.cursor);
        let request = self
            .t
            .request(Method::GET, &["v1", "interventions"])
            .query(&query);
        self.t.send_json(request).await
    }

    /// Returns one intervention without waiting.
    pub async fn get(&self, id: &str) -> Result<Intervention, Error> {
        require(id, "intervention id")?;
        let request = self.t.request(Method::GET, &["v1", "interventions", id]);
        self.t.send_json(request).await
    }

    /// Waits until the intervention resolves. Every intervention has a
    /// deadline, so this always returns eventually; drop the future to stop
    /// early. Each poll is independent, so a dropped connection or an
    /// orchestrator restart only costs one poll.
    pub async fn wait(&self, id: &str) -> Result<Intervention, Error> {
        wait_intervention(&self.t, id).await
    }

    /// Resolves a pending intervention as approved. An ask needs an
    /// `answer` (one of its choices, when it has any); an empty `reason` is
    /// omitted. Requires the master token.
    pub async fn approve(
        &self,
        id: &str,
        answer: &str,
        reason: &str,
    ) -> Result<Intervention, Error> {
        self.decide(id, "approve", answer, reason).await
    }

    /// Resolves a pending intervention as rejected. The reason is fed back
    /// to the agent; the server fills one in when it is empty. Requires the
    /// master token.
    pub async fn reject(&self, id: &str, reason: &str) -> Result<Intervention, Error> {
        self.decide(id, "reject", "", reason).await
    }

    async fn decide(
        &self,
        id: &str,
        action: &str,
        answer: &str,
        reason: &str,
    ) -> Result<Intervention, Error> {
        require(id, "intervention id")?;
        #[derive(Serialize)]
        struct Body<'a> {
            #[serde(skip_serializing_if = "str::is_empty")]
            answer: &'a str,
            #[serde(skip_serializing_if = "str::is_empty")]
            reason: &'a str,
        }
        let request = self
            .t
            .request(Method::POST, &["v1", "interventions", id])
            .query(&[("action", action)])
            .json(&Body { answer, reason });
        self.t.send_json(request).await
    }
}

// wait_intervention long-polls until the intervention is terminal. each poll
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
        if intervention.is_terminal() {
            return Ok(intervention);
        }
    }
}

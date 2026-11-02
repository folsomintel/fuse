use std::sync::Arc;
use std::time::Duration;

use reqwest::Method;
use serde::{Deserialize, Serialize};

use crate::error::Error;
use crate::transport::{require, Transport};
use crate::types::{ActorLabel, ApiKey, CreatedApiKey, HitlScope};

/// The body of [`ApiKeys::create_with_options`]. Without scopes it mints a
/// full-access key, like [`ApiKeys::create`]. With scopes the key is limited
/// to the hitl routes and must expire.
///
/// ```
/// use std::time::Duration;
/// use fuse::{ActorLabel, CreateApiKeyOptions, HitlScope};
///
/// let options = CreateApiKeyOptions::new()
///     .label("reviewer")
///     .scopes([HitlScope::Read, HitlScope::Decide])
///     .env_id("vm-1")
///     .ttl(Duration::from_secs(3600))
///     .actor(ActorLabel::new("alice").name("Alice"));
/// ```
#[derive(Debug, Clone, Default)]
#[non_exhaustive]
pub struct CreateApiKeyOptions {
    pub label: Option<String>,
    /// `None` mints a full-access key.
    pub scopes: Option<Vec<HitlScope>>,
    /// Limits the key to one environment and its forks.
    pub env_id: Option<String>,
    /// How long the key lives, sent as whole seconds. Required with scopes.
    pub ttl: Option<Duration>,
    /// Pins the key to an actor.
    pub actor: Option<ActorLabel>,
}

impl CreateApiKeyOptions {
    pub fn new() -> Self {
        Self::default()
    }

    pub fn label(mut self, label: impl Into<String>) -> Self {
        self.label = Some(label.into());
        self
    }

    pub fn scopes(mut self, scopes: impl IntoIterator<Item = HitlScope>) -> Self {
        self.scopes = Some(scopes.into_iter().collect());
        self
    }

    pub fn env_id(mut self, env_id: impl Into<String>) -> Self {
        self.env_id = Some(env_id.into());
        self
    }

    pub fn ttl(mut self, ttl: Duration) -> Self {
        self.ttl = Some(ttl);
        self
    }

    pub fn actor(mut self, actor: ActorLabel) -> Self {
        self.actor = Some(actor);
        self
    }
}

/// The API keys service, from [`Client::api_keys`](crate::Client::api_keys).
///
/// Every method requires the master token, except that a scoped key may
/// mint keys within its own access; the server enforces this.
#[derive(Clone)]
pub struct ApiKeys {
    t: Arc<Transport>,
}

impl ApiKeys {
    pub(crate) fn new(t: Arc<Transport>) -> Self {
        Self { t }
    }

    /// Issues a new API key. The raw secret is returned once in
    /// [`CreatedApiKey::key`] and is unrecoverable afterward.
    pub async fn create(&self, label: impl Into<String>) -> Result<CreatedApiKey, Error> {
        self.create_with_options(CreateApiKeyOptions::new().label(label))
            .await
    }

    /// Issues a new API key with scopes, an environment, an expiry or a
    /// pinned actor. The raw secret is returned once in
    /// [`CreatedApiKey::key`].
    pub async fn create_with_options(
        &self,
        options: CreateApiKeyOptions,
    ) -> Result<CreatedApiKey, Error> {
        #[derive(Serialize)]
        struct Body {
            #[serde(skip_serializing_if = "Option::is_none")]
            label: Option<String>,
            #[serde(skip_serializing_if = "Option::is_none")]
            scopes: Option<Vec<HitlScope>>,
            #[serde(skip_serializing_if = "Option::is_none")]
            env_id: Option<String>,
            #[serde(skip_serializing_if = "Option::is_none")]
            ttl_seconds: Option<u64>,
            #[serde(skip_serializing_if = "Option::is_none")]
            actor: Option<ActorLabel>,
        }
        let body = Body {
            label: options.label.filter(|label| !label.is_empty()),
            scopes: options.scopes,
            env_id: options.env_id.filter(|env_id| !env_id.is_empty()),
            ttl_seconds: options
                .ttl
                .map(|ttl| ttl.as_secs())
                .filter(|secs| *secs > 0),
            actor: options.actor,
        };
        let request = self
            .t
            .request(Method::POST, &["v1", "api-keys"])
            .json(&body);
        self.t.send_json(request).await
    }

    /// Returns the metadata for all API keys.
    pub async fn list(&self) -> Result<Vec<ApiKey>, Error> {
        #[derive(Deserialize)]
        struct Wire {
            #[serde(default)]
            api_keys: Vec<ApiKey>,
        }
        let request = self.t.request(Method::GET, &["v1", "api-keys"]);
        let wire: Wire = self.t.send_json(request).await?;
        Ok(wire.api_keys)
    }

    /// Deletes the API key with the given id.
    pub async fn revoke(&self, id: &str) -> Result<(), Error> {
        require(id, "id")?;
        let request = self.t.request(Method::DELETE, &["v1", "api-keys", id]);
        self.t.send_unit(request).await
    }
}

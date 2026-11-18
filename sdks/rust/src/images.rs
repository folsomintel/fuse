use std::sync::Arc;

use reqwest::Method;

use crate::error::Error;
use crate::operations::CallOptions;
use crate::transport::{require, Transport};
use crate::types::{ConvertImageRequest, Image};

/// The images service, from [`Client::images`](crate::Client::images).
///
/// Converts oci images (a Fusefile's `from:`) into bootable rootfs images.
#[derive(Clone)]
pub struct Images {
    t: Arc<Transport>,
}

impl Images {
    pub(crate) fn new(t: Arc<Transport>) -> Self {
        Self { t }
    }

    /// Starts converting `request.ref` and returns the image at once,
    /// usually still converting. Poll [`get`](Self::get) until it is ready.
    /// Converting a ref that is already converting or ready returns that
    /// image; one that failed is retried.
    pub async fn convert(&self, request: ConvertImageRequest) -> Result<Image, Error> {
        self.convert_with_options(request, CallOptions::new()).await
    }

    /// Converts with per-call options such as an idempotency key, so a
    /// retry attaches to the first convert instead of starting another.
    pub async fn convert_with_options(
        &self,
        request: ConvertImageRequest,
        options: CallOptions,
    ) -> Result<Image, Error> {
        require(&request.r#ref, "image ref")?;
        let builder = self
            .t
            .request(Method::POST, &["v1", "images"])
            .json(&request);
        self.t.send_json(options.apply(builder)).await
    }

    /// Reports an image's conversion state by digest.
    pub async fn get(&self, digest: &str) -> Result<Image, Error> {
        require(digest, "image digest")?;
        self.t
            .send_json(self.t.request(Method::GET, &["v1", "images", digest]))
            .await
    }
}

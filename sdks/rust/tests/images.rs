// images tests against a mock orchestrator: convert's method, path and
// body, get's path escaping and failed-image decoding, a 404 on get, and
// the spec's image_digest field.

use fuse::{Client, ConvertImageRequest, ImageState, Spec};
use serde_json::{json, Value};
use wiremock::matchers::{method, path};
use wiremock::{Mock, MockServer, ResponseTemplate};

const DIGEST: &str = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef";

#[tokio::test]
async fn convert_sends_the_body_and_decodes_the_image() {
    let server = MockServer::start().await;
    Mock::given(method("POST"))
        .and(path("/v1/images"))
        .respond_with(ResponseTemplate::new(202).set_body_json(json!({
            "digest": DIGEST,
            "ref": "docker.io/library/python:3.12-slim",
            "manifest_digest": "sha256:abc",
            "host_id": "host-1",
            "state": "converting",
        })))
        .expect(1)
        .mount(&server)
        .await;
    let client = Client::new(server.uri(), "secret").unwrap();

    let image = client
        .images()
        .convert(
            ConvertImageRequest::new("docker.io/library/python:3.12-slim")
                .host_id("host-1")
                .label("zone", "a"),
        )
        .await
        .unwrap();
    assert_eq!(image.digest, DIGEST);
    assert_eq!(image.r#ref, "docker.io/library/python:3.12-slim");
    assert_eq!(image.manifest_digest, "sha256:abc");
    assert_eq!(image.host_id, "host-1");
    assert_eq!(image.state, ImageState::Converting);
    assert_eq!(image.error, None);

    let requests = server.received_requests().await.unwrap();
    let body: Value = serde_json::from_slice(&requests[0].body).unwrap();
    assert_eq!(
        body,
        json!({
            "ref": "docker.io/library/python:3.12-slim",
            "host_id": "host-1",
            "labels": {"zone": "a"},
        })
    );
}

#[tokio::test]
async fn convert_without_a_ref_is_rejected() {
    let client = Client::new("http://127.0.0.1:1", "secret").unwrap();
    let err = client
        .images()
        .convert(ConvertImageRequest::default())
        .await
        .unwrap_err();
    assert!(matches!(err, fuse::Error::InvalidRequest(_)), "{err:?}");
}

#[tokio::test]
async fn get_escapes_the_digest_and_decodes_a_failed_image() {
    let server = MockServer::start().await;
    Mock::given(method("GET"))
        .and(path(format!("/v1/images/{DIGEST}")))
        .respond_with(ResponseTemplate::new(200).set_body_json(json!({
            "digest": DIGEST,
            "ref": "docker.io/library/nope:1",
            "manifest_digest": "",
            "host_id": "host-1",
            "state": "failed",
            "error": "pull: manifest unknown",
        })))
        .mount(&server)
        .await;
    let client = Client::new(server.uri(), "secret").unwrap();

    let image = client.images().get(DIGEST).await.unwrap();
    assert_eq!(image.state, ImageState::Failed);
    assert_eq!(image.error.as_deref(), Some("pull: manifest unknown"));

    // a digest with path structure stays one escaped segment.
    let _ = client.images().get("../hosts").await;
    let requests = server.received_requests().await.unwrap();
    assert_eq!(requests[1].url.path(), "/v1/images/..%2Fhosts");
}

#[tokio::test]
async fn get_unknown_image_is_not_found() {
    let server = MockServer::start().await;
    Mock::given(method("GET"))
        .and(path(format!("/v1/images/{DIGEST}")))
        .respond_with(
            ResponseTemplate::new(404).set_body_json(
                json!({"error": {"code": "not_found", "message": "image not found"}}),
            ),
        )
        .mount(&server)
        .await;
    let client = Client::new(server.uri(), "secret").unwrap();
    let err = client.images().get(DIGEST).await.unwrap_err();
    assert!(err.is_not_found(), "{err:?}");
}

#[test]
fn unknown_image_state_passes_through() {
    assert_eq!(
        ImageState::from("pending"),
        ImageState::Other("pending".into())
    );
}

#[test]
fn spec_serializes_image_digest() {
    let spec = Spec::new().image_digest(DIGEST);
    assert_eq!(
        serde_json::to_value(&spec).unwrap(),
        json!({"image_digest": DIGEST})
    );
    assert_eq!(serde_json::to_value(Spec::new()).unwrap(), json!({}));
}

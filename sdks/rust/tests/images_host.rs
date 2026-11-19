// images on one host against a mock orchestrator: get_on_host's path, the
// host's record decoded, a 404 for an unknown host and an empty host id
// refused before any request.

use fuse::{Client, ImageState};
use serde_json::json;
use wiremock::matchers::{method, path};
use wiremock::{Mock, MockServer, ResponseTemplate};

const DIGEST: &str = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef";

#[tokio::test]
async fn get_on_host_reads_that_hosts_record() {
    let server = MockServer::start().await;
    Mock::given(method("GET"))
        .and(path(format!("/v1/hosts/fc-2/images/{DIGEST}")))
        .respond_with(ResponseTemplate::new(200).set_body_json(json!({
            "digest": DIGEST,
            "ref": "ubuntu",
            "manifest_digest": "sha256:abc",
            "host_id": "fc-2",
            "state": "converting",
        })))
        .expect(1)
        .mount(&server)
        .await;
    Mock::given(method("GET"))
        .and(path(format!("/v1/hosts/gone/images/{DIGEST}")))
        .respond_with(ResponseTemplate::new(404).set_body_json(
            json!({"error": {"code": "not_found", "message": "host \"gone\" is not registered"}}),
        ))
        .mount(&server)
        .await;
    let client = Client::new(server.uri(), "secret").unwrap();

    let image = client.images().get_on_host("fc-2", DIGEST).await.unwrap();
    assert_eq!(image.host_id, "fc-2");
    assert_eq!(image.state, ImageState::Converting);

    let err = client
        .images()
        .get_on_host("gone", DIGEST)
        .await
        .unwrap_err();
    assert!(err.is_not_found(), "{err:?}");

    let err = client.images().get_on_host("", DIGEST).await.unwrap_err();
    assert!(matches!(err, fuse::Error::InvalidRequest(_)), "{err:?}");
}

// 503 retry against a mock orchestrator: a keyed call answered 503 is sent
// again under the same idempotency key, and only up to the client's retries.

use fuse::{Client, CreateRequest, Error, ErrorCode};
use serde_json::{json, Value};
use wiremock::matchers::{method, path};
use wiremock::{Mock, MockServer, ResponseTemplate};

fn env_body(id: &str) -> Value {
    json!({
        "id": id,
        "state": "running",
        "task_id": "t-1",
        "url": format!("http://10.0.0.5/{id}"),
        "spec": {"cpus": 1},
        "created_at": "2026-01-02T03:04:05Z",
        "updated_at": "2026-01-02T03:04:06Z",
    })
}

// Retry-After 0 keeps the test fast while still going through the header.
fn unavailable() -> ResponseTemplate {
    ResponseTemplate::new(503)
        .insert_header("retry-after", "0")
        .set_body_json(
            json!({"error": {"code": "unavailable", "message": "no host has sufficient capacity"}}),
        )
}

fn assert_unavailable(err: Error) {
    match err {
        Error::Api(e) => {
            assert_eq!(e.status, 503);
            assert_eq!(e.code, Some(ErrorCode::Unavailable));
        }
        other => panic!("err = {other:?}, want a 503"),
    }
}

#[tokio::test]
async fn create_retries_unavailable_under_the_same_key() {
    let server = MockServer::start().await;
    Mock::given(method("POST"))
        .and(path("/v1/environments"))
        .respond_with(unavailable())
        .up_to_n_times(2)
        .mount(&server)
        .await;
    Mock::given(method("POST"))
        .and(path("/v1/environments"))
        .respond_with(ResponseTemplate::new(201).set_body_json(env_body("vm-1")))
        .mount(&server)
        .await;
    let client = Client::new(server.uri(), "secret").unwrap();

    let env = client
        .environments()
        .create(CreateRequest::new("t-1"))
        .await
        .unwrap();

    assert_eq!(env.id, "vm-1");
    let requests = server.received_requests().await.unwrap();
    assert_eq!(requests.len(), 3);
    let key = requests[0].headers.get("idempotency-key").unwrap();
    for r in &requests {
        assert_eq!(r.headers.get("idempotency-key").unwrap(), key);
        assert_eq!(r.body, requests[0].body);
    }
}

#[tokio::test]
async fn create_returns_unavailable_once_retries_run_out() {
    let server = MockServer::start().await;
    Mock::given(method("POST"))
        .and(path("/v1/environments"))
        .respond_with(unavailable())
        .expect(4)
        .mount(&server)
        .await;
    let client = Client::new(server.uri(), "secret").unwrap();

    let err = client
        .environments()
        .create(CreateRequest::new("t-1"))
        .await
        .unwrap_err();
    assert_unavailable(err);
}

#[tokio::test]
async fn retries_zero_returns_the_first_503() {
    let server = MockServer::start().await;
    Mock::given(method("POST"))
        .and(path("/v1/environments"))
        .respond_with(unavailable())
        .expect(1)
        .mount(&server)
        .await;
    let client = Client::builder(server.uri()).retries(0).build().unwrap();

    let err = client
        .environments()
        .create(CreateRequest::new("t-1"))
        .await
        .unwrap_err();
    assert_unavailable(err);
}

#[tokio::test]
async fn unkeyed_call_is_not_retried() {
    let server = MockServer::start().await;
    Mock::given(method("GET"))
        .and(path("/v1/environments/vm-1"))
        .respond_with(unavailable())
        .expect(1)
        .mount(&server)
        .await;
    let client = Client::new(server.uri(), "secret").unwrap();

    let err = client.environments().get("vm-1").await.unwrap_err();
    assert_unavailable(err);
}

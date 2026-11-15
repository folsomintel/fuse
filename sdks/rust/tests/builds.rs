// builds tests against a mock orchestrator: create's body and idempotency
// key, get decoding, wait polling and its timeout, and the build event.

use std::time::{Duration, Instant};

use fuse::{
    BuildLayerStatus, BuildSetup, BuildState, BuildStep, CallOptions, Client, CreateBuildRequest,
    Event, EventKind,
};
use serde_json::{json, Value};
use wiremock::matchers::{method, path, query_param};
use wiremock::{Mock, MockServer, ResponseTemplate};

fn build_body(id: &str, state: &str) -> Value {
    json!({
        "id": id,
        "name": "web",
        "state": state,
        "step": "layer_0",
        "builder_id": "vm-b",
        "arch": "x86_64",
        "cache_hits": 1,
        "layers": [
            {"index": 0, "key": "k0", "status": "hit", "snapshot_id": "snap-0"},
            {"index": 1, "status": "skip"},
        ],
        "created_at": "2026-01-02T03:04:05Z",
        "updated_at": "2026-01-02T03:04:06Z",
    })
}

fn request() -> CreateBuildRequest {
    CreateBuildRequest::new(
        "web",
        BuildSetup {
            prelude: "set -e".into(),
            files: None,
            steps: vec![BuildStep::new("apt-get update").key("k0")],
        },
    )
    .keep(true)
}

#[tokio::test]
async fn create_sends_the_body_and_idempotency_keys() {
    let server = MockServer::start().await;
    Mock::given(method("POST"))
        .and(path("/v1/builds"))
        .respond_with(
            ResponseTemplate::new(202)
                .insert_header("location", "/v1/builds/b-1")
                .set_body_json(build_body("b-1", "running")),
        )
        .mount(&server)
        .await;
    let client = Client::new(server.uri(), "secret").unwrap();

    let build = client
        .builds()
        .create(request(), CallOptions::new())
        .await
        .unwrap();
    assert_eq!(build.id, "b-1");
    client
        .builds()
        .create(request(), CallOptions::new().idempotency_key("build-42"))
        .await
        .unwrap();

    let requests = server.received_requests().await.unwrap();
    let keys: Vec<String> = requests
        .iter()
        .map(|r| {
            r.headers
                .get("idempotency-key")
                .unwrap()
                .to_str()
                .unwrap()
                .to_owned()
        })
        .collect();
    assert_eq!(keys[0].len(), 36);
    assert_eq!(keys[1], "build-42");

    let body: Value = serde_json::from_slice(&requests[0].body).unwrap();
    assert_eq!(
        body,
        json!({
            "name": "web",
            "keep": true,
            "setup": {"prelude": "set -e", "steps": [{"script": "apt-get update", "key": "k0"}]},
        })
    );
}

#[tokio::test]
async fn get_decodes_a_build_with_layers() {
    let server = MockServer::start().await;
    Mock::given(method("GET"))
        .and(path("/v1/builds/b-1"))
        .and(query_param("wait", "5000ms"))
        .respond_with(ResponseTemplate::new(200).set_body_json(build_body("b-1", "running")))
        .mount(&server)
        .await;
    let client = Client::new(server.uri(), "secret").unwrap();

    let build = client
        .builds()
        .get("b-1", Some(Duration::from_secs(5)))
        .await
        .unwrap();
    assert_eq!(build.state, BuildState::Running);
    assert_eq!(build.cache_hits, 1);
    assert_eq!(build.layers.len(), 2);
    assert_eq!(build.layers[0].status, BuildLayerStatus::Hit);
    assert_eq!(build.layers[0].snapshot_id.as_deref(), Some("snap-0"));
    assert_eq!(build.layers[1].status, BuildLayerStatus::Skip);
    assert_eq!(build.layers[1].key, None);
}

#[tokio::test]
async fn get_unknown_build_is_not_found() {
    let server = MockServer::start().await;
    Mock::given(method("GET"))
        .and(path("/v1/builds/nope"))
        .respond_with(
            ResponseTemplate::new(404).set_body_json(
                json!({"error": {"code": "not_found", "message": "build not found"}}),
            ),
        )
        .mount(&server)
        .await;
    let client = Client::new(server.uri(), "secret").unwrap();
    let err = client.builds().get("nope", None).await.unwrap_err();
    assert!(err.is_not_found(), "{err:?}");
}

#[tokio::test]
async fn wait_polls_until_finished_and_returns_a_failed_build() {
    let server = MockServer::start().await;
    Mock::given(method("GET"))
        .and(path("/v1/builds/b-1"))
        .and(query_param("wait", "30000ms"))
        .respond_with(ResponseTemplate::new(200).set_body_json(build_body("b-1", "running")))
        .up_to_n_times(1)
        .expect(1)
        .mount(&server)
        .await;
    let mut failed = build_body("b-1", "failed");
    failed["error"] = json!("step 0 exited 1");
    failed["output"] = json!("E: no such package");
    Mock::given(method("GET"))
        .and(path("/v1/builds/b-1"))
        .respond_with(ResponseTemplate::new(200).set_body_json(failed))
        .expect(1)
        .mount(&server)
        .await;
    let client = Client::new(server.uri(), "secret").unwrap();

    let build = client.builds().wait("b-1", None).await.unwrap();
    assert_eq!(build.state, BuildState::Failed);
    assert_eq!(build.output.as_deref(), Some("E: no such package"));
}

#[tokio::test]
async fn wait_respects_its_timeout() {
    let server = MockServer::start().await;
    // the server holds the poll longer than the timeout.
    Mock::given(method("GET"))
        .and(path("/v1/builds/b-1"))
        .and(query_param("wait", "300ms"))
        .respond_with(
            ResponseTemplate::new(200)
                .set_body_json(build_body("b-1", "running"))
                .set_delay(Duration::from_secs(5)),
        )
        .mount(&server)
        .await;
    Mock::given(method("GET"))
        .and(path("/v1/builds/b-1"))
        .respond_with(ResponseTemplate::new(200).set_body_json(build_body("b-1", "running")))
        .mount(&server)
        .await;
    let client = Client::new(server.uri(), "secret").unwrap();

    let start = Instant::now();
    let build = client
        .builds()
        .wait("b-1", Some(Duration::from_millis(300)))
        .await
        .unwrap();
    assert_eq!(build.state, BuildState::Running);
    assert!(
        start.elapsed() < Duration::from_secs(3),
        "{:?}",
        start.elapsed()
    );
}

#[test]
fn event_decodes_the_build_member() {
    let event: Event = serde_json::from_value(json!({
        "id": "7",
        "event": "build",
        "vm_id": "vm-b",
        "build": {"id": "b-1", "state": "running", "step": "snapshot"},
    }))
    .unwrap();
    assert_eq!(event.kind, EventKind::Build);
    let build = event.build.unwrap();
    assert_eq!(build.id, "b-1");
    assert_eq!(build.state, BuildState::Running);
    assert_eq!(build.step.as_deref(), Some("snapshot"));
}

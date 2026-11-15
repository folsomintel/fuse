// operation tests against a mock orchestrator: the idempotency key on
// mutating calls, transparently following a 202 operation, and the
// operations service.

use std::sync::{Arc, Mutex};
use std::time::Duration;

use fuse::{CallOptions, Client, CreateRequest, OperationKind, OperationState};
use serde_json::{json, Value};
use wiremock::matchers::{header, method, path, query_param};
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

fn operation(id: &str, kind: &str, state: &str) -> Value {
    json!({
        "id": id,
        "kind": kind,
        "state": state,
        "step": "boot",
        "started_at": "2026-01-02T03:04:05Z",
    })
}

fn accepted(id: &str, kind: &str) -> ResponseTemplate {
    ResponseTemplate::new(202)
        .insert_header("location", format!("/v1/operations/{id}").as_str())
        .insert_header("fuse-operation-id", id)
        .set_body_json(json!({
            "operation_id": id,
            "operation": operation(id, kind, "running"),
        }))
}

fn finished(id: &str, kind: &str, state: &str, status: u16, extra: Value) -> Value {
    let mut body = operation(id, kind, state);
    body["finished_at"] = json!("2026-01-02T03:05:05Z");
    body["status_code"] = json!(status);
    for (key, value) in extra.as_object().unwrap() {
        body[key] = value.clone();
    }
    body
}

fn idempotency_keys(requests: &[wiremock::Request]) -> Vec<String> {
    requests
        .iter()
        .filter_map(|r| r.headers.get("idempotency-key"))
        .map(|v| v.to_str().unwrap().to_owned())
        .collect()
}

#[tokio::test]
async fn create_sends_auto_and_explicit_idempotency_keys() {
    let server = MockServer::start().await;
    Mock::given(method("POST"))
        .and(path("/v1/environments"))
        .respond_with(ResponseTemplate::new(201).set_body_json(env_body("vm-1")))
        .mount(&server)
        .await;
    let client = Client::new(server.uri(), "secret").unwrap();

    client
        .environments()
        .create(CreateRequest::new("t-1"))
        .await
        .unwrap();
    client
        .environments()
        .create(CreateRequest::new("t-1"))
        .await
        .unwrap();
    client
        .environments()
        .create_with_options(
            CreateRequest::new("t-1"),
            CallOptions::new().idempotency_key("deploy-42"),
        )
        .await
        .unwrap();

    let keys = idempotency_keys(&server.received_requests().await.unwrap());
    assert_eq!(keys.len(), 3);
    // the auto keys are uuids, and each call gets its own.
    assert_eq!(keys[0].len(), 36);
    assert_ne!(keys[0], keys[1]);
    assert_eq!(keys[2], "deploy-42");
}

#[tokio::test]
async fn create_follows_a_202_operation_to_its_environment() {
    let server = MockServer::start().await;
    Mock::given(method("POST"))
        .and(path("/v1/environments"))
        .and(header("idempotency-key", "k-1"))
        .respond_with(accepted("op_1", "create"))
        .expect(1)
        .mount(&server)
        .await;
    // the first poll is still running, the second has the answer.
    Mock::given(method("GET"))
        .and(path("/v1/operations/op_1"))
        .and(query_param("wait", "30000ms"))
        .respond_with(
            ResponseTemplate::new(200).set_body_json(operation("op_1", "create", "running")),
        )
        .up_to_n_times(1)
        .expect(1)
        .mount(&server)
        .await;
    Mock::given(method("GET"))
        .and(path("/v1/operations/op_1"))
        .respond_with(ResponseTemplate::new(200).set_body_json(finished(
            "op_1",
            "create",
            "succeeded",
            201,
            json!({"result": env_body("vm-9")}),
        )))
        .expect(1)
        .mount(&server)
        .await;

    let seen = Arc::new(Mutex::new(Vec::new()));
    let seen_in = seen.clone();
    let client = Client::builder(server.uri())
        .token("secret")
        .on_operation(move |accepted| seen_in.lock().unwrap().push(accepted.operation_id.clone()))
        .build()
        .unwrap();

    let env = client
        .environments()
        .create_with_options(
            CreateRequest::new("t-1"),
            CallOptions::new().idempotency_key("k-1"),
        )
        .await
        .unwrap();
    assert_eq!(env.id, "vm-9");
    assert_eq!(*seen.lock().unwrap(), vec!["op_1".to_owned()]);
}

#[tokio::test]
async fn followed_operation_that_failed_returns_the_normal_error() {
    let server = MockServer::start().await;
    Mock::given(method("POST"))
        .and(path("/v1/environments/vm-1"))
        .and(query_param("action", "fork"))
        .respond_with(accepted("op_2", "fork"))
        .mount(&server)
        .await;
    Mock::given(method("GET"))
        .and(path("/v1/operations/op_2"))
        .respond_with(ResponseTemplate::new(200).set_body_json(finished(
            "op_2",
            "fork",
            "failed",
            404,
            json!({"error": {"code": "not_found", "message": "vm not found"}}),
        )))
        .mount(&server)
        .await;
    let client = Client::new(server.uri(), "secret").unwrap();

    let err = client
        .environments()
        .fork("vm-1", Default::default())
        .await
        .unwrap_err();
    assert!(err.is_not_found(), "{err:?}");
    assert_eq!(err.status(), Some(404));
    assert_eq!(err.api().unwrap().message, "vm not found");
}

#[tokio::test]
async fn followed_destroy_with_no_result_returns_unit() {
    let server = MockServer::start().await;
    Mock::given(method("DELETE"))
        .and(path("/v1/environments/vm-1"))
        .respond_with(accepted("op_3", "destroy"))
        .mount(&server)
        .await;
    Mock::given(method("GET"))
        .and(path("/v1/operations/op_3"))
        .respond_with(ResponseTemplate::new(200).set_body_json(finished(
            "op_3",
            "destroy",
            "succeeded",
            204,
            json!({}),
        )))
        .mount(&server)
        .await;
    let client = Client::new(server.uri(), "secret").unwrap();

    client.environments().destroy("vm-1").await.unwrap();
    let keys = idempotency_keys(&server.received_requests().await.unwrap());
    assert_eq!(keys.len(), 1);
}

#[tokio::test]
async fn operations_get_and_wait() {
    let server = MockServer::start().await;
    Mock::given(method("GET"))
        .and(path("/v1/operations/op_4"))
        .and(query_param("wait", "5000ms"))
        .respond_with(ResponseTemplate::new(200).set_body_json(finished(
            "op_4",
            "migrate",
            "succeeded",
            200,
            json!({"result": env_body("vm-5")}),
        )))
        .mount(&server)
        .await;
    Mock::given(method("GET"))
        .and(path("/v1/operations/op_4"))
        .respond_with(
            ResponseTemplate::new(200).set_body_json(operation("op_4", "migrate", "running")),
        )
        .up_to_n_times(1)
        .mount(&server)
        .await;
    Mock::given(method("GET"))
        .and(path("/v1/operations/op_4"))
        .and(query_param("wait", "30000ms"))
        .respond_with(ResponseTemplate::new(200).set_body_json(finished(
            "op_4",
            "migrate",
            "succeeded",
            200,
            json!({"result": env_body("vm-5")}),
        )))
        .mount(&server)
        .await;
    Mock::given(method("GET"))
        .and(path("/v1/operations/op_missing"))
        .respond_with(ResponseTemplate::new(404).set_body_json(json!({
            "error": {"code": "not_found", "message": "operation not found"},
        })))
        .mount(&server)
        .await;
    let client = Client::new(server.uri(), "secret").unwrap();
    let operations = client.operations();

    // no wait answers at once with the running operation.
    let running = operations.get("op_4", None).await.unwrap();
    assert_eq!(running.state, OperationState::Running);
    assert_eq!(running.kind, OperationKind::Migrate);
    assert_eq!(running.step.as_deref(), Some("boot"));
    assert!(!running.is_terminal());

    let got = operations
        .get("op_4", Some(Duration::from_secs(5)))
        .await
        .unwrap();
    assert_eq!(got.state, OperationState::Succeeded);
    assert_eq!(got.status_code, Some(200));
    assert_eq!(got.result.unwrap()["id"], "vm-5");

    let waited = operations.wait("op_4", None).await.unwrap();
    assert!(waited.is_terminal());
    assert!(waited.finished_at.is_some());

    let err = operations.get("op_missing", None).await.unwrap_err();
    assert!(err.is_not_found());
}

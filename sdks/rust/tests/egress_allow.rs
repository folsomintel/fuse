// egress allowlist tests against a mock orchestrator: the put body (with an
// empty list kept), the decoded egress status, a followed 202 operation, a
// 409 for a proxy environment, and allow on the create body.

use fuse::{Client, CreateRequest, EgressMode, EgressSpec};
use serde_json::{json, Value};
use wiremock::matchers::{method, path};
use wiremock::{Mock, MockServer, ResponseTemplate};

const ALLOW_PATH: &str = "/v1/environments/vm-1/egress/allow";

#[tokio::test]
async fn set_egress_allow_sends_the_list_and_decodes_the_status() {
    let server = MockServer::start().await;
    Mock::given(method("PUT"))
        .and(path(ALLOW_PATH))
        .respond_with(ResponseTemplate::new(200).set_body_json(json!({
            "mode": "direct",
            "allow": ["10.0.0.0/8", "1.2.3.4/32"],
        })))
        .up_to_n_times(1)
        .mount(&server)
        .await;
    Mock::given(method("PUT"))
        .and(path(ALLOW_PATH))
        .respond_with(ResponseTemplate::new(200).set_body_json(json!({"mode": "direct"})))
        .mount(&server)
        .await;
    let client = Client::new(server.uri(), "secret").unwrap();

    let status = client
        .environments()
        .set_egress_allow("vm-1", ["10.0.0.0/8", "1.2.3.4"])
        .await
        .unwrap();
    assert_eq!(status.mode, EgressMode::Direct);
    assert_eq!(
        status.allow,
        Some(vec!["10.0.0.0/8".to_owned(), "1.2.3.4/32".to_owned()])
    );

    // an empty list lifts the restriction and must still send the key.
    let lifted = client
        .environments()
        .set_egress_allow("vm-1", Vec::<String>::new())
        .await
        .unwrap();
    assert_eq!(lifted.allow, None);

    let requests = server.received_requests().await.unwrap();
    let bodies: Vec<Value> = requests
        .iter()
        .map(|r| serde_json::from_slice(&r.body).unwrap())
        .collect();
    assert_eq!(
        bodies,
        vec![
            json!({"allow": ["10.0.0.0/8", "1.2.3.4"]}),
            json!({"allow": []}),
        ]
    );
    assert!(requests[0].headers.contains_key("idempotency-key"));
}

#[tokio::test]
async fn set_egress_allow_follows_an_accepted_operation() {
    let server = MockServer::start().await;
    let running = json!({
        "id": "op_1",
        "kind": "egress.allow",
        "state": "running",
        "started_at": "2026-01-02T03:04:05Z",
    });
    Mock::given(method("PUT"))
        .and(path(ALLOW_PATH))
        .respond_with(ResponseTemplate::new(202).set_body_json(json!({
            "operation_id": "op_1",
            "operation": running,
        })))
        .mount(&server)
        .await;
    let mut done = running.clone();
    done["state"] = json!("succeeded");
    done["status_code"] = json!(200);
    done["result"] = json!({"mode": "direct", "allow": ["10.0.0.0/8"]});
    Mock::given(method("GET"))
        .and(path("/v1/operations/op_1"))
        .respond_with(ResponseTemplate::new(200).set_body_json(done))
        .mount(&server)
        .await;
    let client = Client::new(server.uri(), "secret").unwrap();

    let status = client
        .environments()
        .set_egress_allow("vm-1", ["10.0.0.0/8"])
        .await
        .unwrap();
    assert_eq!(status.allow, Some(vec!["10.0.0.0/8".to_owned()]));
}

#[tokio::test]
async fn set_egress_allow_on_a_proxy_environment_is_a_conflict() {
    let server = MockServer::start().await;
    Mock::given(method("PUT"))
        .and(path(ALLOW_PATH))
        .respond_with(ResponseTemplate::new(409).set_body_json(json!({
            "error": {"code": "conflict", "message": "environment uses proxy egress"},
        })))
        .mount(&server)
        .await;
    let client = Client::new(server.uri(), "secret").unwrap();

    let err = client
        .environments()
        .set_egress_allow("vm-1", ["10.0.0.0/8"])
        .await
        .unwrap_err();
    assert!(err.is_conflict(), "err = {err:?}");
}

#[tokio::test]
async fn create_sends_egress_allow() {
    let server = MockServer::start().await;
    Mock::given(method("POST"))
        .and(path("/v1/environments"))
        .respond_with(ResponseTemplate::new(201).set_body_json(json!({
            "id": "vm-1",
            "state": "running",
            "task_id": "t-1",
            "url": "http://10.0.0.5",
            "spec": {},
            "created_at": "2026-01-02T03:04:05Z",
            "updated_at": "2026-01-02T03:04:06Z",
        })))
        .mount(&server)
        .await;
    let client = Client::new(server.uri(), "secret").unwrap();

    client
        .environments()
        .create(CreateRequest::new("t-1").egress(EgressSpec::direct_allow(["10.0.0.0/8"])))
        .await
        .unwrap();

    let requests = server.received_requests().await.unwrap();
    let body: Value = serde_json::from_slice(&requests[0].body).unwrap();
    assert_eq!(
        body["egress"],
        json!({"mode": "direct", "allow": ["10.0.0.0/8"]})
    );
}

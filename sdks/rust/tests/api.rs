// api tests against a mock orchestrator: request shape, auth headers, error
// envelope handling, pagination walking, and the sse event stream.

use std::time::Duration;

use fuse::{
    ActorLabel, ActorSource, Arch, AuditEvent, AuditOptions, Client, CreateApiKeyOptions,
    CreateRequest, Error, ExecRequest, ForbiddenReason, ForkOptions, HitlOnTimeout, HitlScope,
    HitlSpec, HumanRequest, InterventionOutcome, InterventionState, ListEnvironmentsOptions,
    ListInterventionsOptions, MigrateOptions, Spec, ToolResultBlock,
};
use serde_json::json;
use wiremock::matchers::{body_json, body_partial_json, header, method, path, query_param};
use wiremock::{Mock, MockServer, ResponseTemplate};

fn env_body(id: &str, state: &str) -> serde_json::Value {
    json!({
        "id": id,
        "state": state,
        "task_id": "t-1",
        "url": format!("http://10.0.0.5/{id}"),
        "spec": {"cpus": 1},
        "created_at": "2026-01-02T03:04:05Z",
        "updated_at": "2026-01-02T03:04:06Z",
    })
}

async fn client(server: &MockServer) -> Client {
    Client::new(server.uri(), "secret").unwrap()
}

#[test]
fn builder_rejects_bad_base_urls() {
    assert!(matches!(
        Client::new("", "t"),
        Err(Error::InvalidRequest(_))
    ));
    assert!(matches!(
        Client::new("not a url", "t"),
        Err(Error::InvalidRequest(_))
    ));
    assert!(matches!(
        Client::new("mailto:nope@example.com", "t"),
        Err(Error::InvalidRequest(_))
    ));
    assert!(Client::new("http://127.0.0.1:8080", "").is_ok());
}

#[tokio::test]
async fn create_sends_auth_and_body() {
    let server = MockServer::start().await;
    Mock::given(method("POST"))
        .and(path("/v1/environments"))
        .and(header("authorization", "Bearer secret"))
        .and(header(
            "user-agent",
            concat!("fuse-rust/", env!("CARGO_PKG_VERSION")),
        ))
        .and(body_partial_json(json!({
            "task_id": "t-1",
            "spec": {"cpus": 2, "ram_mb": 2048},
        })))
        .respond_with(ResponseTemplate::new(200).set_body_json(env_body("vm-1", "provisioning")))
        .expect(1)
        .mount(&server)
        .await;

    let env = client(&server)
        .await
        .environments()
        .create(CreateRequest::new("t-1").spec(Spec::new().cpus(2).ram_mb(2048)))
        .await
        .unwrap();
    assert_eq!(env.id, "vm-1");
    assert_eq!(env.state, "provisioning".into());
}

#[tokio::test]
async fn empty_token_sends_no_auth_header() {
    let server = MockServer::start().await;
    Mock::given(method("GET"))
        .and(path("/v1/environments/vm-1"))
        .respond_with(ResponseTemplate::new(200).set_body_json(env_body("vm-1", "running")))
        .mount(&server)
        .await;

    let client = Client::new(server.uri(), "").unwrap();
    client.environments().get("vm-1").await.unwrap();

    let requests = server.received_requests().await.unwrap();
    assert!(requests[0].headers.get("authorization").is_none());
}

#[tokio::test]
async fn empty_ids_are_rejected_client_side() {
    let server = MockServer::start().await;
    let client = client(&server).await;
    assert!(matches!(
        client.environments().get("").await,
        Err(Error::InvalidRequest(_))
    ));
    assert!(matches!(
        client.snapshots().delete("").await,
        Err(Error::InvalidRequest(_))
    ));
    assert!(matches!(
        client.hosts().cordon("").await,
        Err(Error::InvalidRequest(_))
    ));
    assert!(server.received_requests().await.unwrap().is_empty());
}

#[tokio::test]
async fn api_error_envelope_is_parsed() {
    let server = MockServer::start().await;
    Mock::given(method("GET"))
        .and(path("/v1/environments/vm-x"))
        .respond_with(
            ResponseTemplate::new(404)
                .insert_header("x-request-id", "req-9")
                .set_body_json(json!({
                    "error": {
                        "code": "not_found",
                        "message": "environment not found",
                        "details": {"id": "vm-x"},
                    }
                })),
        )
        .mount(&server)
        .await;

    let err = client(&server)
        .await
        .environments()
        .get("vm-x")
        .await
        .unwrap_err();
    assert!(err.is_not_found());
    assert_eq!(err.status(), Some(404));
    let api = err.api().unwrap();
    assert_eq!(api.message, "environment not found");
    assert_eq!(api.request_id.as_deref(), Some("req-9"));
    assert_eq!(api.details.get("id").map(String::as_str), Some("vm-x"));
    assert!(err.to_string().contains("status=404"));
}

#[tokio::test]
async fn non_envelope_error_falls_back_to_status_text() {
    let server = MockServer::start().await;
    Mock::given(method("GET"))
        .and(path("/v1/environments/vm-x"))
        .respond_with(ResponseTemplate::new(502).set_body_string("<html>bad gateway</html>"))
        .mount(&server)
        .await;

    let err = client(&server)
        .await
        .environments()
        .get("vm-x")
        .await
        .unwrap_err();
    let api = err.api().unwrap();
    assert_eq!(api.status, 502);
    assert_eq!(api.code, None);
    assert_eq!(api.message, "bad gateway");
    assert_eq!(api.body, b"<html>bad gateway</html>");
}

#[tokio::test]
async fn list_walks_every_page() {
    let server = MockServer::start().await;
    Mock::given(method("GET"))
        .and(path("/v1/environments"))
        .and(query_param("cursor", "c1"))
        .and(query_param("limit", "200"))
        .respond_with(ResponseTemplate::new(200).set_body_json(json!({
            "environments": [env_body("vm-2", "running")],
        })))
        .expect(1)
        .mount(&server)
        .await;
    Mock::given(method("GET"))
        .and(path("/v1/environments"))
        .and(query_param("limit", "200"))
        .and(query_param("state", "running"))
        .respond_with(ResponseTemplate::new(200).set_body_json(json!({
            "environments": [env_body("vm-1", "running")],
            "next_cursor": "c1",
        })))
        .expect(1)
        .mount(&server)
        .await;

    let environments = client(&server)
        .await
        .environments()
        .list(ListEnvironmentsOptions::new().state("running".into()))
        .await
        .unwrap();
    assert_eq!(
        environments
            .iter()
            .map(|e| e.id.as_str())
            .collect::<Vec<_>>(),
        ["vm-1", "vm-2"]
    );
}

#[tokio::test]
async fn fork_posts_options_body() {
    let server = MockServer::start().await;
    Mock::given(method("POST"))
        .and(path("/v1/environments/vm-1"))
        .and(query_param("action", "fork"))
        .and(body_partial_json(json!({"reuse_snapshot_id": "snap-1"})))
        .respond_with(ResponseTemplate::new(200).set_body_json(env_body("vm-2", "provisioning")))
        .mount(&server)
        .await;

    let forked = client(&server)
        .await
        .environments()
        .fork("vm-1", ForkOptions::new().reuse_snapshot_id("snap-1"))
        .await
        .unwrap();
    assert_eq!(forked.id, "vm-2");
}

#[tokio::test]
async fn migrate_posts_options_body() {
    let server = MockServer::start().await;
    Mock::given(method("POST"))
        .and(path("/v1/environments/vm-1"))
        .and(query_param("action", "migrate"))
        .and(body_partial_json(
            json!({"target_host_id": "host-b", "live": true, "lazy": true}),
        ))
        .respond_with(ResponseTemplate::new(201).set_body_json(env_body("vm-2", "running")))
        .mount(&server)
        .await;

    let migrated = client(&server)
        .await
        .environments()
        .migrate(
            "vm-1",
            MigrateOptions::new()
                .target_host_id("host-b")
                .live(true)
                .lazy(true),
        )
        .await
        .unwrap();
    assert_eq!(migrated.id, "vm-2");
}

#[tokio::test]
async fn exec_decodes_result_and_rejects_empty_commands() {
    let server = MockServer::start().await;
    Mock::given(method("POST"))
        .and(path("/v1/environments/vm-1"))
        .and(query_param("action", "exec"))
        .and(body_partial_json(json!({"cmd": ["false"]})))
        .respond_with(ResponseTemplate::new(200).set_body_json(json!({
            "exit_code": 1,
            "stdout": "",
            "stderr": "boom",
        })))
        .mount(&server)
        .await;

    let client = client(&server).await;
    let result = client
        .environments()
        .exec("vm-1", ExecRequest::cmd(["false"]))
        .await
        .unwrap();
    assert!(!result.success());
    assert_eq!(result.stderr, "boom");

    let empty: [&str; 0] = [];
    assert!(matches!(
        client
            .environments()
            .exec("vm-1", ExecRequest::cmd(empty))
            .await,
        Err(Error::InvalidRequest(_))
    ));
    assert!(matches!(
        client
            .environments()
            .exec("vm-1", ExecRequest::shell(""))
            .await,
        Err(Error::InvalidRequest(_))
    ));
}

#[tokio::test]
async fn events_stream_parses_sse_and_stops_at_terminal_state() {
    let server = MockServer::start().await;
    let body = concat!(
        ": keepalive\n",
        "data: {\"id\":\"1\",\"vm_id\":\"vm-1\",\"state\":\"provisioning\"}\n",
        "\n",
        "data: {\"event\":\"step\",\"vm_id\":\"vm-1\",\"state\":\"\",\"index\":1,\"total\":2,\"key\":\"apt\",\"cached\":true}\n",
        "\n",
        "data: {\"vm_id\":\"vm-1\",\"state\":\"destroyed\"}\n",
        "\n",
        "data: {\"vm_id\":\"vm-1\",\"state\":\"running\"}\n",
        "\n",
    );
    Mock::given(method("GET"))
        .and(path("/v1/environments/vm-1/events"))
        .and(header("accept", "text/event-stream"))
        .respond_with(ResponseTemplate::new(200).set_body_raw(body, "text/event-stream"))
        .mount(&server)
        .await;

    let mut events = client(&server)
        .await
        .environments()
        .events("vm-1")
        .await
        .unwrap();

    let first = events.next().await.unwrap().unwrap();
    assert_eq!(first.state, Some("provisioning".into()));

    let step = events.next().await.unwrap().unwrap();
    assert!(!step.is_state());
    assert!(step.cached);

    let terminal = events.next().await.unwrap().unwrap();
    assert!(terminal.is_terminal());

    // the terminal event closes the stream; the trailing "running" event
    // must not be delivered.
    assert!(events.next().await.is_none());
    assert!(events.next().await.is_none());
}

#[tokio::test]
async fn computer_tool_result_maps_blocks_and_tool_errors() {
    let server = MockServer::start().await;
    Mock::given(method("POST"))
        .and(path("/v1/environments/vm-1/computer"))
        .and(body_partial_json(json!({"action": "screenshot"})))
        .respond_with(ResponseTemplate::new(200).set_body_json(json!({"screenshot": "cGpn"})))
        .mount(&server)
        .await;
    Mock::given(method("POST"))
        .and(path("/v1/environments/vm-2/computer"))
        .respond_with(ResponseTemplate::new(503).set_body_json(json!({
            "error": {"code": "unavailable", "message": "display is not up"},
        })))
        .mount(&server)
        .await;

    let client = client(&server).await;

    let ok = client
        .environments()
        .computer_tool_result("vm-1", &json!({"action": "screenshot"}))
        .await
        .unwrap();
    assert!(!ok.is_error);
    assert!(matches!(
        &ok.content[0],
        ToolResultBlock::Image { source } if source.data == "cGpn"
    ));

    // the environment refused the action: that goes back to the model as an
    // error tool_result, not up the stack as an Err.
    let refused = client
        .environments()
        .computer_tool_result("vm-2", &json!({"action": "screenshot"}))
        .await
        .unwrap();
    assert!(refused.is_error);
    assert!(matches!(
        &refused.content[0],
        ToolResultBlock::Text { text } if text == "display is not up"
    ));

    let no_action = client
        .environments()
        .computer_tool_result("vm-1", &json!({"coordinate": [1, 2]}))
        .await
        .unwrap();
    assert!(no_action.is_error);

    assert!(matches!(
        client
            .environments()
            .computer_tool_result("vm-1", &json!("not an object"))
            .await,
        Err(Error::InvalidRequest(_))
    ));
}

#[tokio::test]
async fn snapshot_resolve_reports_miss_as_none() {
    let server = MockServer::start().await;
    Mock::given(method("GET"))
        .and(path("/v1/snapshots/resolve"))
        .and(query_param("layer_key", "abc"))
        .and(query_param("arch", "amd64"))
        .respond_with(ResponseTemplate::new(200).set_body_json(json!({"found": false})))
        .mount(&server)
        .await;

    let resolved = client(&server)
        .await
        .snapshots()
        .resolve("abc", Arch::Amd64)
        .await
        .unwrap();
    assert!(resolved.is_none());
}

#[tokio::test]
async fn api_keys_round_trip() {
    let server = MockServer::start().await;
    Mock::given(method("POST"))
        .and(path("/v1/api-keys"))
        .and(body_partial_json(json!({"label": "ci"})))
        .respond_with(ResponseTemplate::new(200).set_body_json(json!({
            "id": "key-1",
            "label": "ci",
            "created_at": "2026-01-02T03:04:05Z",
            "key": "fuse_sk_live",
        })))
        .mount(&server)
        .await;
    Mock::given(method("DELETE"))
        .and(path("/v1/api-keys/key-1"))
        .respond_with(ResponseTemplate::new(204))
        .mount(&server)
        .await;

    let client = client(&server).await;
    let created = client.api_keys().create("ci").await.unwrap();
    assert_eq!(created.key, "fuse_sk_live");
    assert_eq!(created.api_key.id, "key-1");
    client.api_keys().revoke("key-1").await.unwrap();
}

#[tokio::test]
async fn version_distinguishes_wrong_service() {
    let server = MockServer::start().await;
    Mock::given(method("GET"))
        .and(path("/v1/version"))
        .respond_with(
            ResponseTemplate::new(200)
                .insert_header("server", "fuse-orchestrator/0.24.0")
                .set_body_json(json!({"service": "fuse-orchestrator", "version": "0.24.0"})),
        )
        .mount(&server)
        .await;

    let info = client(&server).await.version().await.unwrap();
    assert!(info.is_orchestrator());
    assert_eq!(info.version, "0.24.0");
    assert_eq!(info.server_header, "fuse-orchestrator/0.24.0");

    // a reachable-but-wrong service is a report, not an error.
    let wrong = MockServer::start().await;
    Mock::given(method("GET"))
        .and(path("/v1/version"))
        .respond_with(
            ResponseTemplate::new(404)
                .insert_header("server", "fc-agent/0.1")
                .set_body_string("not json"),
        )
        .mount(&wrong)
        .await;
    let info = Client::new(wrong.uri(), "")
        .unwrap()
        .version()
        .await
        .unwrap();
    assert!(!info.is_orchestrator());
    assert_eq!(info.server_header, "fc-agent/0.1");
    assert_eq!(info.status, 404);
}

fn intervention_body(
    id: &str,
    state: &str,
    decision: Option<serde_json::Value>,
) -> serde_json::Value {
    let mut body = json!({
        "id": id,
        "env_id": "vm-1",
        "kind": "ask",
        "state": state,
        "payload": {"question": "deploy?", "choices": ["yes", "no"]},
        "triggered_by": {"id": "key:k1", "source": "key", "key_id": "k1"},
        "deadline": "2026-01-02T03:14:05Z",
        "on_timeout": "reject",
        "created_at": "2026-01-02T03:04:05Z",
        "updated_at": "2026-01-02T03:04:06Z",
    });
    if let Some(decision) = decision {
        body["decision"] = decision;
    }
    body
}

fn approve_decision(answer: &str) -> serde_json::Value {
    json!({
        "outcome": "approve",
        "answer": answer,
        "decided_by": {"id": "master", "source": "key"},
        "decided_at": "2026-01-02T03:05:00Z",
    })
}

#[tokio::test]
async fn create_carries_hitl() {
    let server = MockServer::start().await;
    Mock::given(method("POST"))
        .and(path("/v1/environments"))
        .and(body_partial_json(json!({
            "hitl": {
                "timeout_seconds": 300,
                "on_timeout": "allow",
                "requests": {"ask": {}},
            },
        })))
        .respond_with(ResponseTemplate::new(200).set_body_json(env_body("vm-1", "provisioning")))
        .expect(1)
        .mount(&server)
        .await;

    let client = client(&server).await;
    client
        .environments()
        .create(
            CreateRequest::new("t-1").hitl(
                HitlSpec::new()
                    .ask()
                    .timeout_seconds(300)
                    .on_timeout(HitlOnTimeout::Allow),
            ),
        )
        .await
        .unwrap();

    // without hitl the field stays off the wire.
    let plain = serde_json::to_value(CreateRequest::new("t-1")).unwrap();
    assert!(plain.get("hitl").is_none());
}

#[tokio::test]
async fn request_human_raises_then_polls_until_decided() {
    let server = MockServer::start().await;
    Mock::given(method("POST"))
        .and(path("/v1/environments/vm-1"))
        .and(query_param("action", "request"))
        .and(body_json(json!({
            "kind": "ask",
            "question": "deploy?",
            "choices": ["yes", "no"],
            "timeout_seconds": 60,
        })))
        .respond_with(
            ResponseTemplate::new(201).set_body_json(intervention_body("int_1", "pending", None)),
        )
        .expect(1)
        .mount(&server)
        .await;
    // pending on the first poll, decided on the second.
    Mock::given(method("GET"))
        .and(path("/v1/interventions/int_1"))
        .and(query_param("wait", "30s"))
        .respond_with(
            ResponseTemplate::new(200).set_body_json(intervention_body("int_1", "pending", None)),
        )
        .up_to_n_times(1)
        .expect(1)
        .mount(&server)
        .await;
    Mock::given(method("GET"))
        .and(path("/v1/interventions/int_1"))
        .and(query_param("wait", "30s"))
        .respond_with(ResponseTemplate::new(200).set_body_json(intervention_body(
            "int_1",
            "approved",
            Some(approve_decision("yes")),
        )))
        .expect(1)
        .mount(&server)
        .await;

    let intervention = client(&server)
        .await
        .environments()
        .request_human(
            "vm-1",
            HumanRequest::ask("deploy?")
                .choices(["yes", "no"])
                .timeout_seconds(60),
        )
        .await
        .unwrap();
    assert!(intervention.is_terminal());
    assert!(intervention.is_approved());
    assert_eq!(intervention.state, InterventionState::Approved);
    let decision = intervention.decision.unwrap();
    assert_eq!(decision.answer.as_deref(), Some("yes"));
    assert_eq!(decision.decided_by.id, "master");
}

#[tokio::test]
async fn request_human_no_wait_does_not_poll() {
    let server = MockServer::start().await;
    Mock::given(method("POST"))
        .and(path("/v1/environments/vm-1"))
        .and(query_param("action", "request"))
        .respond_with(
            ResponseTemplate::new(201).set_body_json(intervention_body("int_1", "pending", None)),
        )
        .expect(1)
        .mount(&server)
        .await;

    let intervention = client(&server)
        .await
        .environments()
        .request_human("vm-1", HumanRequest::ask("q").no_wait())
        .await
        .unwrap();
    assert_eq!(intervention.state, InterventionState::Pending);
    assert!(!intervention.is_terminal());

    let requests = server.received_requests().await.unwrap();
    assert_eq!(requests.len(), 1);
    let body: serde_json::Value = serde_json::from_slice(&requests[0].body).unwrap();
    assert_eq!(body, json!({"kind": "ask", "question": "q"}));
}

#[tokio::test]
async fn interventions_approve_and_reject_send_action_and_body() {
    let server = MockServer::start().await;
    Mock::given(method("POST"))
        .and(path("/v1/interventions/int_1"))
        .and(query_param("action", "approve"))
        .and(body_json(json!({"answer": "yes"})))
        .respond_with(ResponseTemplate::new(200).set_body_json(intervention_body(
            "int_1",
            "approved",
            Some(approve_decision("yes")),
        )))
        .expect(1)
        .mount(&server)
        .await;
    Mock::given(method("POST"))
        .and(path("/v1/interventions/int_2"))
        .and(query_param("action", "reject"))
        .and(body_json(json!({"reason": "no"})))
        .respond_with(ResponseTemplate::new(200).set_body_json(intervention_body(
            "int_2",
            "rejected",
            Some(json!({
                "outcome": "reject",
                "reason": "no",
                "decided_by": {"id": "master", "source": "key"},
                "decided_at": "2026-01-02T03:05:00Z",
            })),
        )))
        .expect(1)
        .mount(&server)
        .await;

    let client = client(&server).await;
    let approved = client
        .interventions()
        .approve("int_1", "yes", "")
        .await
        .unwrap();
    assert!(approved.is_approved());
    let rejected = client.interventions().reject("int_2", "no").await.unwrap();
    assert!(!rejected.is_approved());
    assert_eq!(
        rejected.decision.unwrap().outcome,
        InterventionOutcome::Reject
    );
    assert!(matches!(
        client.interventions().approve("", "yes", "").await,
        Err(Error::InvalidRequest(_))
    ));
}

#[tokio::test]
async fn interventions_list_walks_every_page() {
    let server = MockServer::start().await;
    Mock::given(method("GET"))
        .and(path("/v1/interventions"))
        .and(query_param("cursor", "c1"))
        .and(query_param("limit", "200"))
        .respond_with(ResponseTemplate::new(200).set_body_json(json!({
            "interventions": [intervention_body("int_2", "pending", None)],
        })))
        .expect(1)
        .mount(&server)
        .await;
    Mock::given(method("GET"))
        .and(path("/v1/interventions"))
        .and(query_param("env", "vm-1"))
        .and(query_param("state", "pending"))
        .and(query_param("limit", "200"))
        .respond_with(ResponseTemplate::new(200).set_body_json(json!({
            "interventions": [intervention_body("int_1", "pending", None)],
            "next_cursor": "c1",
        })))
        .expect(1)
        .mount(&server)
        .await;

    let all = client(&server)
        .await
        .interventions()
        .list(
            ListInterventionsOptions::new()
                .env_id("vm-1")
                .state(InterventionState::Pending),
        )
        .await
        .unwrap();
    assert_eq!(
        all.iter().map(|i| i.id.as_str()).collect::<Vec<_>>(),
        ["int_1", "int_2"]
    );
}

#[test]
fn intervention_decodes_unknown_enum_values() {
    let mut body = intervention_body("int_1", "escalated", None);
    body["kind"] = json!("approve_tool");
    body["triggered_by"]["source"] = json!("oidc");
    let intervention: fuse::Intervention = serde_json::from_value(body).unwrap();
    assert_eq!(
        intervention.state,
        InterventionState::Other("escalated".into())
    );
    assert!(intervention.is_terminal());
}

#[tokio::test]
async fn with_actor_sends_actor_headers_and_leaves_original_unchanged() {
    let server = MockServer::start().await;
    Mock::given(method("GET"))
        .and(path("/v1/interventions/int_1"))
        .and(header("fuse-actor", "alice"))
        .and(header("fuse-actor-name", "Alice"))
        .respond_with(
            ResponseTemplate::new(200).set_body_json(intervention_body("int_1", "pending", None)),
        )
        .expect(1)
        .mount(&server)
        .await;
    Mock::given(method("GET"))
        .and(path("/v1/interventions/int_2"))
        .respond_with(
            ResponseTemplate::new(200).set_body_json(intervention_body("int_2", "pending", None)),
        )
        .expect(1)
        .mount(&server)
        .await;

    let base = client(&server).await;
    let alice = base.with_actor(ActorLabel::new("alice").name("Alice"));
    alice.interventions().get("int_1").await.unwrap();
    base.interventions().get("int_2").await.unwrap();

    let requests = server.received_requests().await.unwrap();
    let plain = requests
        .iter()
        .find(|r| r.url.path() == "/v1/interventions/int_2")
        .unwrap();
    assert!(plain.headers.get("fuse-actor").is_none());
    assert!(plain.headers.get("fuse-actor-name").is_none());
    assert_eq!(plain.headers.get("authorization").unwrap(), "Bearer secret");
}

#[tokio::test]
async fn builder_actor_sends_actor_headers() {
    let server = MockServer::start().await;
    Mock::given(method("GET"))
        .and(path("/v1/interventions/int_1"))
        .and(header("fuse-actor", "bob"))
        .respond_with(
            ResponseTemplate::new(200).set_body_json(intervention_body("int_1", "pending", None)),
        )
        .expect(1)
        .mount(&server)
        .await;

    let client = Client::builder(server.uri())
        .token("secret")
        .actor(ActorLabel::new("bob"))
        .build()
        .unwrap();
    client.interventions().get("int_1").await.unwrap();

    // no name means no name header.
    let requests = server.received_requests().await.unwrap();
    assert!(requests[0].headers.get("fuse-actor-name").is_none());
}

#[tokio::test]
async fn scoped_api_key_create_sends_options_and_parses_fields() {
    let server = MockServer::start().await;
    Mock::given(method("POST"))
        .and(path("/v1/api-keys"))
        .and(body_json(json!({
            "label": "reviewer",
            "scopes": ["hitl:read", "hitl:decide"],
            "env_id": "vm-1",
            "ttl_seconds": 3600,
            "actor": {"id": "alice", "name": "Alice"},
        })))
        .respond_with(ResponseTemplate::new(201).set_body_json(json!({
            "id": "key-2",
            "label": "reviewer",
            "created_at": "2026-01-02T03:04:05Z",
            "scopes": ["hitl:read", "hitl:decide"],
            "env_id": "vm-1",
            "expires_at": "2026-01-02T04:04:05Z",
            "actor": {"id": "alice", "name": "Alice"},
            "created_by": "master",
            "key": "fuse_sk_scoped",
        })))
        .expect(1)
        .mount(&server)
        .await;

    let created = client(&server)
        .await
        .api_keys()
        .create_with_options(
            CreateApiKeyOptions::new()
                .label("reviewer")
                .scopes([HitlScope::Read, HitlScope::Decide])
                .env_id("vm-1")
                .ttl(Duration::from_secs(3600))
                .actor(ActorLabel::new("alice").name("Alice")),
        )
        .await
        .unwrap();
    assert_eq!(created.key, "fuse_sk_scoped");
    let key = created.api_key;
    assert_eq!(key.scopes, Some(vec![HitlScope::Read, HitlScope::Decide]));
    assert_eq!(key.env_id.as_deref(), Some("vm-1"));
    assert_eq!(
        key.expires_at.unwrap().to_rfc3339(),
        "2026-01-02T04:04:05+00:00"
    );
    assert_eq!(key.actor, Some(ActorLabel::new("alice").name("Alice")));
    assert_eq!(key.created_by.as_deref(), Some("master"));
}

#[tokio::test]
async fn audit_page_sends_filters_and_parses_entries() {
    let server = MockServer::start().await;
    Mock::given(method("GET"))
        .and(path("/v1/hitl/audit"))
        .and(query_param("env", "vm-1"))
        .and(query_param("actor", "alice"))
        .and(query_param("since", "24h"))
        .and(query_param("limit", "10"))
        .and(query_param("cursor", "c0"))
        .respond_with(ResponseTemplate::new(200).set_body_json(json!({
            "entries": [{
                "seq": 7,
                "event": "decided",
                "intervention_id": "int_1",
                "env_id": "vm-1",
                "actor": {"id": "alice", "name": "Alice", "source": "pinned", "key_id": "k2"},
                "remote_addr": "10.0.0.9:5000",
                "at": "2026-01-02T03:04:05Z",
                "prev_hash": "aa",
                "hash": "bb",
            }],
            "next_cursor": "c1",
        })))
        .expect(1)
        .mount(&server)
        .await;

    let page = client(&server)
        .await
        .interventions()
        .audit_page(
            AuditOptions::new()
                .env_id("vm-1")
                .actor("alice")
                .since("24h")
                .limit(10)
                .cursor("c0"),
        )
        .await
        .unwrap();
    assert_eq!(page.next_cursor.as_deref(), Some("c1"));
    let entry = &page.entries[0];
    assert_eq!(entry.seq, 7);
    assert_eq!(entry.event, AuditEvent::Decided);
    assert_eq!(entry.intervention_id.as_deref(), Some("int_1"));
    assert_eq!(entry.actor.source, ActorSource::Pinned);
    assert_eq!(entry.actor.key_id.as_deref(), Some("k2"));
    assert_eq!(entry.remote_addr.as_deref(), Some("10.0.0.9:5000"));
    assert_eq!(entry.prev_hash, "aa");
    assert_eq!(entry.hash, "bb");
}

#[tokio::test]
async fn audit_walks_every_page() {
    let server = MockServer::start().await;
    let entry = |seq: i64| {
        json!({
            "seq": seq,
            "event": "raised",
            "actor": {"id": "master", "source": "key"},
            "at": "2026-01-02T03:04:05Z",
            "prev_hash": "",
            "hash": "cc",
        })
    };
    Mock::given(method("GET"))
        .and(path("/v1/hitl/audit"))
        .and(query_param("cursor", "c1"))
        .respond_with(ResponseTemplate::new(200).set_body_json(json!({"entries": [entry(2)]})))
        .expect(1)
        .mount(&server)
        .await;
    Mock::given(method("GET"))
        .and(path("/v1/hitl/audit"))
        .and(query_param("limit", "200"))
        .respond_with(
            ResponseTemplate::new(200)
                .set_body_json(json!({"entries": [entry(1)], "next_cursor": "c1"})),
        )
        .up_to_n_times(1)
        .mount(&server)
        .await;

    let all = client(&server)
        .await
        .interventions()
        .audit(AuditOptions::new())
        .await
        .unwrap();
    assert_eq!(all.iter().map(|e| e.seq).collect::<Vec<_>>(), [1, 2]);
}

#[tokio::test]
async fn interventions_list_sends_actor_filter() {
    let server = MockServer::start().await;
    Mock::given(method("GET"))
        .and(path("/v1/interventions"))
        .and(query_param("actor", "alice"))
        .respond_with(ResponseTemplate::new(200).set_body_json(json!({"interventions": []})))
        .expect(1)
        .mount(&server)
        .await;

    client(&server)
        .await
        .interventions()
        .list_page(ListInterventionsOptions::new().actor("alice"))
        .await
        .unwrap();
}

#[tokio::test]
async fn forbidden_error_surfaces_reason_and_scope() {
    let server = MockServer::start().await;
    Mock::given(method("POST"))
        .and(path("/v1/interventions/int_1"))
        .respond_with(ResponseTemplate::new(403).set_body_json(json!({
            "error": {
                "code": "forbidden",
                "message": "a key cannot approve its own request",
                "details": {"reason": "self_approval"},
            },
        })))
        .mount(&server)
        .await;
    Mock::given(method("GET"))
        .and(path("/v1/hitl/audit"))
        .respond_with(ResponseTemplate::new(403).set_body_json(json!({
            "error": {
                "code": "forbidden",
                "message": "this request needs the hitl:read scope",
                "details": {"reason": "missing_scope", "scope": "hitl:read"},
            },
        })))
        .mount(&server)
        .await;

    let client = client(&server).await;
    let err = client
        .interventions()
        .approve("int_1", "yes", "")
        .await
        .unwrap_err();
    assert!(err.is_forbidden());
    assert_eq!(err.status(), Some(403));
    assert_eq!(err.forbidden_reason(), Some(ForbiddenReason::SelfApproval));
    assert_eq!(err.forbidden_scope(), None);

    let err = client
        .interventions()
        .audit_page(AuditOptions::new())
        .await
        .unwrap_err();
    assert_eq!(err.forbidden_reason(), Some(ForbiddenReason::MissingScope));
    assert_eq!(err.forbidden_scope(), Some("hitl:read"));
}

#[tokio::test]
async fn unauthorized_403_is_not_forbidden() {
    // older servers refused master-only routes with unauthorized and a 403.
    let server = MockServer::start().await;
    Mock::given(method("GET"))
        .and(path("/v1/api-keys"))
        .respond_with(ResponseTemplate::new(403).set_body_json(json!({
            "error": {"code": "unauthorized", "message": "master token required"},
        })))
        .mount(&server)
        .await;

    let err = client(&server).await.api_keys().list().await.unwrap_err();
    assert!(err.is_unauthorized());
    assert!(!err.is_forbidden());
    assert_eq!(err.forbidden_reason(), None);
}

#[tokio::test]
async fn create_carries_hitl_notify_and_require_actor() {
    let server = MockServer::start().await;
    Mock::given(method("POST"))
        .and(path("/v1/environments"))
        .and(body_partial_json(json!({
            "hitl": {
                "requests": {"ask": {}},
                "require_actor": true,
                "notify": [{"url": "https://hooks.example.com/fuse", "secret": "HOOK_KEY"}],
            },
        })))
        .respond_with(ResponseTemplate::new(200).set_body_json(env_body("vm-1", "provisioning")))
        .expect(1)
        .mount(&server)
        .await;

    client(&server)
        .await
        .environments()
        .create(
            CreateRequest::new("t-1").secret("HOOK_KEY", "s3cret").hitl(
                HitlSpec::new()
                    .ask()
                    .require_actor()
                    .notify("https://hooks.example.com/fuse", "HOOK_KEY"),
            ),
        )
        .await
        .unwrap();

    // the defaults stay off the wire.
    let plain = serde_json::to_value(HitlSpec::new().ask()).unwrap();
    assert!(plain.get("require_actor").is_none());
    assert!(plain.get("notify").is_none());
}

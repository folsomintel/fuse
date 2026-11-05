// hitl secret requests, terminal takeover, stall and expiry signals, and the
// 423 lock that exec and computer calls wait out (issue #312).

use fuse::{
    ApproveOptions, AuditEvent, AuditOptions, Client, ComputerAction, CreateRequest, Error,
    ExecRequest, HitlKind, HitlSecretTarget, HitlSpec, HumanRequest, InterventionState, Reaper,
    ReleaseOptions, TakeoverOptions,
};
use serde_json::json;
use wiremock::matchers::{body_json, body_partial_json, method, path, query_param};
use wiremock::{Mock, MockServer, ResponseTemplate};

async fn client(server: &MockServer) -> Client {
    Client::new(server.uri(), "secret").unwrap()
}

fn intervention(
    id: &str,
    kind: &str,
    state: &str,
    payload: serde_json::Value,
    answer: Option<&str>,
) -> serde_json::Value {
    let mut body = json!({
        "id": id,
        "env_id": "vm-1",
        "kind": kind,
        "state": state,
        "payload": payload,
        "triggered_by": {"id": "master", "source": "key"},
        "deadline": "2026-01-02T03:14:05Z",
        "on_timeout": "reject",
        "created_at": "2026-01-02T03:04:05Z",
        "updated_at": "2026-01-02T03:04:06Z",
    });
    if let Some(answer) = answer {
        body["decision"] = json!({
            "outcome": "approve",
            "answer": answer,
            "decided_by": {"id": "master", "source": "key"},
            "decided_at": "2026-01-02T03:05:00Z",
        });
    }
    body
}

fn locked(id: &str, kind: &str) -> ResponseTemplate {
    ResponseTemplate::new(423).set_body_json(json!({
        "error": {
            "code": "locked",
            "message": "a human holds the environment",
            "details": {"intervention_id": id, "kind": kind},
        }
    }))
}

#[tokio::test]
async fn secret_request_and_answer() {
    let server = MockServer::start().await;
    let secret_payload = json!({
        "secret": {"target": "GH_TOKEN", "label": "github token", "message": "need push"},
    });
    Mock::given(method("POST"))
        .and(path("/v1/environments"))
        .and(body_partial_json(json!({
            "hitl": {"requests": {
                "secret": {"targets": [
                    {"env": "GH_TOKEN", "label": "github token"},
                    {"file": "/root/.netrc", "label": "netrc"},
                ]},
                "takeover": {},
            }},
        })))
        .respond_with(ResponseTemplate::new(200).set_body_json(json!({
            "id": "vm-1", "state": "provisioning", "task_id": "t-1", "url": "",
            "spec": {}, "created_at": "2026-01-02T03:04:05Z", "updated_at": "2026-01-02T03:04:05Z",
        })))
        .expect(1)
        .mount(&server)
        .await;
    Mock::given(method("POST"))
        .and(path("/v1/environments/vm-1"))
        .and(query_param("action", "request"))
        .and(body_json(json!({
            "kind": "secret",
            "target": "GH_TOKEN",
            "message": "need push",
        })))
        .respond_with(ResponseTemplate::new(201).set_body_json(intervention(
            "int_s",
            "secret",
            "pending",
            secret_payload.clone(),
            None,
        )))
        .expect(1)
        .mount(&server)
        .await;
    Mock::given(method("POST"))
        .and(path("/v1/interventions/int_s"))
        .and(query_param("action", "secret"))
        .and(body_json(json!({"value": "ghp_x"})))
        .respond_with(ResponseTemplate::new(200).set_body_json(intervention(
            "int_s",
            "secret",
            "approved",
            secret_payload.clone(),
            Some("done"),
        )))
        .expect(1)
        .mount(&server)
        .await;
    Mock::given(method("GET"))
        .and(path("/v1/interventions/int_s"))
        .respond_with(ResponseTemplate::new(200).set_body_json(intervention(
            "int_s",
            "secret",
            "approved",
            secret_payload,
            Some("done"),
        )))
        .expect(1)
        .mount(&server)
        .await;

    let client = client(&server).await;
    client
        .environments()
        .create(
            CreateRequest::new("t-1").hitl(
                HitlSpec::new()
                    .secret_target(HitlSecretTarget::env("GH_TOKEN", "github token"))
                    .secret_target(HitlSecretTarget::file("/root/.netrc", "netrc"))
                    .takeover(),
            ),
        )
        .await
        .unwrap();

    let pending = client
        .environments()
        .request_human(
            "vm-1",
            HumanRequest::secret("GH_TOKEN")
                .message("need push")
                .no_wait(),
        )
        .await
        .unwrap();
    assert_eq!(pending.kind, HitlKind::Secret);
    let secret = pending.payload.secret.as_ref().unwrap();
    assert_eq!(secret.target, "GH_TOKEN");
    assert_eq!(secret.label, "github token");
    assert_eq!(secret.message.as_deref(), Some("need push"));

    let answered = client
        .interventions()
        .answer_secret("int_s", "ghp_x")
        .await
        .unwrap();
    assert_eq!(answered.state, InterventionState::Approved);

    let resolved = client.interventions().wait("int_s").await.unwrap();
    assert_eq!(resolved.decision.unwrap().answer.as_deref(), Some("done"));

    // an empty value is refused before anything is sent.
    assert!(matches!(
        client.interventions().answer_secret("int_s", "").await,
        Err(Error::InvalidRequest(_))
    ));
}

#[tokio::test]
async fn takeover_and_release() {
    let server = MockServer::start().await;
    let held_payload = json!({"takeover": {
        "via": "terminal",
        "reason": "captcha",
        "holder": {"id": "alice", "source": "asserted"},
        "started_at": "2026-01-02T03:05:00Z",
    }});
    Mock::given(method("POST"))
        .and(path("/v1/environments/vm-1"))
        .and(query_param("action", "request"))
        .and(body_json(json!({
            "kind": "takeover",
            "via": "terminal",
            "reason": "captcha",
        })))
        .respond_with(ResponseTemplate::new(201).set_body_json(intervention(
            "int_t",
            "takeover",
            "pending",
            json!({"takeover": {"via": "terminal", "reason": "captcha"}}),
            None,
        )))
        .expect(1)
        .mount(&server)
        .await;
    Mock::given(method("POST"))
        .and(path("/v1/environments/vm-1"))
        .and(query_param("action", "takeover"))
        .and(body_json(json!({"via": "terminal", "reason": "captcha"})))
        .respond_with(ResponseTemplate::new(200).set_body_json(intervention(
            "int_t",
            "takeover",
            "pending",
            held_payload.clone(),
            None,
        )))
        .expect(1)
        .mount(&server)
        .await;
    Mock::given(method("POST"))
        .and(path("/v1/environments/vm-1"))
        .and(query_param("action", "release"))
        .and(body_json(json!({"note": "solved it"})))
        .respond_with(ResponseTemplate::new(200).set_body_json(intervention(
            "int_t",
            "takeover",
            "approved",
            held_payload,
            Some("solved it"),
        )))
        .expect(1)
        .mount(&server)
        .await;
    Mock::given(method("GET"))
        .and(path("/v1/hitl/audit"))
        .respond_with(
            ResponseTemplate::new(200).set_body_json(json!({"entries": [{
                "seq": 7,
                "event": "takeover_session",
                "intervention_id": "int_t",
                "env_id": "vm-1",
                "actor": {"id": "alice", "source": "asserted"},
                "recording_id": "rec_1",
                "at": "2026-01-02T03:06:00Z",
                "prev_hash": "aa",
                "hash": "bb",
            }]})),
        )
        .expect(1)
        .mount(&server)
        .await;
    let cast = "{\"version\": 2, \"width\": 80, \"height\": 24}\n[0.1, \"o\", \"$ \"]\n";
    Mock::given(method("GET"))
        .and(path("/v1/hitl/recordings/rec_1"))
        .respond_with(
            ResponseTemplate::new(200)
                .insert_header("content-type", "application/x-asciicast")
                .set_body_string(cast),
        )
        .expect(1)
        .mount(&server)
        .await;

    let client = client(&server).await;
    let request = client
        .environments()
        .request_human("vm-1", HumanRequest::takeover().reason("captcha").no_wait())
        .await
        .unwrap();
    assert_eq!(request.kind, HitlKind::Takeover);

    let held = client
        .environments()
        .takeover(
            "vm-1",
            TakeoverOptions::new()
                .via(fuse::TakeoverVia::Terminal)
                .reason("captcha"),
        )
        .await
        .unwrap();
    let takeover = held.payload.takeover.unwrap();
    assert_eq!(takeover.holder.unwrap().id, "alice");
    assert!(takeover.started_at.is_some());

    let released = client
        .environments()
        .release("vm-1", ReleaseOptions::new().note("solved it"))
        .await
        .unwrap();
    assert_eq!(
        released.decision.unwrap().answer.as_deref(),
        Some("solved it")
    );

    let entries = client
        .interventions()
        .audit(AuditOptions::new())
        .await
        .unwrap();
    assert_eq!(entries[0].event, AuditEvent::TakeoverSession);
    let recording_id = entries[0].recording_id.as_deref().unwrap();
    let text = client
        .interventions()
        .recording(recording_id)
        .await
        .unwrap();
    assert_eq!(text, cast);
}

#[tokio::test]
async fn exec_and_computer_wait_out_a_lock_then_retry() {
    let server = MockServer::start().await;
    // the first exec and the first computer action are locked, the retries
    // go through.
    Mock::given(method("POST"))
        .and(path("/v1/environments/vm-1"))
        .and(query_param("action", "exec"))
        .respond_with(locked("int_t", "takeover"))
        .up_to_n_times(1)
        .expect(1)
        .mount(&server)
        .await;
    Mock::given(method("POST"))
        .and(path("/v1/environments/vm-1"))
        .and(query_param("action", "exec"))
        .and(body_json(json!({"cmd": ["ls"]})))
        .respond_with(ResponseTemplate::new(200).set_body_json(json!({
            "exit_code": 0, "stdout": "ok\n", "stderr": "",
        })))
        .expect(1)
        .mount(&server)
        .await;
    Mock::given(method("POST"))
        .and(path("/v1/environments/vm-1/computer"))
        .respond_with(locked("int_st", "stall"))
        .up_to_n_times(1)
        .expect(1)
        .mount(&server)
        .await;
    Mock::given(method("POST"))
        .and(path("/v1/environments/vm-1/computer"))
        .respond_with(ResponseTemplate::new(200).set_body_json(json!({"screenshot": "aGk="})))
        .expect(1)
        .mount(&server)
        .await;
    // the lock's intervention is pending on the first poll, released on the
    // second.
    Mock::given(method("GET"))
        .and(path("/v1/interventions/int_t"))
        .and(query_param("wait", "30s"))
        .respond_with(ResponseTemplate::new(200).set_body_json(intervention(
            "int_t",
            "takeover",
            "pending",
            json!({"takeover": {"via": "terminal"}}),
            None,
        )))
        .up_to_n_times(1)
        .expect(1)
        .mount(&server)
        .await;
    Mock::given(method("GET"))
        .and(path("/v1/interventions/int_t"))
        .respond_with(ResponseTemplate::new(200).set_body_json(intervention(
            "int_t",
            "takeover",
            "approved",
            json!({"takeover": {"via": "terminal"}}),
            Some("all yours"),
        )))
        .expect(1)
        .mount(&server)
        .await;
    Mock::given(method("GET"))
        .and(path("/v1/interventions/int_st"))
        .respond_with(ResponseTemplate::new(200).set_body_json(intervention(
            "int_st",
            "stall",
            "approved",
            json!({"signal": {"failures": 3, "last_exit_code": 1}}),
            Some(""),
        )))
        .expect(1)
        .mount(&server)
        .await;

    let client = client(&server).await;
    let result = client
        .environments()
        .exec("vm-1", ExecRequest::cmd(["ls"]))
        .await
        .unwrap();
    assert_eq!(result.stdout, "ok\n");

    let shot = client
        .environments()
        .computer("vm-1", ComputerAction::screenshot())
        .await
        .unwrap();
    assert_eq!(shot.screenshot.as_deref(), Some("aGk="));
}

#[tokio::test]
async fn locked_error_when_not_waiting() {
    let server = MockServer::start().await;
    Mock::given(method("POST"))
        .and(path("/v1/environments/vm-1"))
        .and(query_param("action", "exec"))
        .respond_with(locked("int_t", "takeover"))
        .mount(&server)
        .await;
    Mock::given(method("POST"))
        .and(path("/v1/environments/vm-1/computer"))
        .respond_with(locked("int_st", "stall"))
        .mount(&server)
        .await;

    // per exec.
    let err = client(&server)
        .await
        .environments()
        .exec("vm-1", ExecRequest::cmd(["ls"]).no_wait())
        .await
        .unwrap_err();
    assert!(err.is_locked());
    let lock = err.locked().unwrap();
    assert_eq!(lock.intervention_id, "int_t");
    assert_eq!(lock.kind, HitlKind::Takeover);
    assert!(err.to_string().contains("int_t"));

    // client-wide, for exec and computer.
    let client = Client::builder(server.uri())
        .token("secret")
        .hitl_no_wait()
        .build()
        .unwrap();
    let err = client
        .environments()
        .exec("vm-1", ExecRequest::cmd(["ls"]))
        .await
        .unwrap_err();
    assert_eq!(err.locked().unwrap().intervention_id, "int_t");
    let err = client
        .environments()
        .computer("vm-1", ComputerAction::screenshot())
        .await
        .unwrap_err();
    let lock = err.locked().unwrap();
    assert_eq!(lock.intervention_id, "int_st");
    assert_eq!(lock.kind, HitlKind::Stall);
    let err = client
        .environments()
        .computer_tool_result("vm-1", &json!({"action": "screenshot"}))
        .await
        .unwrap_err();
    assert!(err.is_locked());

    // nothing waited on the lock.
    let requests = server.received_requests().await.unwrap();
    assert!(requests.iter().all(|r| r.method.as_str() == "POST"));
}

#[tokio::test]
async fn expiry_approve_extends_and_signal_decodes() {
    let server = MockServer::start().await;
    let payload = json!({"signal": {
        "reaper": "idle_timeout",
        "reap_at": "2026-01-02T03:10:00Z",
        "extended_until": "2026-01-02T03:40:00Z",
    }});
    Mock::given(method("POST"))
        .and(path("/v1/interventions/int_e"))
        .and(query_param("action", "approve"))
        .and(body_json(json!({"extend_seconds": 1800})))
        .respond_with(ResponseTemplate::new(200).set_body_json(intervention(
            "int_e",
            "expiry",
            "approved",
            payload,
            Some(""),
        )))
        .expect(1)
        .mount(&server)
        .await;

    let approved = client(&server)
        .await
        .interventions()
        .approve_with("int_e", ApproveOptions::new().extend_seconds(1800))
        .await
        .unwrap();
    assert_eq!(approved.kind, HitlKind::Expiry);
    let signal = approved.payload.signal.unwrap();
    assert_eq!(signal.reaper, Some(Reaper::IdleTimeout));
    assert!(signal.extended_until.is_some());
}

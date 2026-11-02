// hitl exec and lifecycle gate tests against a mock orchestrator: a held
// call waits until the intervention settles, opt-outs return the pending
// intervention, and rejections, denies and failed runs surface as errors.

use std::sync::{Arc, Mutex};

use fuse::{
    ApproveOptions, Client, CreateRequest, Error, ExecRequest, ForbiddenReason, ForkOptions,
    GateMode, HitlExecPolicy, HitlGate, HitlGateMatch, HitlSpec, LifecycleAction, SnapshotRequest,
};
use serde_json::{json, Value};
use wiremock::matchers::{body_json, body_partial_json, method, path, query_param};
use wiremock::{Mock, MockServer, ResponseTemplate};

async fn client(server: &MockServer) -> Client {
    Client::new(server.uri(), "secret").unwrap()
}

// gated builds an intervention a gate raised, in `state`, with `extra`
// fields (decision, result) merged on top.
fn gated(id: &str, kind: &str, payload: Value, state: &str, extra: Value) -> Value {
    let mut body = json!({
        "id": id,
        "env_id": "vm-1",
        "kind": kind,
        "gate": "force-push",
        "mode": "ask",
        "state": state,
        "payload": payload,
        "triggered_by": {"id": "master", "source": "key"},
        "deadline": "2026-01-02T03:14:05Z",
        "on_timeout": "reject",
        "created_at": "2026-01-02T03:04:05Z",
        "updated_at": "2026-01-02T03:04:06Z",
    });
    for (key, value) in extra.as_object().unwrap() {
        body[key] = value.clone();
    }
    body
}

fn decision(outcome: &str, reason: Option<&str>) -> Value {
    let mut d = json!({
        "outcome": outcome,
        "decided_by": {"id": "master", "source": "key"},
        "decided_at": "2026-01-02T03:05:00Z",
    });
    if let Some(reason) = reason {
        d["reason"] = json!(reason);
    }
    d
}

fn exec_payload() -> Value {
    json!({"exec": {"argv": ["git", "push", "--force"]}})
}

fn lifecycle_payload(action: &str) -> Value {
    json!({"lifecycle": {"action": action}})
}

fn held(intervention: Value) -> ResponseTemplate {
    let id = intervention["id"].clone();
    ResponseTemplate::new(202).set_body_json(json!({
        "intervention_id": id,
        "intervention": intervention,
    }))
}

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

#[tokio::test]
async fn exec_waits_until_held_intervention_settles() {
    let server = MockServer::start().await;
    Mock::given(method("POST"))
        .and(path("/v1/environments/vm-1"))
        .and(query_param("action", "exec"))
        .respond_with(held(gated(
            "int_1",
            "exec",
            exec_payload(),
            "pending",
            json!({}),
        )))
        .expect(1)
        .mount(&server)
        .await;
    // pending, then approved but not run yet (not settled), then run.
    Mock::given(method("GET"))
        .and(path("/v1/interventions/int_1"))
        .respond_with(ResponseTemplate::new(200).set_body_json(gated(
            "int_1",
            "exec",
            exec_payload(),
            "pending",
            json!({}),
        )))
        .up_to_n_times(1)
        .mount(&server)
        .await;
    Mock::given(method("GET"))
        .and(path("/v1/interventions/int_1"))
        .respond_with(ResponseTemplate::new(200).set_body_json(gated(
            "int_1",
            "exec",
            exec_payload(),
            "approved",
            json!({"decision": decision("approve", None)}),
        )))
        .up_to_n_times(1)
        .mount(&server)
        .await;
    Mock::given(method("GET"))
        .and(path("/v1/interventions/int_1"))
        .respond_with(ResponseTemplate::new(200).set_body_json(gated(
            "int_1",
            "exec",
            exec_payload(),
            "approved",
            json!({
                "decision": decision("approve", None),
                "result": {
                    "exec": {"exit_code": 0, "stdout": "pushed", "stderr": ""},
                    "finished_at": "2026-01-02T03:06:00Z",
                },
            }),
        )))
        .mount(&server)
        .await;

    let seen = Arc::new(Mutex::new(String::new()));
    let seen_in = seen.clone();
    let result = client(&server)
        .await
        .environments()
        .exec(
            "vm-1",
            ExecRequest::cmd(["git", "push", "--force"])
                .on_held(move |held| *seen_in.lock().unwrap() = held.id.clone()),
        )
        .await
        .unwrap();
    assert_eq!(result.stdout, "pushed");
    assert_eq!(*seen.lock().unwrap(), "int_1");

    let polls = server
        .received_requests()
        .await
        .unwrap()
        .iter()
        .filter(|r| r.url.path() == "/v1/interventions/int_1")
        .count();
    assert_eq!(polls, 3);
}

#[tokio::test]
async fn exec_no_wait_returns_pending_without_polling() {
    let server = MockServer::start().await;
    Mock::given(method("POST"))
        .and(path("/v1/environments/vm-1"))
        .respond_with(held(gated(
            "int_1",
            "exec",
            exec_payload(),
            "pending",
            json!({}),
        )))
        .expect(1)
        .mount(&server)
        .await;

    let err = client(&server)
        .await
        .environments()
        .exec("vm-1", ExecRequest::cmd(["git"]).no_wait())
        .await
        .unwrap_err();
    let Error::InterventionPending(pending) = &err else {
        panic!("err = {err}");
    };
    assert_eq!(pending.id, "int_1");
    assert_eq!(pending.gate.as_deref(), Some("force-push"));
    assert_eq!(err.intervention().map(|i| i.id.as_str()), Some("int_1"));
    assert_eq!(server.received_requests().await.unwrap().len(), 1);
}

#[tokio::test]
async fn client_hitl_no_wait_and_on_held_apply_to_every_gated_call() {
    let server = MockServer::start().await;
    Mock::given(method("DELETE"))
        .and(path("/v1/environments/vm-1"))
        .respond_with(held(gated(
            "int_4",
            "lifecycle",
            lifecycle_payload("destroy"),
            "pending",
            json!({}),
        )))
        .expect(1)
        .mount(&server)
        .await;

    let seen = Arc::new(Mutex::new(String::new()));
    let seen_in = seen.clone();
    let client = Client::builder(server.uri())
        .token("secret")
        .hitl_no_wait()
        .on_held(move |held| *seen_in.lock().unwrap() = held.id.clone())
        .build()
        .unwrap();
    let err = client.environments().destroy("vm-1").await.unwrap_err();
    assert!(matches!(err, Error::InterventionPending(_)), "err = {err}");
    assert_eq!(*seen.lock().unwrap(), "int_4");
    assert_eq!(server.received_requests().await.unwrap().len(), 1);
}

#[tokio::test]
async fn destroy_denied_and_drain_rejected() {
    let server = MockServer::start().await;
    Mock::given(method("DELETE"))
        .and(path("/v1/environments/vm-1"))
        .respond_with(ResponseTemplate::new(403).set_body_json(json!({
            "error": {
                "code": "forbidden",
                "message": "keep it",
                "details": {"reason": "hitl_denied", "gate": "no-destroy", "intervention_id": "int_9"},
            },
        })))
        .mount(&server)
        .await;
    Mock::given(method("POST"))
        .and(path("/v1/environments/vm-1"))
        .and(query_param("action", "drain"))
        .respond_with(held(gated(
            "int_2",
            "lifecycle",
            lifecycle_payload("drain"),
            "pending",
            json!({}),
        )))
        .mount(&server)
        .await;
    Mock::given(method("GET"))
        .and(path("/v1/interventions/int_2"))
        .respond_with(ResponseTemplate::new(200).set_body_json(gated(
            "int_2",
            "lifecycle",
            lifecycle_payload("drain"),
            "rejected",
            json!({"decision": decision("reject", Some("not now"))}),
        )))
        .mount(&server)
        .await;

    let client = client(&server).await;
    let err = client.environments().destroy("vm-1").await.unwrap_err();
    assert!(err.is_hitl_denied());
    assert_eq!(err.forbidden_reason(), Some(ForbiddenReason::HitlDenied));
    let api = err.api().unwrap();
    assert_eq!(api.message, "keep it");
    assert_eq!(
        api.details.get("gate").map(String::as_str),
        Some("no-destroy")
    );

    let err = client.environments().drain("vm-1").await.unwrap_err();
    let Error::InterventionRejected(rejected) = &err else {
        panic!("err = {err}");
    };
    let reason = rejected.decision.as_ref().unwrap().reason.as_deref();
    assert_eq!(reason, Some("not now"));
    assert!(err.to_string().contains("not now"));
}

#[tokio::test]
async fn held_fork_returns_the_new_environment() {
    let server = MockServer::start().await;
    Mock::given(method("POST"))
        .and(path("/v1/environments/vm-1"))
        .and(query_param("action", "fork"))
        .respond_with(held(gated(
            "int_3",
            "lifecycle",
            lifecycle_payload("fork"),
            "pending",
            json!({}),
        )))
        .mount(&server)
        .await;
    Mock::given(method("GET"))
        .and(path("/v1/interventions/int_3"))
        .respond_with(ResponseTemplate::new(200).set_body_json(gated(
            "int_3",
            "lifecycle",
            lifecycle_payload("fork"),
            "approved",
            json!({
                "decision": decision("approve", None),
                "result": {"env_id": "vm-2", "finished_at": "2026-01-02T03:06:00Z"},
            }),
        )))
        .mount(&server)
        .await;
    Mock::given(method("GET"))
        .and(path("/v1/environments/vm-2"))
        .respond_with(ResponseTemplate::new(200).set_body_json(env_body("vm-2")))
        .expect(1)
        .mount(&server)
        .await;

    let env = client(&server)
        .await
        .environments()
        .fork("vm-1", ForkOptions::new())
        .await
        .unwrap();
    assert_eq!(env.id, "vm-2");
}

#[tokio::test]
async fn held_snapshot_whose_run_failed_is_held_action_failed() {
    let server = MockServer::start().await;
    Mock::given(method("POST"))
        .and(path("/v1/environments/vm-1/snapshots"))
        .respond_with(held(gated(
            "int_5",
            "lifecycle",
            lifecycle_payload("snapshot"),
            "pending",
            json!({}),
        )))
        .mount(&server)
        .await;
    Mock::given(method("GET"))
        .and(path("/v1/interventions/int_5"))
        .respond_with(ResponseTemplate::new(200).set_body_json(gated(
            "int_5",
            "lifecycle",
            lifecycle_payload("snapshot"),
            "approved",
            json!({
                "decision": decision("approve", None),
                "result": {"error": "disk full", "finished_at": "2026-01-02T03:06:00Z"},
            }),
        )))
        .mount(&server)
        .await;

    let err = client(&server)
        .await
        .snapshots()
        .create("vm-1", SnapshotRequest::new())
        .await
        .unwrap_err();
    assert!(matches!(err, Error::HeldActionFailed(_)), "err = {err}");
    assert!(err.to_string().contains("disk full"));
}

#[tokio::test]
async fn approve_with_sends_edited_argv_and_grant() {
    let server = MockServer::start().await;
    Mock::given(method("POST"))
        .and(path("/v1/interventions/int_1"))
        .and(query_param("action", "approve"))
        .and(body_json(json!({
            "edited_argv": ["git", "push"],
            "grant": "ttl:30m",
        })))
        .respond_with(ResponseTemplate::new(200).set_body_json(gated(
            "int_1",
            "exec",
            exec_payload(),
            "approved",
            json!({"decision": decision("approve", None)}),
        )))
        .expect(1)
        .mount(&server)
        .await;

    let approved = client(&server)
        .await
        .interventions()
        .approve_with(
            "int_1",
            ApproveOptions::new()
                .edited_argv(["git", "push"])
                .grant("ttl:30m"),
        )
        .await
        .unwrap();
    assert!(approved.is_approved());
    // approved but not run yet: terminal, not settled.
    assert!(approved.is_terminal());
    assert!(!approved.is_settled());
}

#[tokio::test]
async fn create_carries_hitl_gates_and_exec_policy() {
    let server = MockServer::start().await;
    Mock::given(method("POST"))
        .and(path("/v1/environments"))
        .and(body_partial_json(json!({
            "hitl": {
                "gates": [
                    {
                        "name": "force-push",
                        "on": "exec",
                        "match": {"program": ["git"], "args": ["push", "--force"]},
                        "grant": "env",
                    },
                    {
                        "name": "keep",
                        "on": "lifecycle",
                        "match": {"action": ["destroy", "rotate-token"]},
                        "mode": "deny",
                        "reason": "keep it",
                    },
                ],
                "exec": {"default": "ask", "allow": ["ls", "cat"]},
            },
        })))
        .respond_with(ResponseTemplate::new(200).set_body_json(env_body("vm-1")))
        .expect(1)
        .mount(&server)
        .await;

    let spec = HitlSpec::new()
        .gate(
            HitlGate::exec(
                "force-push",
                HitlGateMatch::new()
                    .program(["git"])
                    .args(["push", "--force"]),
            )
            .grant("env"),
        )
        .gate(
            HitlGate::lifecycle(
                "keep",
                [LifecycleAction::Destroy, LifecycleAction::RotateToken],
            )
            .mode(GateMode::Deny)
            .reason("keep it"),
        )
        .exec_policy(HitlExecPolicy::new(GateMode::Ask, ["ls", "cat"]));
    client(&server)
        .await
        .environments()
        .create(CreateRequest::new("t-1").hitl(spec))
        .await
        .unwrap();
}

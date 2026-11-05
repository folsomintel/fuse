// hitl computer gates, review requests and deciders against a mock
// orchestrator: a held computer action waits until fuse has performed it,
// opt-outs return the pending intervention, rejections surface as errors (or
// as is_error tool results), and a decider's allow counts as settled.

use std::sync::{Arc, Mutex};

use fuse::{
    ActorSource, ApproveOptions, Client, ComputerAction, Error, HitlKind, Intervention,
    ReviewFormat, ReviewRequest,
};
use serde_json::{json, Value};
use wiremock::matchers::{body_partial_json, method, path, query_param};
use wiremock::{Mock, MockServer, ResponseTemplate};

async fn client(server: &MockServer) -> Client {
    Client::new(server.uri(), "secret").unwrap()
}

// intervention builds an intervention of `kind` in `state`, with `extra`
// fields (decision, result) merged on top.
fn intervention(id: &str, kind: &str, payload: Value, state: &str, extra: Value) -> Value {
    let mut body = json!({
        "id": id,
        "env_id": "vm-1",
        "kind": kind,
        "gate": "typing",
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

fn decision(outcome: &str, reason: Option<&str>, source: &str) -> Value {
    let mut d = json!({
        "outcome": outcome,
        "decided_by": {"id": "master", "source": source},
        "decided_at": "2026-01-02T03:05:00Z",
    });
    if let Some(reason) = reason {
        d["reason"] = json!(reason);
    }
    d
}

fn computer_payload() -> Value {
    json!({"computer": {
        "action": "type",
        "text": "rm -rf ~",
        "input": {"action": "type", "text": "rm -rf ~"},
    }})
}

fn held_computer() -> ResponseTemplate {
    ResponseTemplate::new(202).set_body_json(json!({
        "intervention_id": "int_c",
        "intervention": intervention("int_c", "computer", computer_payload(), "pending", json!({})),
    }))
}

async fn mount_held_post(server: &MockServer) {
    Mock::given(method("POST"))
        .and(path("/v1/environments/vm-1/computer"))
        .respond_with(held_computer())
        .mount(server)
        .await;
}

#[tokio::test]
async fn computer_waits_on_held_action_and_returns_the_result() {
    let server = MockServer::start().await;
    mount_held_post(&server).await;
    // approved but not performed yet (not settled), then performed.
    let mut approved = decision("approve", None, "key");
    approved["edited_text"] = json!("ls ~");
    Mock::given(method("GET"))
        .and(path("/v1/interventions/int_c"))
        .respond_with(ResponseTemplate::new(200).set_body_json(intervention(
            "int_c",
            "computer",
            computer_payload(),
            "approved",
            json!({"decision": approved}),
        )))
        .up_to_n_times(1)
        .mount(&server)
        .await;
    Mock::given(method("GET"))
        .and(path("/v1/interventions/int_c"))
        .respond_with(ResponseTemplate::new(200).set_body_json(intervention(
            "int_c",
            "computer",
            computer_payload(),
            "approved",
            json!({
                "decision": approved,
                "result": {"computer": {"screenshot": "UE5H"}, "finished_at": "2026-01-02T03:06:00Z"},
            }),
        )))
        .mount(&server)
        .await;

    let seen = Arc::new(Mutex::new(String::new()));
    let seen_in = seen.clone();
    let result = client(&server)
        .await
        .environments()
        .computer(
            "vm-1",
            ComputerAction::type_text("rm -rf ~").on_held(move |held| {
                *seen_in.lock().unwrap() = held
                    .payload
                    .computer
                    .as_ref()
                    .and_then(|c| c.text.clone())
                    .unwrap_or_default();
            }),
        )
        .await
        .unwrap();
    assert_eq!(result.screenshot.as_deref(), Some("UE5H"));
    assert_eq!(*seen.lock().unwrap(), "rm -rf ~");

    let requests = server.received_requests().await.unwrap();
    let post = requests
        .iter()
        .find(|r| r.method.as_str() == "POST")
        .unwrap();
    let body: Value = serde_json::from_slice(&post.body).unwrap();
    assert_eq!(body, json!({"action": "type", "text": "rm -rf ~"}));
    let polls = requests
        .iter()
        .filter(|r| r.url.path() == "/v1/interventions/int_c")
        .count();
    assert_eq!(polls, 2);
}

#[tokio::test]
async fn computer_no_wait_and_rejection() {
    let server = MockServer::start().await;
    mount_held_post(&server).await;
    Mock::given(method("GET"))
        .and(path("/v1/interventions/int_c"))
        .respond_with(ResponseTemplate::new(200).set_body_json(intervention(
            "int_c",
            "computer",
            computer_payload(),
            "rejected",
            json!({"decision": decision("reject", Some("not that"), "key")}),
        )))
        .mount(&server)
        .await;
    let envs = client(&server).await.environments();

    let err = envs
        .computer("vm-1", ComputerAction::type_text("x").no_wait())
        .await
        .unwrap_err();
    let Error::InterventionPending(pending) = &err else {
        panic!("no_wait: err = {err}");
    };
    assert_eq!(pending.kind, HitlKind::Computer);

    let err = envs
        .computer("vm-1", ComputerAction::type_text("x"))
        .await
        .unwrap_err();
    let Error::InterventionRejected(rejected) = &err else {
        panic!("rejected: err = {err}");
    };
    assert_eq!(
        rejected.decision.as_ref().unwrap().reason.as_deref(),
        Some("not that")
    );

    // the tool helper hands a rejection to the model instead of failing.
    let out = envs
        .computer_tool_result("vm-1", &json!({"action": "type", "text": "x"}))
        .await
        .unwrap();
    assert!(out.is_error, "tool result = {out:?}");
}

#[tokio::test]
async fn computer_tool_result_turns_hitl_denied_into_tool_error() {
    let server = MockServer::start().await;
    Mock::given(method("POST"))
        .and(path("/v1/environments/vm-1/computer"))
        .respond_with(ResponseTemplate::new(403).set_body_json(json!({
            "error": {"code": "forbidden", "message": "no typing secrets", "details": {"reason": "hitl_denied", "gate": "typing"}},
        })))
        .mount(&server)
        .await;
    let out = client(&server)
        .await
        .environments()
        .computer_tool_result("vm-1", &json!({"action": "type", "text": "x"}))
        .await
        .unwrap();
    assert!(out.is_error, "tool result = {out:?}");
}

#[tokio::test]
async fn request_review_returns_comments() {
    let server = MockServer::start().await;
    let review = json!({"review": {"content": "diff --git a b", "format": "diff"}});
    Mock::given(method("POST"))
        .and(path("/v1/environments/vm-1"))
        .and(query_param("action", "request"))
        .and(body_partial_json(json!({
            "kind": "review",
            "content": "diff --git a b",
            "format": "diff",
        })))
        .respond_with(ResponseTemplate::new(201).set_body_json(intervention(
            "int_r",
            "review",
            review.clone(),
            "pending",
            json!({"gate": null, "mode": ""}),
        )))
        .expect(1)
        .mount(&server)
        .await;
    Mock::given(method("GET"))
        .and(path("/v1/interventions/int_r"))
        .respond_with(ResponseTemplate::new(200).set_body_json(intervention(
            "int_r",
            "review",
            review,
            "rejected",
            json!({"gate": null, "mode": "", "decision": decision("reject", Some("hunk 2 drops a check"), "key")}),
        )))
        .mount(&server)
        .await;

    let res = client(&server)
        .await
        .environments()
        .request_review(
            "vm-1",
            ReviewRequest::content("diff --git a b").format(ReviewFormat::Diff),
        )
        .await
        .unwrap();
    assert!(!res.approved);
    assert_eq!(res.comments.as_deref(), Some("hunk 2 drops a check"));
    assert_eq!(
        res.intervention.payload.review.unwrap().format,
        ReviewFormat::Diff
    );
}

#[tokio::test]
async fn approve_sends_edited_text() {
    let server = MockServer::start().await;
    let mut approved = decision("approve", None, "key");
    approved["edited_text"] = json!("ls");
    Mock::given(method("POST"))
        .and(path("/v1/interventions/int_c"))
        .and(query_param("action", "approve"))
        .and(body_partial_json(json!({"edited_text": "ls"})))
        .respond_with(ResponseTemplate::new(200).set_body_json(intervention(
            "int_c",
            "computer",
            computer_payload(),
            "approved",
            json!({"decision": approved}),
        )))
        .expect(1)
        .mount(&server)
        .await;
    let out = client(&server)
        .await
        .interventions()
        .approve_with("int_c", ApproveOptions::new().edited_text("ls"))
        .await
        .unwrap();
    assert_eq!(out.decision.unwrap().edited_text.as_deref(), Some("ls"));
}

#[test]
fn decider_allow_is_settled() {
    let mut body = intervention(
        "int_c",
        "computer",
        computer_payload(),
        "approved",
        json!({"decision": decision("approve", None, "decider")}),
    );
    let in_: Intervention = serde_json::from_value(body.clone()).unwrap();
    assert_eq!(
        in_.decision.as_ref().unwrap().decided_by.source,
        ActorSource::Decider
    );
    assert!(
        in_.is_settled(),
        "a decider allow has no result to wait for"
    );

    body["decision"] = decision("approve", None, "key");
    let in_: Intervention = serde_json::from_value(body).unwrap();
    assert!(
        !in_.is_settled(),
        "a human approval of a computer action waits for its result"
    );
}

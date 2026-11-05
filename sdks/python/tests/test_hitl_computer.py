from __future__ import annotations

import json

import httpx
import pytest
import respx

import fuse

BASE_URL = "https://fuse.test"

HELD_COMPUTER = {
    "intervention_id": "int_c",
    "intervention": {
        "id": "int_c",
        "env_id": "vm-1",
        "kind": "computer",
        "gate": "typing",
        "mode": "ask",
        "state": "pending",
        "payload": {
            "computer": {
                "action": "type",
                "text": "rm -rf ~",
                "input": {"action": "type", "text": "rm -rf ~"},
            }
        },
    },
}

COMPUTER_PAYLOAD = {"computer": {"action": "type"}}

REJECTED_COMPUTER = {
    "id": "int_c",
    "kind": "computer",
    "mode": "ask",
    "state": "rejected",
    "payload": COMPUTER_PAYLOAD,
    "decision": {"outcome": "reject", "reason": "not that"},
}


def new_client() -> fuse.Client:
    return fuse.Client(BASE_URL, "tok")


@respx.mock
def test_computer_waits_on_held_action_and_returns_the_result() -> None:
    respx.post(f"{BASE_URL}/v1/environments/vm-1/computer").mock(
        return_value=httpx.Response(202, json=HELD_COMPUTER)
    )
    decision = {"outcome": "approve", "edited_text": "ls ~"}
    poll_route = respx.get(f"{BASE_URL}/v1/interventions/int_c").mock(
        side_effect=[
            # approved, but not performed yet: not settled.
            httpx.Response(
                200,
                json={"id": "int_c", "kind": "computer", "mode": "ask", "state": "approved",
                      "payload": COMPUTER_PAYLOAD, "decision": decision},
            ),
            httpx.Response(
                200,
                json={"id": "int_c", "kind": "computer", "mode": "ask", "state": "approved",
                      "payload": COMPUTER_PAYLOAD, "decision": decision,
                      "result": {"computer": {"screenshot": "UE5H"}}},
            ),
        ]
    )
    held: list[str] = []

    def on_held(i: fuse.Intervention) -> None:
        assert i.payload.computer is not None
        held.append(i.payload.computer.text)

    with new_client() as client:
        res = client.environments.computer(
            "vm-1", fuse.ComputerAction(action="type", text="rm -rf ~"), on_held=on_held
        )

    assert res.screenshot == "UE5H"
    assert held == ["rm -rf ~"]
    assert poll_route.call_count == 2


@respx.mock
def test_computer_no_wait_raises_pending() -> None:
    respx.post(f"{BASE_URL}/v1/environments/vm-1/computer").mock(
        return_value=httpx.Response(202, json=HELD_COMPUTER)
    )
    poll_route = respx.get(f"{BASE_URL}/v1/interventions/int_c")
    with new_client() as client, pytest.raises(fuse.PendingInterventionError) as exc:
        client.environments.computer("vm-1", fuse.ComputerAction(action="type", text="x"),
                                     wait=False)
    assert exc.value.intervention.kind == fuse.HITL_KIND_COMPUTER
    assert exc.value.intervention.payload.computer is not None
    assert exc.value.intervention.payload.computer.input == {"action": "type", "text": "rm -rf ~"}
    assert poll_route.call_count == 0


@respx.mock
def test_computer_client_no_wait_raises_pending() -> None:
    respx.post(f"{BASE_URL}/v1/environments/vm-1/computer").mock(
        return_value=httpx.Response(202, json=HELD_COMPUTER)
    )
    with fuse.Client(BASE_URL, "tok", hitl_wait=False) as client, pytest.raises(
        fuse.PendingInterventionError
    ):
        client.environments.computer("vm-1", fuse.ComputerAction(action="type", text="x"))


@respx.mock
def test_computer_rejection_raises_and_tool_result_is_error() -> None:
    respx.post(f"{BASE_URL}/v1/environments/vm-1/computer").mock(
        return_value=httpx.Response(202, json=HELD_COMPUTER)
    )
    respx.get(f"{BASE_URL}/v1/interventions/int_c").mock(
        return_value=httpx.Response(200, json=REJECTED_COMPUTER)
    )
    with new_client() as client:
        with pytest.raises(fuse.InterventionRejectedError) as exc:
            client.environments.computer("vm-1", fuse.ComputerAction(action="type", text="x"))
        assert exc.value.intervention.decision is not None
        assert exc.value.intervention.decision.reason == "not that"

        # the tool helper hands a rejection to the model instead of raising.
        out = client.environments.computer_tool_result("vm-1", {"action": "type", "text": "x"})
    assert out.is_error
    assert "not that" in out.content[0]["text"]


@respx.mock
def test_computer_tool_result_turns_hitl_deny_into_is_error() -> None:
    respx.post(f"{BASE_URL}/v1/environments/vm-1/computer").mock(
        return_value=httpx.Response(
            403,
            json={"error": {"code": "forbidden", "message": "typing that is not allowed",
                            "details": {"reason": "hitl_denied", "gate": "typing"}}},
        )
    )
    with new_client() as client:
        out = client.environments.computer_tool_result("vm-1", {"action": "type", "text": "x"})
    assert out.is_error
    assert out.content[0]["text"] == "typing that is not allowed"


@respx.mock
def test_approve_sends_edited_text() -> None:
    route = respx.post(f"{BASE_URL}/v1/interventions/int_c").mock(
        return_value=httpx.Response(
            200,
            json={"id": "int_c", "state": "approved",
                  "decision": {"outcome": "approve", "edited_text": "ls"}},
        )
    )
    with new_client() as client:
        got = client.interventions.approve("int_c", edited_text="ls")

    req = route.calls.last.request
    assert req.url.params["action"] == "approve"
    assert json.loads(req.content) == {"edited_text": "ls"}
    assert got.decision is not None
    assert got.decision.edited_text == "ls"


@respx.mock
def test_request_review_returns_comments() -> None:
    review = {"review": {"content": "diff --git a b", "format": "diff"}}
    route = respx.post(f"{BASE_URL}/v1/environments/vm-1").mock(
        return_value=httpx.Response(
            201, json={"id": "int_r", "kind": "review", "state": "pending", "payload": review}
        )
    )
    respx.get(f"{BASE_URL}/v1/interventions/int_r").mock(
        return_value=httpx.Response(
            200,
            json={"id": "int_r", "kind": "review", "state": "rejected", "payload": review,
                  "decision": {"outcome": "reject", "reason": "hunk 2 drops a check"}},
        )
    )
    with new_client() as client:
        res = client.environments.request_review(
            "vm-1", content="diff --git a b", format=fuse.REVIEW_FORMAT_DIFF
        )

    req = route.calls.last.request
    assert req.url.params["action"] == "request"
    assert json.loads(req.content) == {
        "kind": "review",
        "content": "diff --git a b",
        "format": "diff",
    }
    assert not res.approved
    assert res.comments == "hunk 2 drops a check"
    assert res.intervention.payload.review is not None
    assert res.intervention.payload.review.format == "diff"


def test_decider_allow_is_settled() -> None:
    intervention = fuse.Intervention(
        kind=fuse.HITL_KIND_COMPUTER,
        mode=fuse.GATE_MODE_ASK,
        state=fuse.INTERVENTION_APPROVED,
        payload=fuse.InterventionPayload(computer=fuse.ComputerPayload(action="type")),
        decision=fuse.InterventionDecision(
            outcome=fuse.OUTCOME_APPROVE,
            decided_by=fuse.Actor(id="decider:typing", source=fuse.ACTOR_SOURCE_DECIDER),
        ),
    )
    # a decider allow has no result to wait for.
    assert intervention.settled
    assert intervention.decision is not None
    intervention.decision.decided_by = fuse.Actor(id="alice", source="key")
    # a human approval of a computer action waits for its result.
    assert not intervention.settled


def test_new_wire_fields_parse() -> None:
    gate = fuse.HitlGate.model_validate(
        {"name": "typing", "on": "computer",
         "match": {"action": ["type"], "text_contains": ["rm -rf"]},
         "decide": {"url": "https://d.test", "secret": "dsec"}}
    )
    assert gate.match.text_contains == ["rm -rf"]
    assert gate.decide is not None and gate.decide.secret == "dsec"
    requests = fuse.HitlRequests(review=fuse.HitlReviewRequests())
    assert requests.model_dump(exclude_none=True) == {"review": {}}
    payload = fuse.InterventionPayload.model_validate(
        {"decider": {"verdict": "ask", "reason": "unsure"}}
    )
    assert payload.decider is not None and payload.decider.verdict == fuse.DECIDER_ASK

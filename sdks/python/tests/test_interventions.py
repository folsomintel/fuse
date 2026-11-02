from __future__ import annotations

import json

import httpx
import respx

import fuse

BASE_URL = "https://fuse.test"

DECIDED = {
    "id": "int_1",
    "env_id": "vm-1",
    "kind": "ask",
    "state": "approved",
    "payload": {"question": "deploy?", "choices": ["yes", "no"]},
    "triggered_by": {"id": "key:k1", "source": "key", "key_id": "k1"},
    "deadline": "2026-10-29T12:10:00Z",
    "on_timeout": "reject",
    "decision": {
        "outcome": "approve",
        "answer": "yes",
        "decided_by": {"id": "master", "source": "key"},
        "decided_at": "2026-10-29T12:01:00Z",
    },
    "created_at": "2026-10-29T12:00:00Z",
    "updated_at": "2026-10-29T12:01:00Z",
}


def new_client() -> fuse.Client:
    return fuse.Client(BASE_URL, "tok")


@respx.mock
def test_request_human_raises_then_polls_until_decided() -> None:
    raise_route = respx.post(f"{BASE_URL}/v1/environments/vm-1").mock(
        return_value=httpx.Response(
            201,
            json={"id": "int_1", "env_id": "vm-1", "kind": "ask", "state": "pending"},
        )
    )
    # pending on the first poll, decided on the second.
    poll_route = respx.get(f"{BASE_URL}/v1/interventions/int_1").mock(
        side_effect=[
            httpx.Response(200, json={"id": "int_1", "state": "pending"}),
            httpx.Response(200, json=DECIDED),
        ]
    )
    with new_client() as client:
        got = client.environments.request_human(
            "vm-1", "deploy?", choices=["yes", "no"], timeout_seconds=60
        )

    req = raise_route.calls.last.request
    assert req.url.params["action"] == "request"
    assert json.loads(req.content) == {
        "kind": "ask",
        "question": "deploy?",
        "choices": ["yes", "no"],
        "timeout_seconds": 60,
    }
    assert poll_route.call_count == 2
    for call in poll_route.calls:
        assert call.request.url.params["wait"] == "30s"
    assert got.terminal
    assert got.approved
    assert got.decision is not None
    assert got.decision.answer == "yes"
    assert got.decision.decided_by.id == "master"
    assert got.triggered_by.key_id == "k1"
    assert got.payload.choices == ["yes", "no"]


@respx.mock
def test_request_human_no_wait_does_not_poll() -> None:
    raise_route = respx.post(f"{BASE_URL}/v1/environments/vm-1").mock(
        return_value=httpx.Response(201, json={"id": "int_1", "state": "pending"})
    )
    poll_route = respx.get(f"{BASE_URL}/v1/interventions/int_1").mock(
        return_value=httpx.Response(200, json=DECIDED)
    )
    with new_client() as client:
        got = client.environments.request_human("vm-1", "q", wait=False)

    # unset choices and timeout are omitted from the wire.
    assert json.loads(raise_route.calls.last.request.content) == {
        "kind": "ask",
        "question": "q",
    }
    assert not poll_route.called
    assert got.state == fuse.INTERVENTION_PENDING
    assert not got.terminal


@respx.mock
def test_interventions_get_does_not_wait() -> None:
    route = respx.get(f"{BASE_URL}/v1/interventions/int_1").mock(
        return_value=httpx.Response(200, json={"id": "int_1", "state": "pending"})
    )
    with new_client() as client:
        got = client.interventions.get("int_1")

    assert "wait" not in route.calls.last.request.url.params
    assert got.state == "pending"


@respx.mock
def test_interventions_approve_and_reject() -> None:
    route = respx.post(f"{BASE_URL}/v1/interventions/int_1").mock(
        return_value=httpx.Response(200, json=DECIDED)
    )
    with new_client() as client:
        client.interventions.approve("int_1", "yes")
        client.interventions.approve("int_1", "yes", reason="looks fine")
        client.interventions.reject("int_1", reason="too risky")
        client.interventions.reject("int_1")

    calls = [
        (c.request.url.params["action"], json.loads(c.request.content))
        for c in route.calls
    ]
    assert calls == [
        ("approve", {"answer": "yes"}),
        ("approve", {"answer": "yes", "reason": "looks fine"}),
        ("reject", {"reason": "too risky"}),
        ("reject", {}),
    ]


@respx.mock
def test_interventions_list_walks_pages() -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        assert request.url.params["env"] == "vm-1"
        assert request.url.params["state"] == "pending"
        assert request.url.params["limit"] == "200"
        if "cursor" not in request.url.params:
            return httpx.Response(
                200, json={"interventions": [{"id": "int_1"}], "next_cursor": "c2"}
            )
        assert request.url.params["cursor"] == "c2"
        return httpx.Response(200, json={"interventions": [{"id": "int_2"}]})

    route = respx.get(f"{BASE_URL}/v1/interventions").mock(side_effect=handler)
    with new_client() as client:
        got = client.interventions.list(env_id="vm-1", state="pending")

    assert [i.id for i in got] == ["int_1", "int_2"]
    assert route.call_count == 2


@respx.mock
def test_interventions_list_page_omits_unset_filters() -> None:
    route = respx.get(f"{BASE_URL}/v1/interventions").mock(
        return_value=httpx.Response(200, json={"interventions": []})
    )
    with new_client() as client:
        page = client.interventions.list_page()

    assert dict(route.calls.last.request.url.params) == {}
    assert page.interventions == []
    assert page.next_cursor is None


@respx.mock
def test_environments_create_carries_hitl_and_omits_it_when_unset() -> None:
    route = respx.post(f"{BASE_URL}/v1/environments").mock(
        return_value=httpx.Response(
            200,
            json={"id": "vm-1", "state": "running", "task_id": "task-1", "url": "u"},
        )
    )
    with new_client() as client:
        client.environments.create(
            fuse.CreateRequest(
                task_id="task-1",
                hitl=fuse.HitlSpec(
                    timeout_seconds=300,
                    on_timeout="allow",
                    requests=fuse.HitlRequests(ask=fuse.HitlAskRequests()),
                ),
            )
        )
        client.environments.create(fuse.CreateRequest(task_id="task-1"))

    body = json.loads(route.calls[0].request.content)
    assert body["hitl"] == {
        "timeout_seconds": 300,
        "on_timeout": "allow",
        "requests": {"ask": {}},
    }
    assert "hitl" not in json.loads(route.calls[1].request.content)

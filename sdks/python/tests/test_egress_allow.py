from __future__ import annotations

import json

import httpx
import pytest
import respx

import fuse

BASE_URL = "https://fuse.test"
ALLOW_URL = f"{BASE_URL}/v1/environments/vm-1/egress/allow"


@respx.mock
def test_set_egress_allow_sends_list_and_decodes_status() -> None:
    route = respx.put(ALLOW_URL).mock(
        side_effect=[
            httpx.Response(200, json={"mode": "direct", "allow": ["10.0.0.0/8", "1.2.3.4/32"]}),
            httpx.Response(200, json={"mode": "direct"}),
        ]
    )
    with fuse.Client(BASE_URL, "tok") as client:
        st = client.environments.set_egress_allow("vm-1", ["10.0.0.0/8", "1.2.3.4"])
        assert st.mode == "direct"
        assert st.allow == ["10.0.0.0/8", "1.2.3.4/32"]
        # an empty list lifts the restriction and must still send the key.
        lifted = client.environments.set_egress_allow("vm-1", [])
        assert lifted.allow is None

    bodies = [json.loads(c.request.content) for c in route.calls]
    assert bodies == [{"allow": ["10.0.0.0/8", "1.2.3.4"]}, {"allow": []}]
    assert route.calls[0].request.headers["Idempotency-Key"]


@respx.mock
def test_set_egress_allow_follows_accepted_operation() -> None:
    op = {
        "id": "op_1",
        "kind": "egress.allow",
        "state": "running",
        "started_at": "2026-11-15T00:00:00Z",
    }
    respx.put(ALLOW_URL).mock(
        return_value=httpx.Response(202, json={"operation_id": "op_1", "operation": op})
    )
    done = dict(
        op,
        state="succeeded",
        status_code=200,
        result={"mode": "direct", "allow": ["10.0.0.0/8"]},
    )
    respx.get(f"{BASE_URL}/v1/operations/op_1").mock(return_value=httpx.Response(200, json=done))
    with fuse.Client(BASE_URL, "tok") as client:
        st = client.environments.set_egress_allow("vm-1", ["10.0.0.0/8"])
    assert st.allow == ["10.0.0.0/8"]


@respx.mock
def test_set_egress_allow_conflict_raises() -> None:
    respx.put(ALLOW_URL).mock(
        return_value=httpx.Response(
            409, json={"error": {"code": "conflict", "message": "environment uses proxy egress"}}
        )
    )
    with fuse.Client(BASE_URL, "tok") as client:
        with pytest.raises(fuse.ApiError) as exc:
            client.environments.set_egress_allow("vm-1", ["10.0.0.0/8"])
    assert fuse.is_conflict(exc.value)


def test_set_egress_allow_refuses_a_bare_string() -> None:
    with fuse.Client(BASE_URL, "tok") as client:
        with pytest.raises(TypeError):
            client.environments.set_egress_allow("vm-1", "10.0.0.0/8")


@respx.mock
def test_create_sends_egress_allow() -> None:
    route = respx.post(f"{BASE_URL}/v1/environments").mock(
        return_value=httpx.Response(201, json={"id": "vm-1", "state": "running"})
    )
    with fuse.Client(BASE_URL, "tok") as client:
        client.environments.create(
            fuse.CreateRequest(
                task_id="t", egress=fuse.EgressSpec(mode="direct", allow=["10.0.0.0/8"])
            )
        )
    body = json.loads(route.calls.last.request.content)
    assert body["egress"] == {"mode": "direct", "allow": ["10.0.0.0/8"]}

from __future__ import annotations

import json
from datetime import datetime, timezone

import httpx
import pytest
import respx

import fuse

BASE_URL = "https://fuse.test"


def new_client() -> fuse.Client:
    return fuse.Client(BASE_URL, "tok")


@respx.mock
def test_with_actor_sends_headers_and_original_does_not() -> None:
    route = respx.get(f"{BASE_URL}/v1/interventions").mock(
        return_value=httpx.Response(200, json={"interventions": []})
    )
    with new_client() as client:
        alice = client.with_actor(fuse.ActorLabel(id="u_alice", name="Alice"))
        alice.interventions.list_page()
        client.interventions.list_page()
        # closing the copy leaves the shared connections usable.
        alice.close()
        client.interventions.list_page()

    first, second, third = (c.request.headers for c in route.calls)
    assert first["Fuse-Actor"] == "u_alice"
    assert first["Fuse-Actor-Name"] == "Alice"
    assert first["Authorization"] == "Bearer tok"
    assert "Fuse-Actor" not in second
    assert "Fuse-Actor-Name" not in second
    assert "Fuse-Actor" not in third


@respx.mock
def test_client_actor_option_sends_headers() -> None:
    route = respx.get(f"{BASE_URL}/v1/interventions").mock(
        return_value=httpx.Response(200, json={"interventions": []})
    )
    with fuse.Client(BASE_URL, "tok", actor=fuse.ActorLabel(id="u_bob")) as client:
        client.interventions.list_page()

    headers = route.calls.last.request.headers
    assert headers["Fuse-Actor"] == "u_bob"
    # the name is optional and omitted when unset.
    assert "Fuse-Actor-Name" not in headers


def test_actor_id_is_validated() -> None:
    with pytest.raises(ValueError):
        fuse.Client(BASE_URL, actor=fuse.ActorLabel(id=""))
    with new_client() as client, pytest.raises(ValueError):
        client.with_actor(fuse.ActorLabel(id="x" * 129))


@respx.mock
def test_scoped_api_key_create_sends_options_and_parses_fields() -> None:
    route = respx.post(f"{BASE_URL}/v1/api-keys").mock(
        return_value=httpx.Response(
            201,
            json={
                "id": "k2",
                "key": "fuse_secret",
                "label": "reviewer",
                "created_at": "2026-10-29T12:00:00Z",
                "scopes": ["hitl:read", "hitl:decide"],
                "env_id": "vm-1",
                "expires_at": "2026-10-29T13:00:00Z",
                "actor": {"id": "u_alice", "name": "Alice"},
                "created_by": "master",
            },
        )
    )
    with new_client() as client:
        got = client.api_keys.create(
            "reviewer",
            scopes=[fuse.SCOPE_HITL_READ, fuse.SCOPE_HITL_DECIDE],
            env_id="vm-1",
            ttl_seconds=3600,
            actor=fuse.ActorLabel(id="u_alice", name="Alice"),
        )
        client.api_keys.create("plain")

    assert json.loads(route.calls[0].request.content) == {
        "label": "reviewer",
        "scopes": ["hitl:read", "hitl:decide"],
        "env_id": "vm-1",
        "ttl_seconds": 3600,
        "actor": {"id": "u_alice", "name": "Alice"},
    }
    # the plain form still sends only the label.
    assert json.loads(route.calls[1].request.content) == {"label": "plain"}
    assert got.key == "fuse_secret"
    assert got.scopes == ["hitl:read", "hitl:decide"]
    assert got.env_id == "vm-1"
    assert got.expires_at == datetime(2026, 10, 29, 13, 0, tzinfo=timezone.utc)
    assert got.actor is not None
    assert got.actor.id == "u_alice"
    assert got.actor.name == "Alice"
    assert got.created_by == "master"


@respx.mock
def test_api_key_list_full_access_key_has_no_scopes() -> None:
    respx.get(f"{BASE_URL}/v1/api-keys").mock(
        return_value=httpx.Response(200, json={"api_keys": [{"id": "k1"}]})
    )
    with new_client() as client:
        keys = client.api_keys.list()

    assert keys[0].scopes is None
    assert keys[0].actor is None
    assert keys[0].expires_at is None


@respx.mock
def test_interventions_list_sends_actor_filter() -> None:
    route = respx.get(f"{BASE_URL}/v1/interventions").mock(
        return_value=httpx.Response(200, json={"interventions": []})
    )
    with new_client() as client:
        client.interventions.list(actor="u_alice")

    assert route.calls.last.request.url.params["actor"] == "u_alice"


@respx.mock
def test_audit_page_sends_filters_and_parses_entries() -> None:
    route = respx.get(f"{BASE_URL}/v1/hitl/audit").mock(
        return_value=httpx.Response(
            200,
            json={
                "entries": [
                    {
                        "seq": 7,
                        "event": "decided",
                        "intervention_id": "int_1",
                        "env_id": "vm-1",
                        "actor": {
                            "id": "u_alice",
                            "name": "Alice",
                            "source": "header",
                            "key_id": "k2",
                        },
                        "remote_addr": "10.0.0.1:5000",
                        "at": "2026-10-29T12:01:00Z",
                        "prev_hash": "aa",
                        "hash": "bb",
                    }
                ],
                "next_cursor": "c2",
            },
        )
    )
    with new_client() as client:
        page = client.interventions.audit_page(
            env_id="vm-1", actor="u_alice", since="24h", limit=10, cursor="c1"
        )

    assert dict(route.calls.last.request.url.params) == {
        "env": "vm-1",
        "actor": "u_alice",
        "since": "24h",
        "limit": "10",
        "cursor": "c1",
    }
    assert page.next_cursor == "c2"
    entry = page.entries[0]
    assert entry.seq == 7
    assert entry.event == fuse.AUDIT_DECIDED
    assert entry.intervention_id == "int_1"
    assert entry.env_id == "vm-1"
    assert entry.actor.id == "u_alice"
    assert entry.actor.key_id == "k2"
    assert entry.remote_addr == "10.0.0.1:5000"
    assert entry.at == datetime(2026, 10, 29, 12, 1, tzinfo=timezone.utc)
    assert entry.prev_hash == "aa"
    assert entry.hash == "bb"


@respx.mock
def test_audit_walks_pages() -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        assert request.url.params["limit"] == "200"
        if "cursor" not in request.url.params:
            return httpx.Response(200, json={"entries": [{"seq": 1}], "next_cursor": "c2"})
        assert request.url.params["cursor"] == "c2"
        return httpx.Response(200, json={"entries": [{"seq": 2}]})

    route = respx.get(f"{BASE_URL}/v1/hitl/audit").mock(side_effect=handler)
    with new_client() as client:
        got = client.interventions.audit(env_id="vm-1")

    assert [e.seq for e in got] == [1, 2]
    assert route.call_count == 2


@respx.mock
def test_forbidden_self_approval_is_typed() -> None:
    respx.post(f"{BASE_URL}/v1/interventions/int_1").mock(
        return_value=httpx.Response(
            403,
            json={
                "error": {
                    "code": "forbidden",
                    "message": "the actor that raised this intervention cannot decide it",
                    "details": {"reason": "self_approval"},
                }
            },
        )
    )
    with new_client() as client, pytest.raises(fuse.ForbiddenError) as excinfo:
        client.interventions.approve("int_1", "yes")

    err = excinfo.value
    assert isinstance(err, fuse.ApiError)
    assert err.status == 403
    assert err.reason == fuse.FORBIDDEN_SELF_APPROVAL
    assert err.scope == ""
    assert fuse.is_forbidden(err)


@respx.mock
def test_forbidden_missing_scope_carries_scope() -> None:
    respx.get(f"{BASE_URL}/v1/hitl/audit").mock(
        return_value=httpx.Response(
            403,
            json={
                "error": {
                    "code": "forbidden",
                    "message": "this request needs the hitl:read scope",
                    "details": {"reason": "missing_scope", "scope": "hitl:read"},
                }
            },
        )
    )
    with new_client() as client, pytest.raises(fuse.ForbiddenError) as excinfo:
        client.interventions.audit_page()

    assert excinfo.value.reason == fuse.FORBIDDEN_MISSING_SCOPE
    assert excinfo.value.scope == fuse.SCOPE_HITL_READ


@respx.mock
def test_legacy_unauthorized_403_stays_plain_api_error() -> None:
    respx.get(f"{BASE_URL}/v1/api-keys").mock(
        return_value=httpx.Response(
            403, json={"error": {"code": "unauthorized", "message": "master token required"}}
        )
    )
    with new_client() as client, pytest.raises(fuse.ApiError) as excinfo:
        client.api_keys.list()

    assert not isinstance(excinfo.value, fuse.ForbiddenError)
    assert fuse.is_unauthorized(excinfo.value)


@respx.mock
def test_environments_create_serializes_hitl_notify_and_require_actor() -> None:
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
                secrets={"HOOK_KEY": "s3cret"},
                hitl=fuse.HitlSpec(
                    require_actor=True,
                    notify=[fuse.HitlWebhook(url="https://hooks.test/x", secret="HOOK_KEY")],
                    requests=fuse.HitlRequests(ask=fuse.HitlAskRequests()),
                ),
            )
        )

    body = json.loads(route.calls.last.request.content)
    assert body["hitl"] == {
        "requests": {"ask": {}},
        "require_actor": True,
        "notify": [{"url": "https://hooks.test/x", "secret": "HOOK_KEY"}],
    }

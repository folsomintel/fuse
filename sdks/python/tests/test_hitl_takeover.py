from __future__ import annotations

import json

import httpx
import pytest
import respx

import fuse

BASE_URL = "https://fuse.test"

LOCKED = {
    "error": {
        "code": "locked",
        "message": "a human holds this environment",
        "details": {"intervention_id": "int_t", "kind": "takeover"},
    }
}

HELD = {
    "id": "int_t",
    "env_id": "vm-1",
    "kind": "takeover",
    "state": "pending",
    "payload": {
        "takeover": {
            "via": "terminal",
            "reason": "fix the build",
            "holder": {"id": "master", "source": "key"},
            "started_at": "2026-11-01T12:00:00Z",
        }
    },
}

RELEASED = {
    "id": "int_t",
    "env_id": "vm-1",
    "kind": "takeover",
    "state": "approved",
    "payload": {"takeover": {"via": "terminal"}},
    "decision": {
        "outcome": "approve",
        "answer": "fixed the lockfile",
        "decided_by": {"id": "master", "source": "key"},
        "decided_at": "2026-11-01T12:05:00Z",
    },
}

EXEC_OK = {"exit_code": 0, "stdout": "aGk=", "stderr": "", "duration_ms": 3}


def new_client() -> fuse.Client:
    return fuse.Client(BASE_URL, "tok")


@respx.mock
def test_secret_request_and_answer() -> None:
    raise_route = respx.post(f"{BASE_URL}/v1/environments/vm-1").mock(
        return_value=httpx.Response(
            201,
            json={
                "id": "int_s",
                "kind": "secret",
                "state": "pending",
                "payload": {
                    "secret": {"target": "GH_TOKEN", "label": "github token", "message": "push"}
                },
            },
        )
    )
    answer_route = respx.post(f"{BASE_URL}/v1/interventions/int_s").mock(
        return_value=httpx.Response(
            200,
            json={
                "id": "int_s",
                "kind": "secret",
                "state": "approved",
                "decision": {"outcome": "approve", "answer": "done"},
            },
        )
    )
    with new_client() as client:
        pending = client.environments.request_human(
            "vm-1", kind=fuse.HITL_KIND_SECRET, target="GH_TOKEN", message="push", wait=False
        )
        answered = client.interventions.answer_secret("int_s", "ghp_value")

    assert json.loads(raise_route.calls.last.request.content) == {
        "kind": "secret",
        "target": "GH_TOKEN",
        "message": "push",
    }
    assert pending.payload.secret is not None
    assert pending.payload.secret.label == "github token"
    req = answer_route.calls.last.request
    assert req.url.params["action"] == "secret"
    assert json.loads(req.content) == {"value": "ghp_value"}
    assert answered.decision is not None
    assert answered.decision.answer == "done"


@respx.mock
def test_takeover_and_release() -> None:
    route = respx.post(f"{BASE_URL}/v1/environments/vm-1").mock(
        side_effect=[httpx.Response(200, json=HELD), httpx.Response(200, json=RELEASED)]
    )
    with new_client() as client:
        held = client.environments.takeover("vm-1", via="terminal", reason="fix the build")
        released = client.environments.release("vm-1", note="fixed the lockfile")

    calls = [(c.request.url.params["action"], json.loads(c.request.content)) for c in route.calls]
    assert calls == [
        ("takeover", {"via": "terminal", "reason": "fix the build"}),
        ("release", {"note": "fixed the lockfile"}),
    ]
    assert held.payload.takeover is not None
    assert held.payload.takeover.holder is not None
    assert held.payload.takeover.holder.id == "master"
    assert released.approved
    assert released.decision is not None
    assert released.decision.answer == "fixed the lockfile"


@respx.mock
def test_takeover_request_by_agent_omits_unset_fields() -> None:
    route = respx.post(f"{BASE_URL}/v1/environments/vm-1").mock(
        return_value=httpx.Response(201, json={"id": "int_t", "state": "pending"})
    )
    with new_client() as client:
        client.environments.request_human(
            "vm-1", kind=fuse.HITL_KIND_TAKEOVER, reason="stuck on captcha", wait=False
        )
        client.environments.takeover("vm-1")
        client.environments.release("vm-1")

    bodies = [json.loads(c.request.content) for c in route.calls]
    assert bodies == [{"kind": "takeover", "reason": "stuck on captcha"}, {}, {}]


@respx.mock
def test_exec_waits_out_a_takeover_then_retries() -> None:
    exec_route = respx.post(f"{BASE_URL}/v1/environments/vm-1").mock(
        side_effect=[httpx.Response(423, json=LOCKED), httpx.Response(200, json=EXEC_OK)]
    )
    poll_route = respx.get(f"{BASE_URL}/v1/interventions/int_t").mock(
        side_effect=[httpx.Response(200, json=HELD), httpx.Response(200, json=RELEASED)]
    )
    with new_client() as client:
        got = client.environments.exec("vm-1", fuse.ExecRequest(cmd=["make"]))

    assert got.exit_code == 0
    assert exec_route.call_count == 2
    for call in exec_route.calls:
        assert call.request.url.params["action"] == "exec"
        assert json.loads(call.request.content)["cmd"] == ["make"]
    assert poll_route.call_count == 2
    for call in poll_route.calls:
        assert "wait" in call.request.url.params


@respx.mock
def test_computer_waits_out_a_takeover_then_retries() -> None:
    route = respx.post(f"{BASE_URL}/v1/environments/vm-1/computer").mock(
        side_effect=[
            httpx.Response(423, json=LOCKED),
            httpx.Response(200, json={"screenshot": "cG5n"}),
        ]
    )
    respx.get(f"{BASE_URL}/v1/interventions/int_t").mock(
        return_value=httpx.Response(200, json=RELEASED)
    )
    with new_client() as client:
        got = client.environments.computer("vm-1", fuse.ComputerAction(action="screenshot"))

    assert route.call_count == 2
    assert got.screenshot == "cG5n"


@respx.mock
def test_locked_error_when_not_waiting() -> None:
    exec_route = respx.post(f"{BASE_URL}/v1/environments/vm-1").mock(
        return_value=httpx.Response(423, json=LOCKED)
    )
    computer_route = respx.post(f"{BASE_URL}/v1/environments/vm-1/computer").mock(
        return_value=httpx.Response(423, json=LOCKED)
    )
    poll_route = respx.get(f"{BASE_URL}/v1/interventions/int_t").mock(
        return_value=httpx.Response(200, json=RELEASED)
    )
    with new_client() as client:
        with pytest.raises(fuse.LockedError) as exc:
            client.environments.exec("vm-1", fuse.ExecRequest(cmd=["make"]), wait=False)
        with pytest.raises(fuse.LockedError):
            client.environments.computer(
                "vm-1", fuse.ComputerAction(action="screenshot"), wait=False
            )

    err = exc.value
    assert err.status == 423
    assert err.intervention_id == "int_t"
    assert err.kind == "takeover"
    assert fuse.is_locked(err)
    assert exec_route.call_count == 1
    assert computer_route.call_count == 1
    assert not poll_route.called


@respx.mock
def test_approve_expiry_extends_and_recording_is_text() -> None:
    decide = respx.post(f"{BASE_URL}/v1/interventions/int_e").mock(
        return_value=httpx.Response(200, json={"id": "int_e", "state": "approved"})
    )
    cast = '{"version": 2, "width": 80, "height": 24}\n[0.1, "o", "$ "]\n'
    rec = respx.get(f"{BASE_URL}/v1/hitl/recordings/rec_1").mock(
        return_value=httpx.Response(
            200, text=cast, headers={"Content-Type": "application/x-asciicast"}
        )
    )
    with new_client() as client:
        client.interventions.approve("int_e", extend_seconds=1800)
        client.interventions.approve("int_e")
        got = client.interventions.get_recording("rec_1")

    bodies = [json.loads(c.request.content) for c in decide.calls]
    assert bodies == [{"extend_seconds": 1800}, {}]
    assert rec.called
    assert got == cast


def test_new_wire_fields_parse() -> None:
    expiry = fuse.Intervention.model_validate(
        {
            "id": "int_e",
            "kind": "expiry",
            "payload": {
                "signal": {
                    "reaper": "idle_timeout",
                    "reap_at": "2026-11-01T13:00:00Z",
                    "extended_until": "2026-11-01T13:30:00Z",
                }
            },
        }
    )
    assert expiry.payload.signal is not None
    assert expiry.payload.signal.reaper == fuse.REAPER_IDLE_TIMEOUT
    assert expiry.payload.signal.extended_until is not None

    entry = fuse.AuditEntry.model_validate({"event": "takeover_session", "recording_id": "rec_1"})
    assert entry.event == fuse.AUDIT_TAKEOVER_SESSION
    assert entry.recording_id == "rec_1"

    spec = fuse.HitlSpec(
        requests=fuse.HitlRequests(
            secret=fuse.HitlSecretRequests(
                targets=[
                    fuse.HitlSecretTarget(env="GH_TOKEN", label="github token"),
                    fuse.HitlSecretTarget(file="/root/.netrc", label="netrc"),
                ]
            ),
            takeover=fuse.HitlTakeoverRequests(via=["terminal"]),
        )
    )
    assert spec.model_dump(by_alias=True, exclude_none=True)["requests"] == {
        "secret": {
            "targets": [
                {"env": "GH_TOKEN", "label": "github token"},
                {"file": "/root/.netrc", "label": "netrc"},
            ]
        },
        "takeover": {"via": ["terminal"]},
    }

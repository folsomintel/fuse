from __future__ import annotations

import json

import httpx
import pytest
import respx

import fuse

BASE_URL = "https://fuse.test"

HELD_EXEC = {
    "intervention_id": "int_1",
    "intervention": {
        "id": "int_1",
        "env_id": "vm-1",
        "kind": "exec",
        "gate": "force-push",
        "mode": "ask",
        "state": "pending",
        "payload": {"exec": {"argv": ["git", "push", "--force"]}},
    },
}


def new_client() -> fuse.Client:
    return fuse.Client(BASE_URL, "tok")


@respx.mock
def test_exec_waits_on_held_intervention() -> None:
    respx.post(f"{BASE_URL}/v1/environments/vm-1").mock(
        return_value=httpx.Response(202, json=HELD_EXEC)
    )
    exec_payload = {"exec": {"argv": ["git"]}}
    poll_route = respx.get(f"{BASE_URL}/v1/interventions/int_1").mock(
        side_effect=[
            httpx.Response(
                200,
                json={"id": "int_1", "kind": "exec", "mode": "ask", "state": "pending",
                      "payload": exec_payload},
            ),
            # approved, but not run yet: not settled.
            httpx.Response(
                200,
                json={"id": "int_1", "kind": "exec", "mode": "ask", "state": "approved",
                      "payload": exec_payload,
                      "decision": {"outcome": "approve",
                                   "edited_argv": ["git", "push", "--force-with-lease"]}},
            ),
            httpx.Response(
                200,
                json={"id": "int_1", "kind": "exec", "mode": "ask", "state": "approved",
                      "payload": exec_payload, "decision": {"outcome": "approve"},
                      "result": {"exec": {"exit_code": 0, "stdout": "pushed", "stderr": ""}}},
            ),
        ]
    )
    held: list[str] = []
    with new_client() as client:
        res = client.environments.exec(
            "vm-1",
            fuse.ExecRequest(cmd=["git", "push", "--force"]),
            on_held=lambda i: held.append(i.id),
        )

    assert res.stdout == "pushed"
    assert held == ["int_1"]
    assert poll_route.call_count == 3


@respx.mock
def test_exec_no_wait_raises_pending_without_polling() -> None:
    respx.post(f"{BASE_URL}/v1/environments/vm-1").mock(
        return_value=httpx.Response(202, json=HELD_EXEC)
    )
    poll_route = respx.get(f"{BASE_URL}/v1/interventions/int_1")
    with new_client() as client, pytest.raises(fuse.PendingInterventionError) as exc:
        client.environments.exec("vm-1", fuse.ExecRequest(cmd=["git"]), wait=False)

    assert not poll_route.called
    assert exc.value.intervention.id == "int_1"
    assert exc.value.intervention.gate == "force-push"
    assert exc.value.intervention.payload.exec is not None
    assert exc.value.intervention.payload.exec.argv == ["git", "push", "--force"]


@respx.mock
def test_client_hitl_options_apply_to_every_gated_call() -> None:
    respx.post(f"{BASE_URL}/v1/environments/vm-1").mock(
        return_value=httpx.Response(202, json=HELD_EXEC)
    )
    poll_route = respx.get(f"{BASE_URL}/v1/interventions/int_1")
    held: list[str] = []
    client = fuse.Client(BASE_URL, "tok", hitl_wait=False, on_held=lambda i: held.append(i.id))
    with client:
        with pytest.raises(fuse.PendingInterventionError):
            client.environments.exec("vm-1", fuse.ExecRequest(cmd=["git"]))
        # the per-call callback overrides the client's.
        with pytest.raises(fuse.PendingInterventionError):
            client.environments.exec(
                "vm-1", fuse.ExecRequest(cmd=["git"]), on_held=lambda i: held.append("call")
            )

    assert not poll_route.called
    assert held == ["int_1", "call"]


@respx.mock
def test_destroy_denied_by_gate() -> None:
    respx.delete(f"{BASE_URL}/v1/environments/vm-1").mock(
        return_value=httpx.Response(
            403,
            json={"error": {"code": "forbidden", "message": "keep it",
                            "details": {"reason": "hitl_denied", "gate": "no-destroy",
                                        "intervention_id": "int_9"}}},
        )
    )
    with new_client() as client, pytest.raises(fuse.ForbiddenError) as exc:
        client.environments.destroy("vm-1")

    assert fuse.is_hitl_denied(exc.value)
    assert exc.value.reason == fuse.FORBIDDEN_HITL_DENIED
    assert exc.value.message == "keep it"
    assert exc.value.details["gate"] == "no-destroy"


def test_is_hitl_denied_ignores_other_errors() -> None:
    other = fuse.ForbiddenError(403, code="forbidden", details={"reason": "missing_scope"})
    assert not fuse.is_hitl_denied(other)
    assert not fuse.is_hitl_denied(ValueError("x"))


@respx.mock
def test_drain_held_then_rejected() -> None:
    respx.post(f"{BASE_URL}/v1/environments/vm-1", params={"action": "drain"}).mock(
        return_value=httpx.Response(
            202,
            json={"intervention_id": "int_2",
                  "intervention": {"id": "int_2", "kind": "lifecycle", "mode": "ask",
                                   "state": "pending",
                                   "payload": {"lifecycle": {"action": "drain"}}}},
        )
    )
    respx.get(f"{BASE_URL}/v1/interventions/int_2").mock(
        return_value=httpx.Response(
            200,
            json={"id": "int_2", "kind": "lifecycle", "mode": "ask", "state": "rejected",
                  "payload": {"lifecycle": {"action": "drain"}},
                  "decision": {"outcome": "reject", "reason": "not now"}},
        )
    )
    with new_client() as client, pytest.raises(fuse.InterventionRejectedError) as exc:
        client.environments.drain("vm-1")

    decision = exc.value.intervention.decision
    assert decision is not None
    assert decision.reason == "not now"
    assert "not now" in str(exc.value)


@respx.mock
def test_held_action_failure_raises() -> None:
    respx.post(f"{BASE_URL}/v1/environments/vm-1", params={"action": "rotate-token"}).mock(
        return_value=httpx.Response(
            202,
            json={"intervention_id": "int_4",
                  "intervention": {"id": "int_4", "kind": "lifecycle", "mode": "ask",
                                   "state": "pending",
                                   "payload": {"lifecycle": {"action": "rotate-token"}}}},
        )
    )
    respx.get(f"{BASE_URL}/v1/interventions/int_4").mock(
        return_value=httpx.Response(
            200,
            json={"id": "int_4", "kind": "lifecycle", "mode": "ask", "state": "approved",
                  "payload": {"lifecycle": {"action": "rotate-token"}},
                  "decision": {"outcome": "approve"}, "result": {"error": "agent down"}},
        )
    )
    with new_client() as client, pytest.raises(fuse.HeldActionError) as exc:
        client.environments.rotate_token("vm-1")

    assert exc.value.intervention.result is not None
    assert exc.value.intervention.result.error == "agent down"


@respx.mock
def test_held_fork_returns_new_environment() -> None:
    respx.post(f"{BASE_URL}/v1/environments/vm-1", params={"action": "fork"}).mock(
        return_value=httpx.Response(
            202,
            json={"intervention_id": "int_3",
                  "intervention": {"id": "int_3", "kind": "lifecycle", "mode": "ask",
                                   "state": "pending",
                                   "payload": {"lifecycle": {"action": "fork"}}}},
        )
    )
    respx.get(f"{BASE_URL}/v1/interventions/int_3").mock(
        return_value=httpx.Response(
            200,
            json={"id": "int_3", "kind": "lifecycle", "mode": "ask", "state": "approved",
                  "payload": {"lifecycle": {"action": "fork"}},
                  "decision": {"outcome": "approve"}, "result": {"env_id": "vm-2"}},
        )
    )
    get_route = respx.get(f"{BASE_URL}/v1/environments/vm-2").mock(
        return_value=httpx.Response(200, json={"id": "vm-2", "state": "running"})
    )
    with new_client() as client:
        env = client.environments.fork("vm-1")

    assert get_route.called
    assert env.id == "vm-2"


@respx.mock
def test_held_snapshot_returns_snapshot() -> None:
    respx.post(f"{BASE_URL}/v1/environments/vm-1/snapshots").mock(
        return_value=httpx.Response(
            202,
            json={"intervention_id": "int_5",
                  "intervention": {"id": "int_5", "kind": "lifecycle", "mode": "ask",
                                   "state": "pending",
                                   "payload": {"lifecycle": {"action": "snapshot"}}}},
        )
    )
    respx.get(f"{BASE_URL}/v1/interventions/int_5").mock(
        return_value=httpx.Response(
            200,
            json={"id": "int_5", "kind": "lifecycle", "mode": "ask", "state": "approved",
                  "payload": {"lifecycle": {"action": "snapshot"}},
                  "decision": {"outcome": "approve"}, "result": {"snapshot_id": "snap-1"}},
        )
    )
    respx.get(f"{BASE_URL}/v1/snapshots/snap-1").mock(
        return_value=httpx.Response(200, json={"id": "snap-1"})
    )
    with new_client() as client:
        snap = client.snapshots.create("vm-1")

    assert snap.id == "snap-1"


@respx.mock
def test_approve_sends_edited_argv_and_grant() -> None:
    route = respx.post(f"{BASE_URL}/v1/interventions/int_1").mock(
        return_value=httpx.Response(200, json={"id": "int_1", "state": "approved"})
    )
    with new_client() as client:
        client.interventions.approve("int_1", edited_argv=["git", "push"], grant="ttl:30m")

    req = route.calls.last.request
    assert req.url.params["action"] == "approve"
    assert json.loads(req.content) == {"edited_argv": ["git", "push"], "grant": "ttl:30m"}


def test_settled() -> None:
    def make(**fields: object) -> fuse.Intervention:
        return fuse.Intervention.model_validate(fields)

    gate = {"mode": "ask", "payload": {"exec": {"argv": ["git"]}}}
    approve = {"outcome": "approve"}
    # an ask is settled once terminal.
    assert make(state="approved", decision=approve).settled
    assert not make(state="pending", **gate).settled
    # an approved runnable gate action needs its result.
    assert not make(state="approved", decision=approve, **gate).settled
    assert make(state="approved", decision=approve, result={"env_id": "x"}, **gate).settled
    assert make(state="rejected", decision={"outcome": "reject"}, **gate).settled
    # a notify record does not run.
    assert make(state="approved", decision=approve, mode="notify",
                payload={"exec": {"argv": ["git"]}}).settled


def test_hitl_spec_serializes_gates() -> None:
    spec = fuse.HitlSpec(
        gates=[
            fuse.HitlGate(
                name="force-push",
                on=fuse.HITL_KIND_EXEC,
                match=fuse.HitlGateMatch(program=["git"], args=["push", "--force"]),
                grant="once",
            ),
            fuse.HitlGate(
                name="no-destroy",
                on=fuse.HITL_KIND_LIFECYCLE,
                match=fuse.HitlGateMatch(action=[fuse.LIFECYCLE_DESTROY]),
                mode=fuse.GATE_MODE_DENY,
            ),
        ],
        exec=fuse.HitlExecPolicy(default="ask", allow=["ls"]),
    )
    assert spec.model_dump(by_alias=True, exclude_none=True) == {
        "requests": {},
        "gates": [
            {"name": "force-push", "on": "exec",
             "match": {"program": ["git"], "args": ["push", "--force"]}, "grant": "once"},
            {"name": "no-destroy", "on": "lifecycle", "match": {"action": ["destroy"]},
             "mode": "deny"},
        ],
        "exec": {"default": "ask", "allow": ["ls"]},
    }

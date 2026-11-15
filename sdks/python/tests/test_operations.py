from __future__ import annotations

import uuid

import httpx
import pytest
import respx

import fuse

BASE_URL = "https://fuse.test"

ENV = {"id": "vm-1", "state": "running", "task_id": "task-1", "url": "u"}


def running_op(op_id: str = "op_1") -> dict[str, object]:
    return {
        "id": op_id,
        "kind": "create",
        "state": "running",
        "step": "boot",
        "started_at": "2026-11-15T00:00:00Z",
    }


def finished_op(status_code: int, **extra: object) -> dict[str, object]:
    op = running_op()
    op.update(
        state="succeeded" if status_code < 300 else "failed",
        finished_at="2026-11-15T00:01:00Z",
        status_code=status_code,
        **extra,
    )
    return op


def accepted() -> httpx.Response:
    return httpx.Response(
        202,
        json={"operation_id": "op_1", "operation": running_op()},
        headers={"Location": "/v1/operations/op_1", "Fuse-Operation-Id": "op_1"},
    )


@respx.mock
def test_create_sends_auto_idempotency_key() -> None:
    route = respx.post(f"{BASE_URL}/v1/environments").mock(
        return_value=httpx.Response(200, json=ENV)
    )
    with fuse.Client(BASE_URL, "tok") as client:
        client.environments.create(fuse.CreateRequest(task_id="task-1"))
        client.environments.create(fuse.CreateRequest(task_id="task-1"))

    first = route.calls[0].request.headers["Idempotency-Key"]
    second = route.calls[1].request.headers["Idempotency-Key"]
    uuid.UUID(first)
    # each logical call gets its own key.
    assert first != second


@respx.mock
def test_create_sends_explicit_idempotency_key() -> None:
    route = respx.post(f"{BASE_URL}/v1/environments").mock(
        return_value=httpx.Response(200, json=ENV)
    )
    with fuse.Client(BASE_URL, "tok") as client:
        client.environments.create(fuse.CreateRequest(task_id="task-1"), idempotency_key="k-1")

    assert route.calls.last.request.headers["Idempotency-Key"] == "k-1"


@respx.mock
def test_create_follows_accepted_operation() -> None:
    respx.post(f"{BASE_URL}/v1/environments").mock(return_value=accepted())
    op_route = respx.get(f"{BASE_URL}/v1/operations/op_1").mock(
        side_effect=[
            httpx.Response(200, json=running_op()),
            httpx.Response(200, json=finished_op(201, result=ENV)),
        ]
    )
    seen: list[fuse.OperationAccepted] = []
    with fuse.Client(BASE_URL, "tok", on_operation=seen.append) as client:
        env = client.environments.create(fuse.CreateRequest(task_id="task-1"))

    assert env.id == "vm-1"
    assert env.state == "running"
    assert [acc.operation_id for acc in seen] == ["op_1"]
    assert seen[0].operation.state == fuse.OPERATION_RUNNING
    assert op_route.call_count == 2
    assert op_route.calls.last.request.url.params["wait"] == "30s"


@respx.mock
def test_followed_operation_failure_raises_not_found() -> None:
    respx.post(f"{BASE_URL}/v1/environments/vm-1").mock(return_value=accepted())
    respx.get(f"{BASE_URL}/v1/operations/op_1").mock(
        return_value=httpx.Response(
            200,
            json=finished_op(
                404, error={"code": "not_found", "message": "environment not found"}
            ),
        )
    )
    with fuse.Client(BASE_URL, "tok") as client, pytest.raises(fuse.ApiError) as excinfo:
        client.environments.fork("vm-1")

    assert fuse.is_not_found(excinfo.value)
    assert excinfo.value.status == 404
    assert excinfo.value.message == "environment not found"


@respx.mock
def test_followed_destroy_returns_none_on_204() -> None:
    route = respx.delete(f"{BASE_URL}/v1/environments/vm-1").mock(return_value=accepted())
    respx.get(f"{BASE_URL}/v1/operations/op_1").mock(
        return_value=httpx.Response(200, json=finished_op(204))
    )
    with fuse.Client(BASE_URL, "tok") as client:
        assert client.environments.destroy("vm-1", idempotency_key="k-2") is None

    assert route.calls.last.request.headers["Idempotency-Key"] == "k-2"


@respx.mock
def test_followed_snapshot_held_goes_down_hitl_path() -> None:
    held = {
        "intervention_id": "int_1",
        "intervention": {"id": "int_1", "state": "pending", "gate": "snap"},
    }
    respx.post(f"{BASE_URL}/v1/environments/vm-1/snapshots").mock(return_value=accepted())
    respx.get(f"{BASE_URL}/v1/operations/op_1").mock(
        return_value=httpx.Response(200, json=finished_op(202, result=held))
    )
    with fuse.Client(BASE_URL, "tok", hitl_wait=False) as client:
        with pytest.raises(fuse.PendingInterventionError) as excinfo:
            client.snapshots.create("vm-1")

    assert excinfo.value.intervention.id == "int_1"


@respx.mock
def test_operations_get() -> None:
    route = respx.get(f"{BASE_URL}/v1/operations/op_1").mock(
        return_value=httpx.Response(200, json=running_op())
    )
    with fuse.Client(BASE_URL, "tok") as client:
        op = client.operations.get("op_1")
        assert "wait" not in route.calls.last.request.url.params
        client.operations.get("op_1", wait=5)

    assert route.calls.last.request.url.params["wait"] == "5s"
    assert isinstance(op, fuse.Operation)
    assert op.id == "op_1"
    assert op.kind == "create"
    assert op.step == "boot"
    assert not op.done


@respx.mock
def test_operations_get_unknown_raises_not_found() -> None:
    respx.get(f"{BASE_URL}/v1/operations/op_x").mock(
        return_value=httpx.Response(
            404, json={"error": {"code": "not_found", "message": "operation not found"}}
        )
    )
    with fuse.Client(BASE_URL, "tok") as client, pytest.raises(fuse.ApiError) as excinfo:
        client.operations.get("op_x")

    assert fuse.is_not_found(excinfo.value)


@respx.mock
def test_operations_wait_returns_finished_operation() -> None:
    route = respx.get(f"{BASE_URL}/v1/operations/op_1").mock(
        side_effect=[
            httpx.Response(200, json=running_op()),
            httpx.Response(200, json=finished_op(201, result=ENV)),
        ]
    )
    with fuse.Client(BASE_URL, "tok") as client:
        op = client.operations.wait("op_1")

    assert route.call_count == 2
    assert op.done
    assert op.state == fuse.OPERATION_SUCCEEDED
    assert op.status_code == 201
    assert fuse.EnvironmentInfo.model_validate(op.result).id == "vm-1"


@respx.mock
def test_operations_wait_timeout() -> None:
    respx.get(f"{BASE_URL}/v1/operations/op_1").mock(
        return_value=httpx.Response(200, json=running_op())
    )
    with fuse.Client(BASE_URL, "tok") as client, pytest.raises(TimeoutError):
        client.operations.wait("op_1", timeout=0.01)

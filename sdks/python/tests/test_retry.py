from __future__ import annotations

from typing import Optional

import httpx
import pytest
import respx

import fuse
import fuse._retry
from fuse._retry import retry_delay

BASE_URL = "https://fuse.test"

ENV = {"id": "vm-1", "state": "running", "task_id": "task-1", "url": "u"}


def unavailable(retry_after: str = "0") -> httpx.Response:
    return httpx.Response(
        503,
        json={"error": {"code": "unavailable", "message": "no host has sufficient capacity"}},
        headers={"Retry-After": retry_after},
    )


@respx.mock
def test_create_retries_unavailable_under_the_same_key() -> None:
    route = respx.post(f"{BASE_URL}/v1/environments").mock(
        side_effect=[unavailable(), unavailable(), httpx.Response(201, json=ENV)]
    )
    with fuse.Client(BASE_URL, "tok") as client:
        env = client.environments.create(fuse.CreateRequest(task_id="task-1"))

    assert env.id == "vm-1"
    assert route.call_count == 3
    keys = {c.request.headers["Idempotency-Key"] for c in route.calls}
    bodies = {c.request.content for c in route.calls}
    assert len(keys) == 1 and len(bodies) == 1


@respx.mock
def test_create_raises_unavailable_once_retries_run_out() -> None:
    route = respx.post(f"{BASE_URL}/v1/environments").mock(return_value=unavailable())
    with fuse.Client(BASE_URL, "tok") as client:
        with pytest.raises(fuse.ApiError) as exc:
            client.environments.create(fuse.CreateRequest(task_id="task-1"))

    assert fuse.is_unavailable(exc.value)
    assert route.call_count == 4


@respx.mock
def test_retries_zero_raises_the_first_503() -> None:
    route = respx.post(f"{BASE_URL}/v1/environments").mock(return_value=unavailable())
    with fuse.Client(BASE_URL, "tok", retries=0) as client:
        with pytest.raises(fuse.ApiError):
            client.environments.create(fuse.CreateRequest(task_id="task-1"))

    assert route.call_count == 1


@respx.mock
def test_unkeyed_computer_action_is_not_retried() -> None:
    # a computer action is a POST with no idempotency key: its 503 (no
    # display) is not something a retry fixes, and it is not safe to resend.
    route = respx.post(f"{BASE_URL}/v1/environments/vm-1/computer").mock(return_value=unavailable())
    with fuse.Client(BASE_URL, "tok") as client:
        with pytest.raises(fuse.ApiError):
            client.environments.computer("vm-1", fuse.ComputerAction(action="screenshot"))

    assert route.call_count == 1


def test_retry_delay_honors_retry_after_plus_jitter() -> None:
    assert retry_delay("2", 0, lambda: 0.0) == 2.0
    assert 2.0 <= retry_delay("2", 3, lambda: 0.999) <= 3.0
    assert retry_delay("3600", 0, lambda: 0.0) == 60.0


def test_retry_delay_backs_off_without_retry_after() -> None:
    assert retry_delay(None, 0, lambda: 0.0) == 1.0
    assert retry_delay(None, 2, lambda: 0.0) == 4.0
    assert retry_delay(None, 20, lambda: 0.0) == 30.0


@respx.mock
def test_polled_unavailable_waits_the_operations_retry_after(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    # a create that answers 202 and whose operation then fails 503 waits the
    # failed answer's Retry-After, carried on the operation.
    asked: list[Optional[str]] = []

    def record(retry_after: Optional[str], attempt: int) -> float:
        asked.append(retry_after)
        return 0.0

    monkeypatch.setattr(fuse._retry, "retry_delay", record)
    running = {
        "id": "op_1",
        "kind": "create",
        "state": "running",
        "started_at": "2026-11-15T00:00:00Z",
    }
    route = respx.post(f"{BASE_URL}/v1/environments").mock(
        side_effect=[
            httpx.Response(202, json={"operation_id": "op_1", "operation": running}),
            httpx.Response(201, json=ENV),
        ]
    )
    respx.get(f"{BASE_URL}/v1/operations/op_1").mock(
        return_value=httpx.Response(
            200,
            json={
                **running,
                "state": "failed",
                "finished_at": "2026-11-15T00:01:00Z",
                "status_code": 503,
                "retry_after": 5,
                "error": {"code": "unavailable", "message": "no host has sufficient capacity"},
            },
        )
    )
    with fuse.Client(BASE_URL, "tok") as client:
        env = client.environments.create(fuse.CreateRequest(task_id="task-1"))

    assert env.id == "vm-1"
    assert route.call_count == 2
    assert asked == ["5"]

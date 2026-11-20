from __future__ import annotations

import httpx
import pytest
import respx

import fuse
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
def test_unkeyed_call_is_not_retried() -> None:
    route = respx.get(f"{BASE_URL}/v1/environments/vm-1").mock(return_value=unavailable())
    with fuse.Client(BASE_URL, "tok") as client:
        with pytest.raises(fuse.ApiError):
            client.environments.get("vm-1")

    assert route.call_count == 1


def test_retry_delay_honors_retry_after_plus_jitter() -> None:
    assert retry_delay("2", 0, lambda: 0.0) == 2.0
    assert 2.0 <= retry_delay("2", 3, lambda: 0.999) <= 3.0
    assert retry_delay("3600", 0, lambda: 0.0) == 60.0


def test_retry_delay_backs_off_without_retry_after() -> None:
    assert retry_delay(None, 0, lambda: 0.0) == 1.0
    assert retry_delay(None, 2, lambda: 0.0) == 4.0
    assert retry_delay(None, 20, lambda: 0.0) == 30.0

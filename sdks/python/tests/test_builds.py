from __future__ import annotations

import json
import time
import uuid

import httpx
import pytest
import respx

import fuse

BASE_URL = "https://fuse.test"


def running_build() -> dict[str, object]:
    return {
        "id": "bld_1",
        "name": "web",
        "state": "running",
        "step": "layer_0",
        "cache_hits": 0,
        "layers": [],
        "created_at": "2026-11-15T00:00:00Z",
        "updated_at": "2026-11-15T00:00:00Z",
    }


def finished_build() -> dict[str, object]:
    b = running_build()
    b.update(
        state="succeeded",
        step="done",
        builder_id="vm-b",
        arch="x86_64",
        cache_hits=1,
        seed_snapshot_id="snap-seed",
        layers=[
            {"index": 0, "key": "k0", "status": "hit", "snapshot_id": "snap-0"},
            {"index": 1, "status": "skip"},
        ],
        snapshot_id="snap-final",
        finished_at="2026-11-15T00:01:00Z",
    )
    return b


def request() -> fuse.CreateBuildRequest:
    return fuse.CreateBuildRequest(
        name="web",
        spec=fuse.Spec(cpus=2),
        setup=fuse.BuildSetup(
            prelude="set -e",
            steps=[fuse.BuildStep(script="apt-get install -y curl", key="k0")],
        ),
    )


@respx.mock
def test_create_sends_body_and_auto_idempotency_key() -> None:
    route = respx.post(f"{BASE_URL}/v1/builds").mock(
        return_value=httpx.Response(
            202, json=running_build(), headers={"Location": "/v1/builds/bld_1"}
        )
    )
    with fuse.Client(BASE_URL, "tok") as client:
        b = client.builds.create(request())
        client.builds.create(request())

    assert b.id == "bld_1"
    assert b.state == fuse.BUILD_RUNNING
    assert not b.done
    body = json.loads(route.calls[0].request.content)
    assert body == {
        "name": "web",
        "spec": {"cpus": 2},
        "setup": {
            "prelude": "set -e",
            "steps": [{"script": "apt-get install -y curl", "key": "k0"}],
        },
    }
    first = route.calls[0].request.headers["Idempotency-Key"]
    second = route.calls[1].request.headers["Idempotency-Key"]
    uuid.UUID(first)
    assert first != second


@respx.mock
def test_create_sends_explicit_idempotency_key() -> None:
    route = respx.post(f"{BASE_URL}/v1/builds").mock(
        return_value=httpx.Response(202, json=running_build())
    )
    with fuse.Client(BASE_URL, "tok") as client:
        client.builds.create(request(), idempotency_key="k-1")

    assert route.calls.last.request.headers["Idempotency-Key"] == "k-1"


@respx.mock
def test_get_decodes_layers() -> None:
    route = respx.get(f"{BASE_URL}/v1/builds/bld_1").mock(
        return_value=httpx.Response(200, json=finished_build())
    )
    with fuse.Client(BASE_URL, "tok") as client:
        b = client.builds.get("bld_1")
        assert "wait" not in route.calls.last.request.url.params
        client.builds.get("bld_1", wait=5)

    assert route.calls.last.request.url.params["wait"] == "5s"
    assert isinstance(b, fuse.Build)
    assert b.done
    assert b.state == fuse.BUILD_SUCCEEDED
    assert b.snapshot_id == "snap-final"
    assert b.cache_hits == 1
    assert b.seed_snapshot_id == "snap-seed"
    assert b.layers[0] == fuse.BuildLayer(index=0, key="k0", status="hit", snapshot_id="snap-0")
    assert b.layers[1].status == "skip"
    assert b.layers[1].key == ""
    assert b.finished_at is not None


@respx.mock
def test_get_unknown_raises_not_found() -> None:
    respx.get(f"{BASE_URL}/v1/builds/bld_x").mock(
        return_value=httpx.Response(
            404, json={"error": {"code": "not_found", "message": "build not found"}}
        )
    )
    with fuse.Client(BASE_URL, "tok") as client, pytest.raises(fuse.ApiError) as excinfo:
        client.builds.get("bld_x")

    assert fuse.is_not_found(excinfo.value)


@respx.mock
def test_wait_polls_until_finished() -> None:
    route = respx.get(f"{BASE_URL}/v1/builds/bld_1").mock(
        side_effect=[
            httpx.Response(200, json=running_build()),
            httpx.Response(200, json=finished_build()),
        ]
    )
    with fuse.Client(BASE_URL, "tok") as client:
        b = client.builds.wait("bld_1")

    assert route.call_count == 2
    assert route.calls.last.request.url.params["wait"] == "30s"
    assert b.snapshot_id == "snap-final"


@respx.mock
def test_wait_returns_failed_build() -> None:
    failed = running_build()
    failed.update(state="failed", error="setup step 0 exited 1", output="boom")
    respx.get(f"{BASE_URL}/v1/builds/bld_1").mock(return_value=httpx.Response(200, json=failed))
    with fuse.Client(BASE_URL, "tok") as client:
        b = client.builds.wait("bld_1")

    assert b.state == fuse.BUILD_FAILED
    assert b.output == "boom"


@respx.mock
def test_wait_timeout() -> None:
    respx.get(f"{BASE_URL}/v1/builds/bld_1").mock(
        return_value=httpx.Response(200, json=running_build())
    )
    with fuse.Client(BASE_URL, "tok") as client, pytest.raises(TimeoutError):
        client.builds.wait("bld_1", timeout=0.01)


@respx.mock
def test_wait_bounds_http_by_timeout() -> None:
    seen: list[float] = []

    def slow(request: httpx.Request) -> httpx.Response:
        seen.append(request.extensions["timeout"]["read"])
        time.sleep(0.25)
        raise httpx.ReadTimeout("slow", request=request)

    respx.get(f"{BASE_URL}/v1/builds/bld_1").mock(side_effect=slow)
    with fuse.Client(BASE_URL, "tok") as client, pytest.raises(TimeoutError):
        client.builds.wait("bld_1", timeout=0.2)
    assert seen and seen[0] <= 0.2


def test_event_decodes_build_member() -> None:
    ev = fuse.Event.model_validate_json(
        '{"event":"build","vm_id":"vm-b","build":{"id":"bld_1","state":"running","step":"layer_0"}}'
    )
    assert ev.kind == "build"
    assert ev.build == fuse.BuildEvent(id="bld_1", state="running", step="layer_0")
    assert fuse.Event.model_validate_json('{"event":"state"}').build is None

from __future__ import annotations

import httpx
import pytest
import respx

import fuse

BASE_URL = "https://fuse.test"


def new_client() -> fuse.Client:
    # client pointed at the mocked base url. respx intercepts httpx globally,
    # so both the normal and streaming clients are captured.
    return fuse.Client(BASE_URL, "tok")


@respx.mock
def test_environments_create() -> None:
    route = respx.post(f"{BASE_URL}/v1/environments").mock(
        return_value=httpx.Response(
            200,
            json={
                "id": "vm-1",
                "state": "running",
                "task_id": "task-1",
                "url": "https://x",
            },
        )
    )
    with new_client() as client:
        env = client.environments.create(fuse.CreateRequest(task_id="task-1"))

    assert route.calls.last.request.method == "POST"
    assert route.calls.last.request.url.path == "/v1/environments"
    assert env.id == "vm-1"
    assert env.state == "running"


@respx.mock
def test_environments_list() -> None:
    route = respx.get(f"{BASE_URL}/v1/environments").mock(
        return_value=httpx.Response(
            200,
            json={
                "environments": [
                    {"id": "vm-1", "state": "running", "task_id": "task-1", "url": "u"},
                    {"id": "vm-2", "state": "draining", "task_id": "task-1", "url": "u"},
                ]
            },
        )
    )
    with new_client() as client:
        envs = client.environments.list(task_id="task-1")

    request = route.calls.last.request
    assert request.method == "GET"
    assert request.url.path == "/v1/environments"
    assert request.url.params.get("task_id") == "task-1"
    assert [e.id for e in envs] == ["vm-1", "vm-2"]


@respx.mock
def test_environments_drain() -> None:
    route = respx.post(f"{BASE_URL}/v1/environments/vm-1").mock(
        return_value=httpx.Response(
            200,
            json={"id": "vm-1", "state": "draining", "task_id": "task-1", "url": "u"},
        )
    )
    with new_client() as client:
        env = client.environments.drain("vm-1")

    request = route.calls.last.request
    assert request.method == "POST"
    assert request.url.path == "/v1/environments/vm-1"
    assert request.url.params.get("action") == "drain"
    assert env.state == "draining"


@respx.mock
def test_environments_rotate_token() -> None:
    route = respx.post(f"{BASE_URL}/v1/environments/vm-1").mock(
        return_value=httpx.Response(204)
    )
    with new_client() as client:
        client.environments.rotate_token("vm-1")

    request = route.calls.last.request
    assert request.method == "POST"
    assert request.url.path == "/v1/environments/vm-1"
    assert request.url.params.get("action") == "rotate-token"


@respx.mock
def test_snapshots_create() -> None:
    route = respx.post(f"{BASE_URL}/v1/environments/vm-1/snapshots").mock(
        return_value=httpx.Response(
            200,
            json={"id": "snap-1", "vm_id": "vm-1", "created_at": "2024-01-01T00:00:00Z"},
        )
    )
    with new_client() as client:
        snap = client.snapshots.create("vm-1", fuse.SnapshotRequest(comment="c"))

    request = route.calls.last.request
    assert request.method == "POST"
    assert request.url.path == "/v1/environments/vm-1/snapshots"
    assert snap.id == "snap-1"
    assert snap.vm_id == "vm-1"


@respx.mock
def test_snapshots_list() -> None:
    route = respx.get(f"{BASE_URL}/v1/snapshots").mock(
        return_value=httpx.Response(
            200,
            json={
                "snapshots": [
                    {"id": "snap-1", "vm_id": "vm-1", "created_at": "2024-01-01T00:00:00Z"}
                ]
            },
        )
    )
    with new_client() as client:
        snaps = client.snapshots.list(vm_id="vm-1")

    request = route.calls.last.request
    assert request.method == "GET"
    assert request.url.params.get("vm_id") == "vm-1"
    assert [s.id for s in snaps] == ["snap-1"]


@respx.mock
def test_snapshots_restore() -> None:
    route = respx.post(f"{BASE_URL}/v1/snapshots/snap-1").mock(
        return_value=httpx.Response(204)
    )
    with new_client() as client:
        client.snapshots.restore("snap-1")

    request = route.calls.last.request
    assert request.method == "POST"
    assert request.url.path == "/v1/snapshots/snap-1"
    assert request.url.params.get("action") == "restore"


@respx.mock
def test_hosts_register() -> None:
    route = respx.post(f"{BASE_URL}/v1/hosts").mock(
        return_value=httpx.Response(
            200,
            json={
                "id": "host-1",
                "url": "https://h",
                "state": "active",
                "capacity": {
                    "cpus": 4,
                    "ram_mb": 8192,
                    "storage_gb": 100,
                    "vm_count": 10,
                },
                "allocated": {"cpus": 0, "ram_mb": 0, "storage_gb": 0, "vm_count": 0},
                "last_seen": "2024-01-01T00:00:00Z",
                "created_at": "2024-01-01T00:00:00Z",
                "updated_at": "2024-01-01T00:00:00Z",
            },
        )
    )
    with new_client() as client:
        host = client.hosts.register(
            fuse.RegisterHostRequest(id="host-1", url="https://h")
        )

    request = route.calls.last.request
    assert request.method == "POST"
    assert request.url.path == "/v1/hosts"
    assert host.id == "host-1"
    assert host.capacity.cpus == 4


@respx.mock
def test_hosts_list() -> None:
    route = respx.get(f"{BASE_URL}/v1/hosts").mock(
        return_value=httpx.Response(
            200,
            json={"hosts": [{"id": "host-1", "url": "https://h", "state": "active"}]},
        )
    )
    with new_client() as client:
        hosts = client.hosts.list()

    request = route.calls.last.request
    assert request.method == "GET"
    assert request.url.path == "/v1/hosts"
    assert [h.id for h in hosts] == ["host-1"]


@respx.mock
def test_hosts_cordon() -> None:
    route = respx.post(f"{BASE_URL}/v1/hosts/host-1").mock(
        return_value=httpx.Response(204)
    )
    with new_client() as client:
        client.hosts.cordon("host-1")

    request = route.calls.last.request
    assert request.method == "POST"
    assert request.url.path == "/v1/hosts/host-1"
    assert request.url.params.get("action") == "cordon"


@respx.mock
def test_apikeys_create() -> None:
    route = respx.post(f"{BASE_URL}/v1/api-keys").mock(
        return_value=httpx.Response(
            200,
            json={
                "id": "key-1",
                "label": "ci",
                "created_at": "2024-01-01T00:00:00Z",
                "key": "secret-raw",
            },
        )
    )
    with new_client() as client:
        key = client.api_keys.create("ci")

    request = route.calls.last.request
    assert request.method == "POST"
    assert request.url.path == "/v1/api-keys"
    assert key.key == "secret-raw"
    assert key.id == "key-1"


@respx.mock
def test_apikeys_list() -> None:
    route = respx.get(f"{BASE_URL}/v1/api-keys").mock(
        return_value=httpx.Response(
            200,
            json={
                "api_keys": [
                    {"id": "key-1", "label": "ci", "created_at": "2024-01-01T00:00:00Z"}
                ]
            },
        )
    )
    with new_client() as client:
        keys = client.api_keys.list()

    request = route.calls.last.request
    assert request.method == "GET"
    assert request.url.path == "/v1/api-keys"
    assert [k.id for k in keys] == ["key-1"]


@respx.mock
def test_check_response_not_found() -> None:
    respx.get(f"{BASE_URL}/v1/environments/missing").mock(
        return_value=httpx.Response(
            404, json={"error": {"code": "not_found", "message": "x"}}
        )
    )
    with new_client() as client:
        with pytest.raises(fuse.ApiError) as excinfo:
            client.environments.get("missing")

    err = excinfo.value
    assert fuse.as_api_error(err) is err
    assert err.status == 404
    assert err.code == "not_found"
    assert err.message == "x"
    assert fuse.is_not_found(err)


@respx.mock
def test_events_stream() -> None:
    body = (
        "id: 1\n"
        'data: {"event":"state","vm_id":"v","state":"running",'
        '"updated_at":"2024-01-01T00:00:00Z"}\n'
        "\n"
        'data: {"event":"state","vm_id":"v","state":"destroyed",'
        '"updated_at":"2024-01-01T00:00:01Z"}\n'
        "\n"
    )
    respx.get(f"{BASE_URL}/v1/environments/v/events").mock(
        return_value=httpx.Response(
            200, headers={"Content-Type": "text/event-stream"}, text=body
        )
    )
    with new_client() as client:
        events = list(client.environments.events("v"))

    assert [e.err for e in events] == [None, None]
    assert events[0].state == fuse.STATE_RUNNING
    assert events[1].state == fuse.STATE_DESTROYED
    # terminal state must end the iterator.
    assert len(events) == 2

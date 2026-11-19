from __future__ import annotations

import json

import httpx
import pytest
import respx

import fuse
from fuse._core import serialize_body

BASE_URL = "https://fuse.test"
DIGEST = "a" * 64


def converting_image() -> dict[str, object]:
    return {
        "digest": DIGEST,
        "ref": "docker.io/library/python:3.12",
        "manifest_digest": "sha256:" + "b" * 64,
        "host_id": "host-1",
        "state": "converting",
    }


@respx.mock
def test_convert_sends_body_and_decodes_image() -> None:
    route = respx.post(f"{BASE_URL}/v1/images").mock(
        return_value=httpx.Response(202, json=converting_image())
    )
    with fuse.Client(BASE_URL, "tok") as client:
        img = client.images.convert(
            fuse.ConvertImageRequest(
                ref="docker.io/library/python:3.12", region="eu-west", labels={"zone": "a"}
            )
        )

    assert route.calls.last.request.method == "POST"
    body = json.loads(route.calls.last.request.content)
    assert body == {
        "ref": "docker.io/library/python:3.12",
        "region": "eu-west",
        "labels": {"zone": "a"},
    }
    assert isinstance(img, fuse.Image)
    assert img.digest == DIGEST
    assert img.state == fuse.IMAGE_CONVERTING
    assert img.host_id == "host-1"
    assert img.error == ""


def test_convert_requires_ref() -> None:
    with fuse.Client(BASE_URL, "tok") as client, pytest.raises(ValueError):
        client.images.convert(fuse.ConvertImageRequest())


@respx.mock
def test_get_escapes_digest_and_decodes_failed_image() -> None:
    failed = converting_image()
    failed.update(state="failed", error="pull: manifest unknown")
    route = respx.get(url__regex=r"https://fuse\.test/v1/images/.*").mock(
        return_value=httpx.Response(200, json=failed)
    )
    with fuse.Client(BASE_URL, "tok") as client:
        img = client.images.get("a/b c")

    assert route.calls.last.request.url.raw_path == b"/v1/images/a%2Fb%20c"
    assert img.state == fuse.IMAGE_FAILED
    assert img.error == "pull: manifest unknown"


@respx.mock
def test_get_unknown_raises_not_found() -> None:
    respx.get(f"{BASE_URL}/v1/images/{DIGEST}").mock(
        return_value=httpx.Response(
            404, json={"error": {"code": "not_found", "message": "image not found"}}
        )
    )
    with fuse.Client(BASE_URL, "tok") as client, pytest.raises(fuse.ApiError) as excinfo:
        client.images.get(DIGEST)

    assert fuse.is_not_found(excinfo.value)


def test_spec_serializes_image_digest() -> None:
    spec = fuse.Spec(cpus=1, image_digest=DIGEST)
    assert serialize_body(spec) == {"cpus": 1, "image_digest": DIGEST}


@respx.mock
def test_convert_sends_an_idempotency_key() -> None:
    route = respx.post(f"{BASE_URL}/v1/images").mock(
        return_value=httpx.Response(202, json=converting_image())
    )
    with fuse.Client(BASE_URL, "tok") as client:
        client.images.convert(fuse.ConvertImageRequest(ref="python:3.12"))
        client.images.convert(fuse.ConvertImageRequest(ref="python:3.12"), idempotency_key="my-key")

    keys = [call.request.headers.get("Idempotency-Key") for call in route.calls]
    assert keys[0]
    assert keys[1] == "my-key"


@respx.mock
def test_convert_unkeyed_calls_send_different_keys() -> None:
    route = respx.post(f"{BASE_URL}/v1/images").mock(
        return_value=httpx.Response(202, json=converting_image())
    )
    with fuse.Client(BASE_URL, "tok") as client:
        client.images.convert(fuse.ConvertImageRequest(ref="python:3.12"))
        client.images.convert(fuse.ConvertImageRequest(ref="python:3.12"))

    keys = [call.request.headers.get("Idempotency-Key") for call in route.calls]
    assert len(keys) == 2
    assert keys[0] and keys[1]
    assert keys[0] != keys[1]


def image_op(state: str, **extra: object) -> dict[str, object]:
    op: dict[str, object] = {
        "id": "op_1",
        "kind": "image.convert",
        "state": state,
        "started_at": "2026-11-15T00:00:00Z",
    }
    op.update(extra)
    return op


@respx.mock
def test_convert_follows_accepted_operation() -> None:
    respx.post(f"{BASE_URL}/v1/images").mock(
        return_value=httpx.Response(
            202, json={"operation_id": "op_1", "operation": image_op("running")}
        )
    )
    respx.get(f"{BASE_URL}/v1/operations/op_1").mock(
        return_value=httpx.Response(
            200, json=image_op("succeeded", status_code=202, result=converting_image())
        )
    )
    with fuse.Client(BASE_URL, "tok") as client:
        img = client.images.convert(fuse.ConvertImageRequest(ref="python:3.12"))

    assert img.digest == DIGEST
    assert img.state == fuse.IMAGE_CONVERTING


@respx.mock
def test_convert_followed_failure_raises() -> None:
    respx.post(f"{BASE_URL}/v1/images").mock(
        return_value=httpx.Response(
            202, json={"operation_id": "op_1", "operation": image_op("running")}
        )
    )
    respx.get(f"{BASE_URL}/v1/operations/op_1").mock(
        return_value=httpx.Response(
            200,
            json=image_op(
                "failed",
                status_code=507,
                error={"code": "unavailable", "message": "image cache full"},
            ),
        )
    )
    with fuse.Client(BASE_URL, "tok") as client, pytest.raises(fuse.ApiError) as err:
        client.images.convert(fuse.ConvertImageRequest(ref="python:3.12"))

    assert err.value.status == 507

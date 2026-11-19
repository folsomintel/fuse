from __future__ import annotations

import httpx
import pytest
import respx

import fuse

BASE_URL = "https://fuse.test"
DIGEST = "a" * 64


@respx.mock
def test_get_on_host_reads_that_hosts_record() -> None:
    route = respx.get(f"{BASE_URL}/v1/hosts/fc-2/images/{DIGEST}").mock(
        return_value=httpx.Response(
            200,
            json={
                "digest": DIGEST,
                "ref": "ubuntu",
                "manifest_digest": "sha256:x",
                "host_id": "fc-2",
                "state": "converting",
            },
        )
    )
    respx.get(f"{BASE_URL}/v1/hosts/gone/images/{DIGEST}").mock(
        return_value=httpx.Response(
            404, json={"error": {"code": "not_found", "message": 'host "gone" is not registered'}}
        )
    )
    with fuse.Client(BASE_URL, "tok") as client:
        img = client.images.get_on_host("fc-2", DIGEST)
        with pytest.raises(fuse.ApiError) as excinfo:
            client.images.get_on_host("gone", DIGEST)
        with pytest.raises(ValueError):
            client.images.get_on_host("", DIGEST)

    assert route.called
    assert img.host_id == "fc-2"
    assert img.state == fuse.IMAGE_CONVERTING
    assert fuse.is_not_found(excinfo.value)

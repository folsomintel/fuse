from __future__ import annotations

import httpx
import pytest
import respx

import fuse

BASE_URL = "https://fuse.test"


@respx.mock
def test_doctor_reads_report() -> None:
    route = respx.get(f"{BASE_URL}/v1/environments/vm-1/doctor").mock(
        return_value=httpx.Response(
            200,
            json={
                "status": "fail",
                "vm_id": "vm-1",
                "version": "v1.4.0",
                "checks": [
                    {"name": "display", "status": "ok", "message": ":1 1280x800"},
                    {"name": "vnc", "status": "fail", "message": "vnc server not reachable"},
                    {"name": "healthcheck", "status": "skip"},
                ],
            },
        )
    )
    with fuse.Client(BASE_URL, "tok") as client:
        res = client.environments.doctor("vm-1")
    assert route.called
    assert isinstance(res, fuse.DoctorReport)
    assert res.status == fuse.DOCTOR_FAIL
    assert res.version == "v1.4.0"
    assert [c.name for c in res.checks] == ["display", "vnc", "healthcheck"]
    assert res.checks[0].message == ":1 1280x800"
    assert res.checks[2].status == fuse.DOCTOR_SKIP


def test_doctor_validates_before_request() -> None:
    with fuse.Client(BASE_URL, "tok") as client:
        with pytest.raises(ValueError, match="vm id is required"):
            client.environments.doctor("")

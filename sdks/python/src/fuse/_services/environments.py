from __future__ import annotations

from collections.abc import Iterator
from urllib.parse import quote

from .._transport import Transport
from ..types import CreateRequest, EnvironmentInfo, Event
from .events import stream_events


class EnvironmentsService:
    def __init__(self, transport: Transport) -> None:
        self._t = transport

    def list(
        self, *, task_id: str = "", state: str = "", host_id: str = ""
    ) -> list[EnvironmentInfo]:
        resp = self._t.request(
            "GET",
            "/v1/environments",
            params={"task_id": task_id, "state": state, "host_id": host_id},
        )
        data = resp.json()
        return [
            EnvironmentInfo.model_validate(item)
            for item in (data.get("environments") or [])
        ]

    def get(self, vm_id: str) -> EnvironmentInfo:
        if not vm_id:
            raise ValueError("vm id is required")
        resp = self._t.request("GET", f"/v1/environments/{quote(vm_id, safe='')}")
        return EnvironmentInfo.model_validate(resp.json())

    def create(self, request: CreateRequest) -> EnvironmentInfo:
        resp = self._t.request("POST", "/v1/environments", body=request)
        return EnvironmentInfo.model_validate(resp.json())

    def drain(self, vm_id: str) -> EnvironmentInfo:
        return self._action(vm_id, "drain")

    def rotate_token(self, vm_id: str) -> None:
        if not vm_id:
            raise ValueError("vm id is required")
        path = f"/v1/environments/{quote(vm_id, safe='')}"
        self._t.request("POST", path, params={"action": "rotate-token"})

    def destroy(self, vm_id: str) -> None:
        if not vm_id:
            raise ValueError("vm id is required")
        self._t.request("DELETE", f"/v1/environments/{quote(vm_id, safe='')}")

    def events(self, vm_id: str) -> Iterator[Event]:
        # opens the sse stream and yields Event values. the iterator ends
        # cleanly on eof, after a terminal-state event, or after a final
        # Event whose err is set on a stream-level failure.
        return stream_events(self._t, vm_id)

    def _action(self, vm_id: str, action: str) -> EnvironmentInfo:
        if not vm_id:
            raise ValueError("vm id is required")
        if not action:
            raise ValueError("action is required")
        path = f"/v1/environments/{quote(vm_id, safe='')}"
        resp = self._t.request("POST", path, params={"action": action})
        return EnvironmentInfo.model_validate(resp.json())

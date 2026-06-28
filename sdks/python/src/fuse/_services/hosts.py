from __future__ import annotations

from urllib.parse import quote

from .._transport import Transport
from ..types import Host, RegisterHostRequest


class HostsService:
    def __init__(self, transport: Transport) -> None:
        self._t = transport

    def register(self, request: RegisterHostRequest) -> Host:
        resp = self._t.request("POST", "/v1/hosts", body=request)
        return Host.model_validate(resp.json())

    def list(self) -> list[Host]:
        resp = self._t.request("GET", "/v1/hosts")
        data = resp.json()
        return [Host.model_validate(item) for item in (data.get("hosts") or [])]

    def get(self, host_id: str) -> Host:
        if not host_id:
            raise ValueError("host id is required")
        resp = self._t.request("GET", f"/v1/hosts/{quote(host_id, safe='')}")
        return Host.model_validate(resp.json())

    def cordon(self, host_id: str) -> None:
        self._action(host_id, "cordon")

    def uncordon(self, host_id: str) -> None:
        self._action(host_id, "uncordon")

    def deregister(self, host_id: str) -> None:
        if not host_id:
            raise ValueError("host id is required")
        self._t.request("DELETE", f"/v1/hosts/{quote(host_id, safe='')}")

    def _action(self, host_id: str, action: str) -> None:
        if not host_id:
            raise ValueError("host id is required")
        if not action:
            raise ValueError("action is required")
        path = f"/v1/hosts/{quote(host_id, safe='')}"
        self._t.request("POST", path, params={"action": action})

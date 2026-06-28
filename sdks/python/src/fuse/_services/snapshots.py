from __future__ import annotations

from typing import Optional
from urllib.parse import quote

from .._transport import Transport
from ..types import Snapshot, SnapshotRequest


class SnapshotsService:
    def __init__(self, transport: Transport) -> None:
        self._t = transport

    def create(
        self, vm_id: str, request: Optional[SnapshotRequest] = None
    ) -> Snapshot:
        if not vm_id:
            raise ValueError("vm id is required")
        path = f"/v1/environments/{quote(vm_id, safe='')}/snapshots"
        resp = self._t.request("POST", path, body=request or SnapshotRequest())
        return Snapshot.model_validate(resp.json())

    def list(
        self,
        *,
        vm_id: str = "",
        task_id: str = "",
        tenant_id: str = "",
        state: str = "",
    ) -> list[Snapshot]:
        resp = self._t.request(
            "GET",
            "/v1/snapshots",
            params={
                "vm_id": vm_id,
                "task_id": task_id,
                "tenant_id": tenant_id,
                "state": state,
            },
        )
        data = resp.json()
        return [Snapshot.model_validate(item) for item in (data.get("snapshots") or [])]

    def get(self, snapshot_id: str) -> Snapshot:
        if not snapshot_id:
            raise ValueError("snapshot id is required")
        resp = self._t.request("GET", f"/v1/snapshots/{quote(snapshot_id, safe='')}")
        return Snapshot.model_validate(resp.json())

    def delete(self, snapshot_id: str) -> None:
        if not snapshot_id:
            raise ValueError("snapshot id is required")
        self._t.request("DELETE", f"/v1/snapshots/{quote(snapshot_id, safe='')}")

    def restore(self, snapshot_id: str) -> None:
        if not snapshot_id:
            raise ValueError("snapshot id is required")
        path = f"/v1/snapshots/{quote(snapshot_id, safe='')}"
        self._t.request("POST", path, params={"action": "restore"})

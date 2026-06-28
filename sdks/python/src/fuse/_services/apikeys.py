from __future__ import annotations

from urllib.parse import quote

from .._transport import Transport
from ..types import APIKey, CreatedAPIKey


class APIKeysService:
    # all api-key operations require the master token; the server enforces this.
    def __init__(self, transport: Transport) -> None:
        self._t = transport

    def create(self, label: str = "") -> CreatedAPIKey:
        # the raw secret is returned once in CreatedAPIKey.key, then unrecoverable.
        body = {"label": label} if label else {}
        resp = self._t.request("POST", "/v1/api-keys", body=body)
        return CreatedAPIKey.model_validate(resp.json())

    def list(self) -> list[APIKey]:
        resp = self._t.request("GET", "/v1/api-keys")
        data = resp.json()
        return [APIKey.model_validate(item) for item in (data.get("api_keys") or [])]

    def revoke(self, key_id: str) -> None:
        if not key_id:
            raise ValueError("id is required")
        self._t.request("DELETE", f"/v1/api-keys/{quote(key_id, safe='')}")

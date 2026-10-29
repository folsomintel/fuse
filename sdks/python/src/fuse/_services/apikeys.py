from __future__ import annotations

from typing import Any, Optional
from urllib.parse import quote

from .._transport import Transport
from ..types import ActorLabel, APIKey, CreatedAPIKey


class APIKeysService:
    # api-key operations require the master token, except that a scoped key
    # may mint keys within its own scopes. the server enforces this.
    def __init__(self, transport: Transport) -> None:
        self._t = transport

    def create(
        self,
        label: str = "",
        *,
        scopes: Optional[list[str]] = None,
        env_id: str = "",
        ttl_seconds: int = 0,
        actor: Optional[ActorLabel] = None,
    ) -> CreatedAPIKey:
        # the raw secret is returned once in CreatedAPIKey.key, then unrecoverable.
        # no scopes mints a full-access key. naming scopes (the SCOPE_HITL_*
        # constants) mints a key limited to the hitl routes, which must expire
        # (ttl_seconds, at most 86400) and may be limited to one environment
        # and its forks (env_id) and pinned to an actor.
        body: dict[str, Any] = {}
        if label:
            body["label"] = label
        if scopes:
            body["scopes"] = scopes
        if env_id:
            body["env_id"] = env_id
        if ttl_seconds > 0:
            body["ttl_seconds"] = ttl_seconds
        if actor is not None:
            body["actor"] = actor.model_dump(exclude_none=True)
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

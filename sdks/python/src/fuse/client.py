from __future__ import annotations

from typing import Callable, Optional

import httpx

from ._core import DEFAULT_TIMEOUT, DEFAULT_USER_AGENT
from ._services.apikeys import APIKeysService
from ._services.environments import EnvironmentsService
from ._services.hosts import HostsService
from ._services.interventions import InterventionsService
from ._services.snapshots import SnapshotsService
from ._transport import Transport
from .types import ActorLabel


class Client:
    # entry point for the fuse api. groups the resource services that share a
    # single transport. token is sent as a bearer token and may be empty for
    # endpoints that do not require auth. actor, when set, is sent as the
    # Fuse-Actor headers on every request.
    def __init__(
        self,
        base_url: str,
        token: str = "",
        *,
        http_client: Optional[httpx.Client] = None,
        user_agent: str = DEFAULT_USER_AGENT,
        request_id: Optional[Callable[[], str]] = None,
        timeout: float = DEFAULT_TIMEOUT,
        actor: Optional[ActorLabel] = None,
    ) -> None:
        if not base_url:
            raise ValueError("base url is required")
        parsed = httpx.URL(base_url)
        if not parsed.scheme or not parsed.host:
            raise ValueError("base url is invalid")

        self._t = Transport(
            base_url,
            token,
            http_client=http_client,
            user_agent=user_agent,
            request_id=request_id,
            timeout=timeout,
            actor=actor,
        )
        self._bind()

    def _bind(self) -> None:
        self.environments = EnvironmentsService(self._t)
        self.snapshots = SnapshotsService(self._t)
        self.hosts = HostsService(self._t)
        self.api_keys = APIKeysService(self._t)
        self.interventions = InterventionsService(self._t)

    def with_actor(self, actor: ActorLabel) -> Client:
        # returns a copy that sends actor as the Fuse-Actor headers, sharing
        # this client's connections and config. this client is unchanged.
        # close the original, not the copy, to release the connections.
        c = Client.__new__(Client)
        c._t = self._t.with_actor(actor)
        c._bind()
        return c

    def close(self) -> None:
        self._t.close()

    def __enter__(self) -> Client:
        return self

    def __exit__(self, *exc: object) -> None:
        self.close()

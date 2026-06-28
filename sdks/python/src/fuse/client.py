from __future__ import annotations

from typing import Callable, Optional

import httpx

from ._core import DEFAULT_TIMEOUT, DEFAULT_USER_AGENT
from ._services.apikeys import APIKeysService
from ._services.environments import EnvironmentsService
from ._services.hosts import HostsService
from ._services.snapshots import SnapshotsService
from ._transport import Transport


class Client:
    # entry point for the fuse api. groups the resource services that share a
    # single transport. token is sent as a bearer token and may be empty for
    # endpoints that do not require auth.
    def __init__(
        self,
        base_url: str,
        token: str = "",
        *,
        http_client: Optional[httpx.Client] = None,
        user_agent: str = DEFAULT_USER_AGENT,
        request_id: Optional[Callable[[], str]] = None,
        timeout: float = DEFAULT_TIMEOUT,
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
        )
        self.environments = EnvironmentsService(self._t)
        self.snapshots = SnapshotsService(self._t)
        self.hosts = HostsService(self._t)
        self.api_keys = APIKeysService(self._t)

    def close(self) -> None:
        self._t.close()

    def __enter__(self) -> Client:
        return self

    def __exit__(self, *exc: object) -> None:
        self.close()

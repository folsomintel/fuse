from __future__ import annotations

import copy
from collections.abc import Iterator
from typing import Any, Callable, Optional

import httpx

from ._core import (
    DEFAULT_TIMEOUT,
    DEFAULT_USER_AGENT,
    clean_params,
    iter_sse_data,
    serialize_body,
)
from .errors import REQUEST_ID_HEADER, check_response
from .types import ActorLabel

ACTOR_HEADER = "Fuse-Actor"
ACTOR_NAME_HEADER = "Fuse-Actor-Name"
# the server rejects a longer actor id.
MAX_ACTOR_ID_BYTES = 128


def check_actor(actor: ActorLabel) -> None:
    if not actor.id:
        raise ValueError("actor id is required")
    if len(actor.id.encode()) > MAX_ACTOR_ID_BYTES:
        raise ValueError(f"actor id is longer than {MAX_ACTOR_ID_BYTES} bytes")


class Transport:
    # shared http layer for all services. a separate no-timeout client is used
    # for long-lived sse streams so they are not killed by the request timeout.
    def __init__(
        self,
        base_url: str,
        token: str = "",
        *,
        http_client: Optional[httpx.Client] = None,
        stream_client: Optional[httpx.Client] = None,
        user_agent: str = DEFAULT_USER_AGENT,
        request_id: Optional[Callable[[], str]] = None,
        timeout: float = DEFAULT_TIMEOUT,
        actor: Optional[ActorLabel] = None,
    ) -> None:
        if actor is not None:
            check_actor(actor)
        self._actor = actor
        self._bearer = token
        self._user_agent = user_agent
        self._request_id = request_id
        # if a custom client is supplied, set its base_url yourself.
        self._owns_http = http_client is None
        self._owns_stream = stream_client is None
        self._http = http_client or httpx.Client(base_url=base_url, timeout=timeout)
        self._stream = stream_client or httpx.Client(base_url=base_url, timeout=None)

    def _headers(self, *, has_body: bool) -> dict[str, str]:
        headers: dict[str, str] = {}
        if has_body:
            headers["Content-Type"] = "application/json"
        if self._bearer:
            headers["Authorization"] = f"Bearer {self._bearer}"
        if self._user_agent:
            headers["User-Agent"] = self._user_agent
        if self._request_id is not None:
            rid = self._request_id()
            if rid:
                headers[REQUEST_ID_HEADER] = rid
        if self._actor is not None:
            headers[ACTOR_HEADER] = self._actor.id
            if self._actor.name:
                headers[ACTOR_NAME_HEADER] = self._actor.name
        return headers

    def with_actor(self, actor: ActorLabel) -> Transport:
        # a copy that shares the http clients but sends actor's headers. the
        # copy never closes the shared clients; the original owns them.
        check_actor(actor)
        t = copy.copy(self)
        t._actor = actor
        t._owns_http = False
        t._owns_stream = False
        return t

    def request(
        self,
        method: str,
        path: str,
        *,
        params: Optional[dict[str, Any]] = None,
        body: Any = None,
        timeout: Optional[float] = None,
    ) -> httpx.Response:
        # timeout overrides the client's default for this one request; a
        # long-poll uses it so a short client timeout cannot cut the hold.
        json_body = serialize_body(body)
        resp = self._http.request(
            method,
            path,
            params=clean_params(params),
            json=json_body,
            headers=self._headers(has_body=json_body is not None),
            timeout=timeout if timeout is not None else httpx.USE_CLIENT_DEFAULT,
        )
        check_response(resp)
        return resp

    def stream_sse(
        self,
        path: str,
        *,
        params: Optional[dict[str, Any]] = None,
    ) -> Iterator[str]:
        headers = self._headers(has_body=False)
        headers["Accept"] = "text/event-stream"
        with self._stream.stream(
            "GET", path, params=clean_params(params), headers=headers
        ) as resp:
            # on error, read the body so check_response can parse the envelope.
            if resp.status_code >= 300:
                resp.read()
                check_response(resp)
            yield from iter_sse_data(resp.iter_lines())

    def close(self) -> None:
        if self._owns_http:
            self._http.close()
        if self._owns_stream:
            self._stream.close()

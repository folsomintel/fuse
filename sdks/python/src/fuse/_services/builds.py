from __future__ import annotations

import time
import uuid
from typing import Optional
from urllib.parse import quote

import httpx

from .._transport import Transport
from ..types import Build, CreateBuildRequest
from .operations import _POLL_HTTP_HEADROOM, _POLL_WAIT_SECONDS, IDEMPOTENCY_KEY_HEADER


class BuildsService:
    # runs setup steps on a builder environment and snapshots the result.
    def __init__(self, transport: Transport) -> None:
        self._t = transport

    def create(
        self, request: CreateBuildRequest, *, idempotency_key: Optional[str] = None
    ) -> Build:
        # starts a build and returns it still running. without a key the call
        # sends a fresh random one; pass your own to retry safely.
        key = idempotency_key or str(uuid.uuid4())
        resp = self._t.request(
            "POST",
            "/v1/builds",
            body=request,
            headers={IDEMPOTENCY_KEY_HEADER: key},
        )
        return Build.model_validate(resp.json())

    def get(self, build_id: str, wait: Optional[float] = None) -> Build:
        # reports a build. wait, in seconds, attaches until it is no longer
        # running for at most that long (capped by the server); none answers
        # at once.
        return self._get(build_id, wait=wait)

    def wait(self, build_id: str, timeout: Optional[float] = None) -> Build:
        # blocks until the build finishes and returns it. a failed build is
        # returned, not raised: its error and output say why. timeout, in
        # seconds, bounds the wait and raises TimeoutError when it passes; the
        # build keeps running either way.
        deadline = None if timeout is None else time.monotonic() + timeout
        expired = f"build {build_id} still running after {timeout}s"
        while True:
            poll = _POLL_WAIT_SECONDS
            remaining = None
            if deadline is not None:
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    raise TimeoutError(expired)
                poll = min(poll, remaining)
            try:
                build = self._get(build_id, wait=poll, http_timeout=remaining)
            except httpx.TimeoutException as e:
                if deadline is not None and time.monotonic() >= deadline:
                    raise TimeoutError(expired) from e
                raise
            if build.done:
                return build

    def _get(
        self,
        build_id: str,
        *,
        wait: Optional[float] = None,
        http_timeout: Optional[float] = None,
    ) -> Build:
        # http_timeout, when given, caps a waiting read's http timeout.
        if not build_id:
            raise ValueError("build id is required")
        path = f"/v1/builds/{quote(build_id, safe='')}"
        if not wait or wait <= 0:
            resp = self._t.request("GET", path)
        else:
            limit = wait + _POLL_HTTP_HEADROOM
            if http_timeout is not None:
                limit = min(limit, http_timeout)
            resp = self._t.request("GET", path, params={"wait": f"{wait:g}s"}, timeout=limit)
        return Build.model_validate(resp.json())

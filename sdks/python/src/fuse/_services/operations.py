from __future__ import annotations

import json
import time
import uuid
from typing import Any, Optional
from urllib.parse import quote

import httpx

from .._retry import retry_unavailable
from .._transport import Transport
from ..errors import REQUEST_ID_HEADER, check_response
from ..types import Operation, OperationAccepted

IDEMPOTENCY_KEY_HEADER = "Idempotency-Key"
OPERATION_ID_HEADER = "Fuse-Operation-Id"

# how long one poll asks the server to hold while following an operation.
# the server caps it at its own attach bound.
_POLL_WAIT_SECONDS = 30.0
# headroom on top of a poll's hold for its http timeout, so a short client
# timeout cannot cut the hold.
_POLL_HTTP_HEADROOM = 30.0


class OperationsService:
    # reads the operations mutating calls run as.
    def __init__(self, transport: Transport) -> None:
        self._t = transport

    def get(self, operation_id: str, wait: Optional[float] = None) -> Operation:
        # reports an operation, finished or still running. wait, in seconds,
        # attaches until it finishes for at most that long (capped by the
        # server); none answers at once.
        return get_operation(self._t, operation_id, wait=wait)

    def wait(self, operation_id: str, timeout: Optional[float] = None) -> Operation:
        # blocks until the operation finishes and returns it. a failed
        # operation is returned, not raised: its error and status_code say
        # why. timeout, in seconds, bounds the wait and raises TimeoutError
        # when it passes; the operation keeps running either way.
        return wait_operation(self._t, operation_id, timeout=timeout)


def get_operation(
    t: Transport,
    operation_id: str,
    *,
    wait: Optional[float] = None,
    http_timeout: Optional[float] = None,
) -> Operation:
    # http_timeout, when given, caps a waiting read's http timeout.
    if not operation_id:
        raise ValueError("operation id is required")
    path = f"/v1/operations/{quote(operation_id, safe='')}"
    if not wait or wait <= 0:
        resp = t.request("GET", path)
    else:
        limit = wait + _POLL_HTTP_HEADROOM
        if http_timeout is not None:
            limit = min(limit, http_timeout)
        resp = t.request(
            "GET",
            path,
            params={"wait": f"{wait:g}s"},
            timeout=limit,
        )
    return Operation.model_validate(resp.json())


def wait_operation(
    t: Transport, operation_id: str, *, timeout: Optional[float] = None
) -> Operation:
    # polls the operation until it finishes.
    deadline = None if timeout is None else time.monotonic() + timeout
    expired = f"operation {operation_id} still running after {timeout}s"
    while True:
        poll = _POLL_WAIT_SECONDS
        remaining = None
        if deadline is not None:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise TimeoutError(expired)
            poll = min(poll, remaining)
        try:
            op = get_operation(t, operation_id, wait=poll, http_timeout=remaining)
        except httpx.TimeoutException as e:
            if deadline is not None and time.monotonic() >= deadline:
                raise TimeoutError(expired) from e
            raise
        if op.done:
            return op


def call_operation(
    t: Transport,
    method: str,
    path: str,
    *,
    params: Optional[dict[str, Any]] = None,
    body: Any = None,
    idempotency_key: Optional[str] = None,
) -> httpx.Response:
    # sends a mutating call with its Idempotency-Key and, when the server
    # answers 202 with an operation, follows it until it finishes. it returns
    # a response standing in for the one the call would have given directly,
    # so callers handle it exactly as before. without a key the call sends a
    # fresh random one. a 503 (no capacity, restate down) is sent again under
    # the same key, up to the client's retries.
    key = idempotency_key or str(uuid.uuid4())
    resp = retry_unavailable(
        t.retries, lambda: _call_operation_once(t, method, path, params, body, key)
    )
    check_response(resp)
    return resp


def _call_operation_once(
    t: Transport,
    method: str,
    path: str,
    params: Optional[dict[str, Any]],
    body: Any,
    key: str,
) -> httpx.Response:
    # one send of call_operation, following a 202 to its end. an error
    # answer is returned, not raised, so call_operation can retry a 503.
    resp = t.request(
        method,
        path,
        params=params,
        body=body,
        headers={IDEMPOTENCY_KEY_HEADER: key},
        check=False,
    )
    if resp.status_code != 202:
        return resp
    data = resp.json()
    if not isinstance(data, dict) or not data.get("operation_id"):
        # a hitl hold, not an operation: hand it back for read_held.
        return resp
    acc = OperationAccepted.model_validate(data)
    if t.on_operation is not None:
        t.on_operation(acc)
    op = wait_operation(t, acc.operation_id)
    return operation_response(op, resp.headers)


def operation_response(op: Operation, headers: httpx.Headers) -> httpx.Response:
    # rebuilds the answer a finished operation stands for.
    status = op.status_code or 500
    content = b""
    if 200 <= status < 300:
        if op.result is not None:
            content = json.dumps(op.result).encode()
    elif op.error is not None:
        content = json.dumps({"error": op.error.model_dump()}).encode()
    # carry only the ids over: the 202's own framing headers do not describe
    # this body.
    out_headers = {"Content-Type": "application/json"}
    for name in (REQUEST_ID_HEADER, OPERATION_ID_HEADER):
        value = headers.get(name)
        if value:
            out_headers[name] = value
    return httpx.Response(status, headers=out_headers, content=content)

from __future__ import annotations 

import http 
import json 
from typing import Optional 
import httpx 


__all__ = [
    "ApiError",
    "CODE_NOT_FOUND",
    "CODE_CONFLICT",
    "CODE_INVALID",
    "CODE_UNAVAILABLE",
    "CODE_INTERNAL"
    "CODE_UNAUTHORIZED",
    "as_api_error",
    "is_not_found",
    "is_conflict",
    "is_unauthorized",
    "is_invalid_argument",
    "is_unavailable",
    "check_response",
    "parse_api_error",
]

REQUEST_ID_HEADER = "X-Request-ID"
MAX_ERROR_BODY_BYTES = 1 << 20
CODE_NOT_FOUND = "not_found"
CODE_CONFLICT = "conflict"
CODE_INVALID_ARGUMENT = "invalid_argument"
CODE_UNAVAILABLE = "unavailable"
CODE_INTERNAL = "internal"
CODE_UNAUTHORIZED = "unauthorized"


class ApiError(Exception):
    def __init__(
            self,
            status: int,
            *,
            code: str = "",
            message: str = "",
            details: Optional[dict[str, str]] = None,
            request_id: str = "",
            body: bytes = b"",

    ) -> None:
        self.status = status 
        self.code = code 
        self.message = message 
        self.details: dict[str, str] = details or {}
        self.request_id = request_id 
        self.body = body 
        super().__init__(self._render())

    def _render(self) -> str:
        parts = [f"status={self.status}"]
        if self.code:
            parts.append(f"code={self.code}")
        if self.message:
            parts.append(self.message)
        else:
            text = _status_text(self.status)
            if text:
                parts.append(text)
        if self.request_id:
            parts.append(f"request_id{self.request_id}")
        return "fuse api error: " + ", ".join(parts)

    def __str__(self) -> str:
        return self._render()


def _status_text(status: int) -> str:
    try:
        return http.HTTPStatus(status).phrase.lower()
    except ValueError:
        return ""

def as_api_error(err: object) -> Optional[ApiError]:
    """Return the ApiError carried by ``err``, walking the exception chain.

    Mirrors Go's AsAPIError: returns None if no ApiError is present.
    """
    seen: set[int] = set()
    cur: object = err
    while isinstance(cur, BaseException) and id(cur) not in seen:
        if isinstance(cur, ApiError):
            return cur
        seen.add(id(cur))
        cur = cur.__cause__ or cur.__context__
    return None

def is_api_error(err: object, code: str) -> Optional[ApiError]:
    api_err = as_api_error(err)
    return api_err is not None and api_err.code == code

def is_not_found(err: object) -> bool:
    return _is_api_error_code(err, CODE_NOT_FOUND)

def is_conflict(err: object) -> bool:
    return _is_api_error_code(err, CODE_CONFLICT)

def is_unauthroized(err: object) -> bool:
    return _is_api_error_code(err, CODE_UNAUTHORIZED)

def is_invalid_argument(err: object) -> bool:
    return _is_api_error_code(err, CODE_INVALID_ARGUMENT)

def is_unavailable(err: object) -> bool:
    return _is_api_error_code(err, CODE_UNAVAILABLE)

def check_response(response: httpx.Response) -> None:
    if 200 <= response.status_code < 300:
        return 
    try:
        body = respones.content[:MAX_ERROR_BODY_BYTES]
    except httpx.ResponseNotRead:
        body = b""
    raise parse_api_error(response.status_code, response.headers, body)

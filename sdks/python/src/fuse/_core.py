from __future__ import annotations

from collections.abc import Iterable, Iterator
from importlib.metadata import PackageNotFoundError
from importlib.metadata import version as _pkg_version
from typing import Any, Optional

from pydantic import BaseModel

# sdk version, read from installed package metadata so it never drifts from
# pyproject. falls back when running from an uninstalled source tree.
try:
    VERSION = _pkg_version("folsom-fuse")
except PackageNotFoundError:
    VERSION = "0+unknown"
DEFAULT_TIMEOUT = 60.0
DEFAULT_USER_AGENT = f"fuse-python/{VERSION}"


def clean_params(params: Optional[dict[str, Any]]) -> Optional[dict[str, Any]]:
    # drop None/empty values so we only send set filters, mirroring the go sdk.
    if not params:
        return None
    out: dict[str, Any] = {}
    for key, value in params.items():
        if value is None or value == "":
            continue
        out[key] = value
    return out or None


def serialize_body(body: Any) -> Optional[Any]:
    # pydantic models serialize with aliases and omit unset (None) fields,
    # matching go's json omitempty behavior for request bodies.
    if body is None:
        return None
    if isinstance(body, BaseModel):
        return body.model_dump(by_alias=True, exclude_none=True)
    return body


def iter_sse_data(lines: Iterable[str]) -> Iterator[str]:
    # turn a stream of sse lines into a stream of data payloads. a blank line
    # terminates an event; comment lines (":") and non-data fields are ignored.
    data: list[str] = []
    has_data = False
    for line in lines:
        if line == "":
            if not has_data:
                continue
            yield "".join(data)
            data = []
            has_data = False
        elif line.startswith(":"):
            continue
        elif line.startswith("data:"):
            payload = line[len("data:"):]
            if payload.startswith(" "):
                payload = payload[1:]
            data.append(payload)
            has_data = True
        else:
            continue

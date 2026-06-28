from __future__ import annotations

from collections.abc import Iterator
from urllib.parse import quote

import httpx
from pydantic import ValidationError

from .._transport import Transport
from ..types import Event, is_terminal_state


def stream_events(transport: Transport, vm_id: str) -> Iterator[Event]:
    # low-level sse driver for EnvironmentsService.events. an ApiError from the
    # initial response propagates directly; mid-stream transport errors are
    # delivered as a final Event with err set, mirroring the go sdk channel.
    if not vm_id:
        raise ValueError("vm id is required")
    path = f"/v1/environments/{quote(vm_id, safe='')}/events"
    try:
        for data in transport.stream_sse(path):
            try:
                event = Event.model_validate_json(data)
            except ValidationError as exc:
                yield Event(err=exc)
                return
            yield event
            if is_terminal_state(event.state):
                return
    except httpx.HTTPError as exc:
        yield Event(err=exc)

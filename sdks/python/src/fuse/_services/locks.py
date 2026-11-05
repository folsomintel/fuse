from __future__ import annotations

from typing import Callable, TypeVar

from .._transport import Transport
from ..errors import LockedError
from .interventions import wait_intervention

T = TypeVar("T")


def call_unlocked(t: Transport, call: Callable[[], T], *, wait: bool) -> T:
    # runs call, and while a human holds the environment (a 423 locked for a
    # takeover or a pending stall) waits for that intervention to end, then
    # runs it again. a new lock waits again. the server bounds every lock by
    # the intervention deadline, so this ends. wait=False raises the
    # LockedError at once instead.
    while True:
        try:
            return call()
        except LockedError as err:
            if not wait or not err.intervention_id:
                raise
            wait_intervention(t, err.intervention_id)

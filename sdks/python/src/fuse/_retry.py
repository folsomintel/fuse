from __future__ import annotations

import random
import time
from typing import Callable, Optional

import httpx

# how many times a keyed call answered 503 is sent again by default.
DEFAULT_RETRIES = 3
# backoff of a 503 without Retry-After: 1s doubling, capped at 30s.
_RETRY_BASE = 1.0
_RETRY_CAP = 30.0
# a server's Retry-After is capped at a minute.
_MAX_RETRY_AFTER = 60.0


def retry_delay(
    retry_after: Optional[str],
    attempt: int,
    rand: Callable[[], float] = random.random,
) -> float:
    # seconds to wait before retry number attempt (from 0) of a 503 whose
    # Retry-After header is retry_after: the header's seconds when it has
    # them, else the backoff, plus up to half again as jitter. jitter only
    # ever adds, so the wait never undercuts the server's Retry-After.
    d: float = min(_RETRY_BASE * 2.0 ** min(attempt, 10), _RETRY_CAP)
    if retry_after is not None and retry_after.strip().isdigit():
        d = min(float(retry_after.strip()), _MAX_RETRY_AFTER)
    return d + rand() * d / 2


def retry_unavailable(
    retries: int,
    send: Callable[[], httpx.Response],
    sleep: Callable[[float], None] = time.sleep,
) -> httpx.Response:
    # calls send, and while its answer is a 503 and retries remain, waits and
    # calls it again. send must send the same request under the same
    # Idempotency-Key, so the retry is safe.
    attempt = 0
    while True:
        resp = send()
        if resp.status_code != 503 or attempt >= retries:
            return resp
        sleep(retry_delay(resp.headers.get("Retry-After"), attempt))
        attempt += 1

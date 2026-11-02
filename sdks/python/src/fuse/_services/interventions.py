from __future__ import annotations

from typing import Optional
from urllib.parse import quote

from .._transport import Transport
from ..types import Intervention, InterventionPage

# the server's max page size; list() requests this internally so it walks
# every page in as few round trips as possible.
_MAX_PAGE_LIMIT = 200

# how long one long-poll asks the server to hold. the server caps it at 50s.
_WAIT_POLL_SECONDS = 30
# the http timeout for one long-poll: the hold plus headroom, so it does not
# depend on the client's default timeout.
_WAIT_HTTP_TIMEOUT = _WAIT_POLL_SECONDS + 30.0


class InterventionsService:
    # reads and decides hitl interventions.
    def __init__(self, transport: Transport) -> None:
        self._t = transport

    def list(
        self, *, env_id: str = "", state: str = "", cursor: str = ""
    ) -> list[Intervention]:
        # returns every intervention matching the filters, oldest first,
        # transparently walking every result page. for explicit single-page
        # control, use list_page.
        out: list[Intervention] = []
        while True:
            page = self.list_page(
                env_id=env_id, state=state, limit=_MAX_PAGE_LIMIT, cursor=cursor
            )
            out.extend(page.interventions)
            if not page.next_cursor:
                break
            cursor = page.next_cursor
        return out

    def list_page(
        self, *, env_id: str = "", state: str = "", limit: int = 0, cursor: str = ""
    ) -> InterventionPage:
        # returns one page of interventions matching the filters.
        params: dict[str, str] = {"env": env_id, "state": state}
        if limit > 0:
            params["limit"] = str(limit)
        if cursor:
            params["cursor"] = cursor
        resp = self._t.request("GET", "/v1/interventions", params=params)
        return InterventionPage.model_validate(resp.json())

    def get(self, intervention_id: str) -> Intervention:
        # returns one intervention without waiting.
        return get_intervention(self._t, intervention_id, wait=False)

    def wait(self, intervention_id: str) -> Intervention:
        # blocks until the intervention resolves. every intervention has a
        # deadline, so this always returns eventually. state lives in the
        # server's database, not the connection, so each poll is independent.
        return wait_intervention(self._t, intervention_id)

    def approve(
        self, intervention_id: str, answer: str, reason: Optional[str] = None
    ) -> Intervention:
        # resolves a pending intervention as approved. an ask needs an answer
        # (one of its choices, when it has any). requires the master token.
        return self._decide(intervention_id, "approve", answer, reason)

    def reject(self, intervention_id: str, reason: Optional[str] = None) -> Intervention:
        # resolves a pending intervention as rejected. the reason is fed back
        # to the agent; the server fills one in when empty. requires the
        # master token.
        return self._decide(intervention_id, "reject", None, reason)

    def _decide(
        self,
        intervention_id: str,
        action: str,
        answer: Optional[str],
        reason: Optional[str],
    ) -> Intervention:
        if not intervention_id:
            raise ValueError("intervention id is required")
        body: dict[str, str] = {}
        if answer:
            body["answer"] = answer
        if reason:
            body["reason"] = reason
        path = f"/v1/interventions/{quote(intervention_id, safe='')}"
        resp = self._t.request("POST", path, params={"action": action}, body=body)
        return Intervention.model_validate(resp.json())


def get_intervention(t: Transport, intervention_id: str, *, wait: bool) -> Intervention:
    # fetches one intervention, long-polling for a change when wait is set.
    if not intervention_id:
        raise ValueError("intervention id is required")
    path = f"/v1/interventions/{quote(intervention_id, safe='')}"
    if not wait:
        resp = t.request("GET", path)
    else:
        resp = t.request(
            "GET",
            path,
            params={"wait": f"{_WAIT_POLL_SECONDS}s"},
            timeout=_WAIT_HTTP_TIMEOUT,
        )
    return Intervention.model_validate(resp.json())


def wait_intervention(t: Transport, intervention_id: str) -> Intervention:
    # long-polls until the intervention is terminal.
    while True:
        intervention = get_intervention(t, intervention_id, wait=True)
        if intervention.terminal:
            return intervention

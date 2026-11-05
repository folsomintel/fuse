from __future__ import annotations

from collections.abc import Sequence
from typing import Any, Callable, Optional
from urllib.parse import quote

import httpx

from .._transport import Transport
from ..errors import HeldActionError, InterventionRejectedError, PendingInterventionError
from ..types import AuditEntry, AuditPage, Intervention, InterventionPage

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

    def audit(
        self, *, env_id: str = "", actor: str = "", since: str = "", cursor: str = ""
    ) -> list[AuditEntry]:
        # returns every hitl audit log entry matching the filters, oldest
        # first, walking every result page. for one page, use audit_page.
        out: list[AuditEntry] = []
        while True:
            page = self.audit_page(
                env_id=env_id,
                actor=actor,
                since=since,
                limit=_MAX_PAGE_LIMIT,
                cursor=cursor,
            )
            out.extend(page.entries)
            if not page.next_cursor:
                break
            cursor = page.next_cursor
        return out

    def audit_page(
        self,
        *,
        env_id: str = "",
        actor: str = "",
        since: str = "",
        limit: int = 0,
        cursor: str = "",
    ) -> AuditPage:
        # returns one page of the hitl audit log. since is an rfc 3339 time or
        # a go duration like "24h" meaning that long ago. needs hitl:read.
        params: dict[str, str] = {"env": env_id, "actor": actor, "since": since}
        if limit > 0:
            params["limit"] = str(limit)
        if cursor:
            params["cursor"] = cursor
        resp = self._t.request("GET", "/v1/hitl/audit", params=params)
        return AuditPage.model_validate(resp.json())

    def get_recording(self, recording_id: str) -> str:
        # returns a recorded takeover session as asciinema v2 cast text. the
        # id comes from a takeover_session audit entry's recording_id. needs
        # hitl:read.
        if not recording_id:
            raise ValueError("recording id is required")
        path = f"/v1/hitl/recordings/{quote(recording_id, safe='')}"
        return self._t.request("GET", path).text

    def list(
        self, *, env_id: str = "", state: str = "", actor: str = "", cursor: str = ""
    ) -> list[Intervention]:
        # returns every intervention matching the filters, oldest first,
        # transparently walking every result page. for explicit single-page
        # control, use list_page.
        out: list[Intervention] = []
        while True:
            page = self.list_page(
                env_id=env_id,
                state=state,
                actor=actor,
                limit=_MAX_PAGE_LIMIT,
                cursor=cursor,
            )
            out.extend(page.interventions)
            if not page.next_cursor:
                break
            cursor = page.next_cursor
        return out

    def list_page(
        self,
        *,
        env_id: str = "",
        state: str = "",
        actor: str = "",
        limit: int = 0,
        cursor: str = "",
    ) -> InterventionPage:
        # returns one page of interventions matching the filters. actor is an
        # actor id that raised or decided the intervention.
        params: dict[str, str] = {"env": env_id, "state": state, "actor": actor}
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
        # blocks until the intervention settles: resolved, and for an approved
        # gated action, performed, with the outcome on its result. every
        # intervention has a deadline, so this always returns eventually. state lives in the
        # server's database, not the connection, so each poll is independent.
        return wait_intervention(self._t, intervention_id)

    def approve(
        self,
        intervention_id: str,
        answer: Optional[str] = None,
        reason: Optional[str] = None,
        *,
        edited_argv: Optional[Sequence[str]] = None,
        edited_text: Optional[str] = None,
        grant: Optional[str] = None,
        extend_seconds: Optional[int] = None,
    ) -> Intervention:
        # resolves a pending intervention as approved. an ask needs an answer
        # (one of its choices, when it has any); a stall needs none.
        # requires hitl:decide or the master token, and the key or actor that
        # raised it may not decide it.
        #
        # for a held exec or lifecycle action, the orchestrator then performs
        # it; wait on the id for its result. edited_argv replaces a held
        # exec's argv and edited_text a held computer "type" action's text
        # (each re-checked against the deny gates, and not combinable with
        # grant). grant is "once", "env" or "ttl:<duration>"; none takes
        # the gate's default. extend_seconds is how long approving an expiry
        # keeps the environment alive; none is one more window of the reaper
        # that was due.
        body: dict[str, Any] = {}
        if answer:
            body["answer"] = answer
        if reason:
            body["reason"] = reason
        if edited_argv:
            body["edited_argv"] = list(edited_argv)
        if edited_text is not None:
            body["edited_text"] = edited_text
        if grant:
            body["grant"] = grant
        if extend_seconds:
            body["extend_seconds"] = extend_seconds
        return self._decide(intervention_id, "approve", body)

    def answer_secret(self, intervention_id: str, value: str) -> Intervention:
        # answers a pending secret request: the value is written to the
        # declared target in the guest and dropped, never stored or returned.
        # needs hitl:secret.
        if not intervention_id:
            raise ValueError("intervention id is required")
        path = f"/v1/interventions/{quote(intervention_id, safe='')}"
        resp = self._t.request("POST", path, params={"action": "secret"}, body={"value": value})
        return Intervention.model_validate(resp.json())

    def reject(self, intervention_id: str, reason: Optional[str] = None) -> Intervention:
        # resolves a pending intervention as rejected. the reason is fed back
        # to the agent; the server fills one in when empty. requires
        # hitl:decide or the master token.
        return self._decide(intervention_id, "reject", {"reason": reason} if reason else {})

    def _decide(self, intervention_id: str, action: str, body: dict[str, Any]) -> Intervention:
        if not intervention_id:
            raise ValueError("intervention id is required")
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
    # long-polls until the intervention is settled, not just terminal: an
    # approved gated action is only done once its result is recorded.
    while True:
        intervention = get_intervention(t, intervention_id, wait=True)
        if intervention.settled:
            return intervention


def read_held(resp: httpx.Response) -> Optional[Intervention]:
    # returns the held intervention of a 202 response, or None when the
    # response is the action's own answer.
    if resp.status_code != 202:
        return None
    return Intervention.model_validate(resp.json()["intervention"])


def settle_held(
    t: Transport,
    held: Intervention,
    *,
    wait: bool = True,
    on_held: Optional[Callable[[Intervention], None]] = None,
) -> Intervention:
    # waits out a held call: it reports the hold, waits until the
    # intervention settles, and turns anything but a successful run into an
    # error. the per-call on_held overrides the client's.
    callback = on_held or t.on_held
    if callback is not None:
        callback(held)
    if not wait or not t.hitl_wait:
        raise PendingInterventionError(held)
    intervention = wait_intervention(t, held.id)
    if not intervention.approved:
        raise InterventionRejectedError(intervention)
    if intervention.result is not None and intervention.result.error:
        raise HeldActionError(intervention)
    return intervention

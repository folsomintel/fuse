from __future__ import annotations

from collections.abc import Iterator, Sequence
from typing import Any, Callable, Optional
from urllib.parse import quote

from .._transport import Transport
from ..errors import ApiError, InterventionRejectedError, is_hitl_denied
from ..types import (
    HITL_KIND_ASK,
    HITL_KIND_REVIEW,
    ComputerAction,
    ComputerDisplay,
    ComputerResult,
    ComputerToolResult,
    CreateRequest,
    EnvironmentInfo,
    EnvironmentPage,
    Event,
    ExecRequest,
    ExecResult,
    ForkOptions,
    HumanRequest,
    Intervention,
    MigrateOptions,
    ReleaseRequest,
    ReviewResult,
    TakeoverRequest,
)
from .events import stream_events
from .interventions import read_held, settle_held, wait_intervention
from .locks import call_unlocked

# the server's max page size; list() requests this internally so it walks
# every page in as few round trips as possible.
_MAX_PAGE_LIMIT = 200


class EnvironmentsService:
    def __init__(self, transport: Transport) -> None:
        self._t = transport

    def list(
        self, *, task_id: str = "", state: str = "", host_id: str = "", cursor: str = ""
    ) -> list[EnvironmentInfo]:
        # returns every environment matching the filters, transparently
        # walking every result page. for explicit single-page control (e.g.
        # a cursor from a previous call), use list_page.
        out: list[EnvironmentInfo] = []
        while True:
            page = self.list_page(
                task_id=task_id,
                state=state,
                host_id=host_id,
                limit=_MAX_PAGE_LIMIT,
                cursor=cursor,
            )
            out.extend(page.environments)
            if not page.next_cursor:
                break
            cursor = page.next_cursor
        return out

    def list_page(
        self,
        *,
        task_id: str = "",
        state: str = "",
        host_id: str = "",
        limit: int = 0,
        cursor: str = "",
    ) -> EnvironmentPage:
        # returns one page of environments matching the filters.
        params: dict[str, str] = {"task_id": task_id, "state": state, "host_id": host_id}
        if limit > 0:
            params["limit"] = str(limit)
        if cursor:
            params["cursor"] = cursor
        resp = self._t.request("GET", "/v1/environments", params=params)
        return EnvironmentPage.model_validate(resp.json())

    def get(self, vm_id: str) -> EnvironmentInfo:
        if not vm_id:
            raise ValueError("vm id is required")
        resp = self._t.request("GET", f"/v1/environments/{quote(vm_id, safe='')}")
        return EnvironmentInfo.model_validate(resp.json())

    def create(self, request: CreateRequest) -> EnvironmentInfo:
        resp = self._t.request("POST", "/v1/environments", body=request)
        return EnvironmentInfo.model_validate(resp.json())

    def drain(self, vm_id: str) -> EnvironmentInfo:
        # phase-1 only: this quiesces the guest workload and moves the vm to
        # "draining" so you can inspect its output, but it does not destroy
        # the vm. call environments.destroy(vm_id) afterward to remove it -
        # a drained vm left undestroyed keeps running (and billing) forever.
        return self._action(vm_id, "drain")

    def fork(self, vm_id: str, options: ForkOptions | None = None) -> EnvironmentInfo:
        if not vm_id:
            raise ValueError("vm id is required")
        path = f"/v1/environments/{quote(vm_id, safe='')}"
        resp = self._t.request(
            "POST", path, params={"action": "fork"}, body=options or ForkOptions()
        )
        held = read_held(resp)
        if held is not None:
            return self._held_environment(held)
        return EnvironmentInfo.model_validate(resp.json())

    def migrate(self, vm_id: str, options: MigrateOptions | None = None) -> EnvironmentInfo:
        # moves the environment to another host and returns the new one. the
        # source is drained and destroyed once the new one is running.
        if not vm_id:
            raise ValueError("vm id is required")
        path = f"/v1/environments/{quote(vm_id, safe='')}"
        resp = self._t.request(
            "POST", path, params={"action": "migrate"}, body=options or MigrateOptions()
        )
        held = read_held(resp)
        if held is not None:
            return self._held_environment(held)
        return EnvironmentInfo.model_validate(resp.json())

    def computer(
        self,
        vm_id: str,
        action: ComputerAction,
        *,
        wait: bool = True,
        on_held: Optional[Callable[[Intervention], None]] = None,
    ) -> ComputerResult:
        # relays one computer-use action to the environment's desktop and
        # returns the result, usually carrying a base64 png screenshot.
        # requires an environment booted from a desktop image; on any other
        # image the server answers 503 with a reason.
        #
        # when a computer gate holds the action, this waits for the decision
        # like exec does; the orchestrator performs the approved action (with
        # the human's edited text, if any) and its result is returned. on_held
        # is called with the pending intervention first. wait=False raises
        # PendingInterventionError at once instead, and a rejection raises
        # InterventionRejectedError.
        # while a human holds the environment this waits for them to finish
        # and then sends the action; wait=False raises LockedError instead.
        if not vm_id:
            raise ValueError("vm id is required")
        if not action.action:
            raise ValueError("action is required")
        path = f"/v1/environments/{quote(vm_id, safe='')}/computer"
        resp = call_unlocked(self._t, lambda: self._t.request("POST", path, body=action), wait=wait)
        held = read_held(resp)
        if held is not None:
            return self._held_computer(held, wait=wait, on_held=on_held)
        return ComputerResult.model_validate(resp.json())

    def computer_tool_result(
        self, vm_id: str, tool_input: dict[str, Any], *, wait: bool = True
    ) -> ComputerToolResult:
        # executes one computer tool_use against the environment and shapes
        # the answer for the messages api. tool_input is the tool_use block's
        # input, verbatim: it is already the wire shape the computer endpoint
        # takes, and it is forwarded untouched so action fields this sdk has
        # not learned yet still reach the guest.
        #
        # an action the environment refuses (a malformed coordinate, a
        # display that is not up) comes back as is_error content rather than
        # raising: the agent loop should feed it to the model, which is the
        # party that can correct it. a raise is reserved for failures the
        # model cannot fix - transport, auth, an unknown environment. a hitl
        # gate's deny, or a human rejecting a held action, is also is_error
        # content: the model should hear why and try something else.
        #
        # like computer, this waits out a human takeover and a held action
        # unless wait=False, which raises LockedError or
        # PendingInterventionError.
        if not vm_id:
            raise ValueError("vm id is required")
        if not tool_input.get("action"):
            return ComputerToolResult(
                content=[{"type": "text", "text": "tool input names no action"}],
                is_error=True,
            )
        path = f"/v1/environments/{quote(vm_id, safe='')}/computer"
        try:
            resp = call_unlocked(
                self._t, lambda: self._t.request("POST", path, body=tool_input), wait=wait
            )
        except ApiError as err:
            if err.status in (400, 409, 503) or is_hitl_denied(err):
                return _tool_error(err.message or "the computer action failed")
            raise
        held = read_held(resp)
        if held is not None:
            try:
                data = self._held_computer(held, wait=wait)
            except InterventionRejectedError as err:
                return _tool_error(f"a human rejected this action: {err}")
        else:
            data = ComputerResult.model_validate(resp.json())
        content: list[dict[str, Any]] = []
        if data.output:
            content.append({"type": "text", "text": data.output})
        if data.screenshot:
            content.append(
                {
                    "type": "image",
                    "source": {
                        "type": "base64",
                        "media_type": "image/png",
                        "data": data.screenshot,
                    },
                }
            )
        if not content:
            content.append({"type": "text", "text": "done"})
        return ComputerToolResult(content=content)

    def computer_display(self, vm_id: str) -> ComputerDisplay:
        # reports whether the environment has a live display and at what
        # geometry. up is false with a reason on an image with no desktop.
        if not vm_id:
            raise ValueError("vm id is required")
        path = f"/v1/environments/{quote(vm_id, safe='')}/computer"
        resp = self._t.request("GET", path)
        return ComputerDisplay.model_validate(resp.json())

    def exec(
        self,
        vm_id: str,
        request: ExecRequest,
        *,
        wait: bool = True,
        on_held: Optional[Callable[[Intervention], None]] = None,
    ) -> ExecResult:
        # runs a command inside a running environment's guest. requires the
        # master token.
        #
        # a non-zero exit_code is returned, not raised: the command ran and
        # failed. an ApiError means the command could not be run at all.
        #
        # when a hitl gate holds the exec, this waits for the decision; the
        # orchestrator runs the approved (possibly edited) argv and its result
        # is returned. on_held is called with the pending intervention first,
        # overriding the client's. wait=False raises PendingInterventionError
        # at once instead. a rejection raises InterventionRejectedError and a
        # deny gate a ForbiddenError that is_hitl_denied matches.
        # while a human holds the environment (a takeover, or a pending
        # stall) this waits for them to finish and then runs the command, so
        # agent code needs no changes. wait=False raises LockedError instead.
        if not vm_id:
            raise ValueError("vm id is required")
        has_cmd = bool(request.cmd)
        has_shell = bool(request.shell)
        if not has_cmd and not has_shell:
            raise ValueError("one of cmd or shell is required")
        if has_cmd and has_shell:
            raise ValueError("cmd and shell are mutually exclusive")
        path = f"/v1/environments/{quote(vm_id, safe='')}"
        resp = call_unlocked(
            self._t,
            lambda: self._t.request("POST", path, params={"action": "exec"}, body=request),
            wait=wait,
        )
        held = read_held(resp)
        if held is not None:
            settled = settle_held(self._t, held, wait=wait, on_held=on_held)
            if settled.result is None or settled.result.exec is None:
                raise RuntimeError(f"intervention {settled.id} settled without an exec result")
            return settled.result.exec
        return ExecResult.model_validate(resp.json())

    def request_human(
        self,
        vm_id: str,
        question: str = "",
        *,
        kind: str = HITL_KIND_ASK,
        choices: Optional[Sequence[str]] = None,
        target: Optional[str] = None,
        message: Optional[str] = None,
        via: Optional[str] = None,
        reason: Optional[str] = None,
        path: Optional[str] = None,
        content: Optional[str] = None,
        format: Optional[str] = None,
        timeout_seconds: Optional[int] = None,
        wait: bool = True,
    ) -> Intervention:
        # asks a human for something the environment's hitl policy declares.
        # by default it blocks until the intervention resolves (answered,
        # rejected, expired or cancelled); read the result off the returned
        # intervention's decision. wait=False returns the pending handle at
        # once; wait on it later with client.interventions.wait.
        # timeout_seconds shortens the policy's deadline, never extends it.
        #
        # an ask takes question and choices. kind="secret" takes target (a
        # declared env name or file path) and an optional message; once a
        # human answers, the value is in the guest and decision.answer is
        # "done". kind="takeover" takes via and reason; on release
        # decision.answer is the human's note. kind="review" takes path or
        # content, format and message; see request_review.
        if not vm_id:
            raise ValueError("vm id is required")
        request = HumanRequest(
            kind=kind or HITL_KIND_ASK,
            question=question or None,
            choices=list(choices) if choices is not None else None,
            target=target or None,
            message=message or None,
            via=via or None,
            reason=reason or None,
            path=path or None,
            content=content or None,
            format=format or None,
            timeout_seconds=timeout_seconds,
        )
        env_path = f"/v1/environments/{quote(vm_id, safe='')}"
        resp = self._t.request("POST", env_path, params={"action": "request"}, body=request)
        intervention = Intervention.model_validate(resp.json())
        if not wait:
            return intervention
        return wait_intervention(self._t, intervention.id)

    def request_review(
        self,
        vm_id: str,
        *,
        path: Optional[str] = None,
        content: Optional[str] = None,
        format: Optional[str] = None,
        message: Optional[str] = None,
        timeout_seconds: Optional[int] = None,
        wait: bool = True,
    ) -> ReviewResult:
        # submits a guest file (path, absolute) or a text/diff blob (content)
        # for a human to approve or reject with comments, and waits for the
        # verdict. exactly one of path and content. format is "text" or
        # "diff", none to guess. it needs hitl.requests.review. wait=False
        # returns at once with the pending intervention and approved false.
        intervention = self.request_human(
            vm_id,
            kind=HITL_KIND_REVIEW,
            path=path,
            content=content,
            format=format,
            message=message,
            timeout_seconds=timeout_seconds,
            wait=wait,
        )
        return ReviewResult(
            approved=intervention.approved,
            comments=intervention.decision.reason if intervention.decision else "",
            intervention=intervention,
        )

    def takeover(
        self, vm_id: str, *, via: Optional[str] = None, reason: Optional[str] = None
    ) -> Intervention:
        # a human takes over the environment and returns the held takeover.
        # it claims the agent's pending takeover request if there is one, else
        # opens a new one. while held, agent exec and computer calls wait.
        # needs hitl:takeover; a conflict if someone else holds it.
        if not vm_id:
            raise ValueError("vm id is required")
        path = f"/v1/environments/{quote(vm_id, safe='')}"
        resp = self._t.request(
            "POST",
            path,
            params={"action": "takeover"},
            body=TakeoverRequest(via=via or None, reason=reason or None),
        )
        return Intervention.model_validate(resp.json())

    def release(self, vm_id: str, *, note: Optional[str] = None) -> Intervention:
        # ends a takeover and returns it, approved with note as its answer.
        # the agent's waiting calls then go through. a conflict if nothing is
        # held.
        if not vm_id:
            raise ValueError("vm id is required")
        path = f"/v1/environments/{quote(vm_id, safe='')}"
        resp = self._t.request(
            "POST", path, params={"action": "release"}, body=ReleaseRequest(note=note or None)
        )
        return Intervention.model_validate(resp.json())

    def rotate_token(self, vm_id: str) -> None:
        if not vm_id:
            raise ValueError("vm id is required")
        path = f"/v1/environments/{quote(vm_id, safe='')}"
        resp = self._t.request("POST", path, params={"action": "rotate-token"})
        held = read_held(resp)
        if held is not None:
            settle_held(self._t, held)

    def destroy(self, vm_id: str) -> None:
        if not vm_id:
            raise ValueError("vm id is required")
        resp = self._t.request("DELETE", f"/v1/environments/{quote(vm_id, safe='')}")
        held = read_held(resp)
        if held is not None:
            settle_held(self._t, held)

    def events(self, vm_id: str) -> Iterator[Event]:
        # opens the sse stream and yields Event values. the iterator ends
        # cleanly on eof, after a terminal-state event, or after a final
        # Event whose err is set on a stream-level failure.
        return stream_events(self._t, vm_id)

    def _action(self, vm_id: str, action: str) -> EnvironmentInfo:
        if not vm_id:
            raise ValueError("vm id is required")
        if not action:
            raise ValueError("action is required")
        path = f"/v1/environments/{quote(vm_id, safe='')}"
        resp = self._t.request("POST", path, params={"action": action})
        held = read_held(resp)
        if held is not None:
            settle_held(self._t, held)
            return self.get(vm_id)
        return EnvironmentInfo.model_validate(resp.json())

    def _held_computer(
        self,
        held: Intervention,
        *,
        wait: bool = True,
        on_held: Optional[Callable[[Intervention], None]] = None,
    ) -> ComputerResult:
        # waits out a held computer action and returns what the orchestrator
        # got performing it.
        settled = settle_held(self._t, held, wait=wait, on_held=on_held)
        if settled.result is None or settled.result.computer is None:
            raise RuntimeError(f"intervention {settled.id} settled without a computer result")
        return settled.result.computer

    def _held_environment(self, held: Intervention) -> EnvironmentInfo:
        # waits out a held fork or migrate and returns the environment it
        # produced.
        settled = settle_held(self._t, held)
        if settled.result is None or not settled.result.env_id:
            raise RuntimeError(f"intervention {settled.id} settled without an environment")
        return self.get(settled.result.env_id)


def _tool_error(text: str) -> ComputerToolResult:
    # one is_error text block for the model.
    return ComputerToolResult(content=[{"type": "text", "text": text}], is_error=True)

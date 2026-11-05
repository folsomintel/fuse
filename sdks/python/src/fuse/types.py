from __future__ import annotations

from datetime import datetime
from typing import Any, Optional

from pydantic import BaseModel, ConfigDict, Field

# lifecycle states for EnvironmentInfo.state and Event.state.
STATE_PROVISIONING = "provisioning"
STATE_RUNNING = "running"
STATE_DRAINING = "draining"
STATE_DESTROYING = "destroying"
STATE_DESTROYED = "destroyed"
STATE_FAILED = "failed"


# verdicts carried in Health.state. separate from the lifecycle states above on
# purpose: a failing probe reports, it does not move the environment anywhere
# new.
HEALTH_STARTING = "starting"
HEALTH_PASSING = "passing"
HEALTH_FAILING = "failing"


# hitl intervention kinds. ask and review are raised by request_human;
# exec, lifecycle and computer are raised by a gate holding that surface.
HITL_KIND_ASK = "ask"
HITL_KIND_EXEC = "exec"
HITL_KIND_LIFECYCLE = "lifecycle"
HITL_KIND_COMPUTER = "computer"
HITL_KIND_REVIEW = "review"

# review formats, for HumanRequest.format and InterventionReview.format.
REVIEW_FORMAT_TEXT = "text"
REVIEW_FORMAT_DIFF = "diff"

# decider verdicts, and the actor source a decider's decision records.
DECIDER_ALLOW = "allow"
DECIDER_DENY = "deny"
DECIDER_ASK = "ask"
ACTOR_SOURCE_DECIDER = "decider"

# gate modes, carried in HitlGate.mode and Intervention.mode.
GATE_MODE_ASK = "ask"
GATE_MODE_NOTIFY = "notify"
GATE_MODE_DENY = "deny"

# lifecycle actions a gate can hold, for HitlGateMatch.action.
LIFECYCLE_DESTROY = "destroy"
LIFECYCLE_FORK = "fork"
LIFECYCLE_MIGRATE = "migrate"
LIFECYCLE_DRAIN = "drain"
LIFECYCLE_SNAPSHOT = "snapshot"
LIFECYCLE_ROTATE_TOKEN = "rotate-token"

# the gate name the exec allowlist reports.
ALLOWLIST_GATE = "exec.allowlist"

HITL_KIND_SECRET = "secret"
HITL_KIND_TAKEOVER = "takeover"
# raised by fuse itself, never by the agent: a stall (a failure run or quiet
# time) or an expiry (a reaper about to fire).
HITL_KIND_STALL = "stall"
HITL_KIND_EXPIRY = "expiry"

# the ways a human takes over. desktop gives the live desktop view, recorded
# as an input-action log plus periodic screenshots; it needs a desktop block.
TAKEOVER_VIA_TERMINAL = "terminal"
TAKEOVER_VIA_DESKTOP = "desktop"

# which reaper an expiry signal is about, carried in InterventionSignal.reaper.
REAPER_IDLE_TIMEOUT = "idle_timeout"
REAPER_MAX_RUNTIME = "max_runtime"

# intervention states. pending is the only non-terminal one.
INTERVENTION_PENDING = "pending"
INTERVENTION_APPROVED = "approved"
INTERVENTION_REJECTED = "rejected"
INTERVENTION_EXPIRED = "expired"
INTERVENTION_CANCELLED = "cancelled"

# intervention outcomes. every terminal intervention carries one.
OUTCOME_APPROVE = "approve"
OUTCOME_REJECT = "reject"

# hitl scopes an api key can be minted with. a key with none is full access.
SCOPE_HITL_READ = "hitl:read"
SCOPE_HITL_DECIDE = "hitl:decide"
SCOPE_HITL_SECRET = "hitl:secret"
SCOPE_HITL_TAKEOVER = "hitl:takeover"
SCOPE_HITL_ACT_AS = "hitl:act-as"

# hitl audit log events, carried in AuditEntry.event.
AUDIT_RAISED = "raised"
AUDIT_DECIDED = "decided"
AUDIT_EXPIRED = "expired"
AUDIT_CANCELLED = "cancelled"
AUDIT_INVITE_MINTED = "invite_minted"
AUDIT_INVITE_USED = "invite_used"
AUDIT_GRANT_CREATED = "grant_created"
AUDIT_GRANT_USED = "grant_used"
AUDIT_SECRET_ANSWERED = "secret_answered"
AUDIT_TAKEOVER_STARTED = "takeover_started"
AUDIT_TAKEOVER_ENDED = "takeover_ended"
# links a recorded takeover session; its AuditEntry.recording_id names the
# cast to fetch with client.interventions.get_recording.
AUDIT_TAKEOVER_SESSION = "takeover_session"


def is_terminal_state(state: str) -> bool:
    # reports whether state is a terminal lifecycle state.
    return state in (STATE_DESTROYED, STATE_FAILED)


class _Model(BaseModel):
    # populate_by_name lets us build models with python field names while still
    # accepting wire aliases. extra="ignore" tolerates unknown server fields.
    model_config = ConfigDict(populate_by_name=True, extra="ignore")


class Spec(_Model):
    # hardware/runtime spec for a microvm. all fields are optional (omitempty).
    cpus: Optional[int] = None
    ram_mb: Optional[int] = None
    storage_gb: Optional[int] = None
    gpus: Optional[int] = None
    gpu_kind: Optional[str] = None
    # mig profile (e.g. "1g.10gb"); when set, gpus counts mig instances.
    gpu_profile: Optional[str] = None
    region: Optional[str] = None
    # restricts scheduling to hosts of this cpu architecture ("amd64",
    # "arm64"). none means any host.
    arch: Optional[str] = None
    max_runtime_seconds: Optional[int] = None
    # destroys the environment after this many seconds with no exec and no
    # attach session. none or 0 means no idle expiry.
    idle_timeout_seconds: Optional[int] = None
    image: Optional[str] = None
    # pins the environment to an exact host id (the fusefile's placement.host).
    host_id: Optional[str] = None
    # placement label selectors (the fusefile's placement.labels); every pair
    # must match the target host's declared labels.
    labels: Optional[dict[str, str]] = None
    # back guest memory with 2M pages. the host must have them reserved; only
    # an environment created with this can be migrated lazily.
    huge_pages: Optional[bool] = None


class ExposeSpec(_Model):
    # a port to publish from the microvm. as_ maps to the wire key "as"
    # because as is a reserved python keyword.
    #
    # protocol is "tcp" (the default when omitted) or "udp". it is a plain
    # str rather than a Literal so a server that grows a third transport
    # does not fail to parse here. the host agent installs a separate
    # forwarding rule per transport, so a service speaking both needs two
    # entries.
    port: int
    as_: Optional[str] = Field(default=None, alias="as")
    protocol: Optional[str] = None


class Endpoint(_Model):
    # a published endpoint reported by the server. as_ maps to the wire
    # key "as" because as is a reserved python keyword.
    #
    # protocol is the transport actually published, not the one requested:
    # an agent too old to know about the field publishes tcp and omits it.
    as_: Optional[str] = Field(default=None, alias="as")
    url: str = ""
    protocol: str = ""
    port: int = 0


class HealthcheckHttp(_Model):
    # an http get against a port inside the guest. the probe runs in-guest, so
    # the port needs no matching expose entry.
    port: int
    # request path, which must start with a slash. none means "/".
    path: Optional[str] = None


class HealthcheckSpec(_Model):
    # the environment-level readiness probe (the fusefile's `healthcheck:`
    # block). exactly one of http and exec must be set; the server rejects a
    # request that sets both or neither.
    #
    # this is not the per-service compose healthcheck that can appear inside a
    # manifest: that one governs a single container and never reaches the
    # control plane. this one is evaluated by the guest agent over the whole
    # environment and its verdict comes back on EnvironmentInfo.health.
    #
    # every duration is in seconds, and none or 0 means "omitted, use the guest
    # agent's default". the server substitutes no defaults of its own, because
    # the probe executes in the guest.
    http: Optional[HealthcheckHttp] = None
    # an argv run inside the guest. exit status 0 passes.
    exec: Optional[list[str]] = None
    # seconds between attempts. none or 0 for the guest agent default (10s).
    interval_seconds: Optional[int] = None
    # bound on one attempt, in seconds. none or 0 for the guest agent default
    # (2s). attempts run one at a time, so a value above interval_seconds is
    # rejected rather than silently stretching the interval.
    timeout_seconds: Optional[int] = None
    # consecutive failures that flip the verdict to failing. none or 0 for the
    # guest agent default (3).
    retries: Optional[int] = None
    # grace window from the first attempt during which failures are not
    # counted. it ends for good once the probe passes once.
    start_period_seconds: Optional[int] = None


class DesktopSpec(_Model):
    # the geometry of the environment's graphical session (the fusefile's
    # `desktop:` block). requires an image that carries the desktop stack; on
    # any other image the declaration is inert and the computer surface
    # reports the display as absent.
    #
    # width and height are required, 320 to 3840 each: a guessed dimension
    # would silently shift every coordinate a computer-use model emits.
    width: int
    height: int
    # window manager to run: "mutter", "xfwm4" or "openbox". none keeps the
    # image's baked default.
    wm: Optional[str] = None


class ComputerAction(_Model):
    # one computer-use action, in the same shape anthropic's computer tool
    # emits as tool_use input, so translating a tool_use block into a call is
    # mechanical. only action is required; which other fields apply depends on
    # the action, and the guest agent enforces the full schema.
    action: str
    # [x, y] target for pointer actions.
    coordinate: Optional[list[int]] = None
    # [x, y] origin for left_click_drag.
    start_coordinate: Optional[list[int]] = None
    # the payload for type, the keysym combo for key/hold_key, or modifiers
    # held during a click action.
    text: Optional[str] = None
    # [x1, y1, x2, y2] crop for zoom.
    region: Optional[list[int]] = None
    # seconds for hold_key and wait, capped guest-side at 100.
    duration: Optional[float] = None
    scroll_direction: Optional[str] = None
    # wheel clicks for scroll, 1 to 100.
    scroll_amount: Optional[int] = None


class ComputerResult(_Model):
    # the guest's answer to one action. screenshot is a base64 png, present on
    # every action that implies one; for zoom it is the cropped region at full
    # resolution.
    output: str = ""
    screenshot: str = ""


class ComputerDisplay(_Model):
    # the environment's display report: whether it is up and at what geometry,
    # so a caller can populate display_width_px / display_height_px in its
    # computer tool definition without hardcoding them.
    display: str = ""
    up: bool = False
    # live display size in pixels, 0 when the display is down.
    width: int = 0
    height: int = 0
    # the window manager actually running, which can differ from
    # DesktopSpec.wm when the image does not carry the one asked for.
    wm: str = ""
    # why the display is down, when it is.
    error: str = ""


class ComputerToolResult(_Model):
    # what goes back to claude for one computer tool_use: the content blocks
    # and whether they describe an error. the blocks are plain dicts already
    # in the messages api shape, so they can be placed on a tool_result block
    # verbatim:
    #
    #   {"type": "tool_result", "tool_use_id": block.id,
    #    "content": result.content, "is_error": result.is_error}
    content: list[dict[str, Any]] = Field(default_factory=list)
    is_error: bool = False


class Health(_Model):
    # the last verdict of an environment's healthcheck.
    #
    # deliberately not folded into EnvironmentInfo.state: that vocabulary is a
    # closed set clients reason about, and an unhealthy environment is still a
    # running one. nothing tears an environment down for a failing probe.
    state: str = ""
    # when the probe entered this state.
    since: Optional[datetime] = None
    # consecutive failed attempts. zero while passing.
    failures: int = 0
    # the last attempt's failure detail. empty while passing.
    message: str = ""


class EgressSpec(_Model):
    # routes the environment's outbound traffic. none (or mode none) means
    # direct egress, which is what every request written before this field
    # existed meant. every field is a plain str rather than a Literal so a
    # server that grows a new mode or provider does not fail to parse here.
    #
    # mode is "direct" (the default) or "proxy". provider is the proxy
    # backend, required when mode is "proxy" (today "mock"; later
    # "cloudflare-warp"). protocol is "socks5" (the default for proxy) or
    # "http".
    mode: Optional[str] = None
    provider: Optional[str] = None
    protocol: Optional[str] = None


class EgressHealth(_Model):
    # the proxy endpoint's last probe verdict. state is "unknown" until the
    # first probe, then "healthy" or "unhealthy"; unhealthy means the
    # environment currently has no egress at all, never direct egress.
    # reason is the backend-level failure, empty while healthy.
    state: str = ""
    reason: str = ""
    checked_at: Optional[datetime] = None


class EgressStatus(_Model):
    # how the environment's outbound traffic is actually routed. mode is
    # always set; provider, protocol and endpoint are present only for proxy
    # egress. endpoint is a host-local address, never a credential.
    mode: str = ""
    provider: str = ""
    protocol: str = ""
    endpoint: str = ""
    health: Optional[EgressHealth] = None


class HitlAskRequests(_Model):
    # configures ask requests. present means allowed.
    pass


class HitlSecretTarget(_Model):
    # one declared destination for a secret: exactly one of env (a variable
    # later execs see) or file (an absolute guest path, written 0600). label
    # is what the human is shown first.
    env: Optional[str] = None
    file: Optional[str] = None
    label: str = ""


class HitlSecretRequests(_Model):
    # configures secret requests. the agent names one of these targets; it
    # can never choose a path or variable of its own.
    targets: list[HitlSecretTarget] = Field(default_factory=list)


class HitlTakeoverRequests(_Model):
    # configures takeover requests. via lists the ways a human may take
    # over: "terminal" and "desktop" (which needs a desktop block). none
    # means terminal.
    via: Optional[list[str]] = None


class HitlReviewRequests(_Model):
    # configures review requests. present means allowed.
    pass


class HitlRequests(_Model):
    # declares the request kinds the agent may raise. present means allowed:
    # set ask to HitlAskRequests() to allow asks, and so on.
    ask: Optional[HitlAskRequests] = None
    secret: Optional[HitlSecretRequests] = None
    takeover: Optional[HitlTakeoverRequests] = None
    review: Optional[HitlReviewRequests] = None


class HitlWebhook(_Model):
    # a url the server posts intervention events to. secret is the name of an
    # entry in the create request's secrets map, used to sign each delivery.
    url: str
    secret: str


class HitlGateMatch(_Model):
    # selects the actions a gate applies to; every field set must match.
    # program matches the basename of argv[0] after env, sudo, nice, timeout,
    # xargs, nohup and stdbuf are stripped. args match when they appear in
    # argv in this order. shell matches any opaque shell exec. action matches
    # lifecycle actions (the LIFECYCLE_* constants) or, on a computer gate,
    # computer action names ("type", "key", "left_click", ...). text_contains
    # matches a computer "type" whose text contains any entry, ignoring case.
    # failures and quiet_seconds raise a stall (on HITL_KIND_STALL);
    # before_seconds raises an expiry ahead of a reap (on HITL_KIND_EXPIRY).
    program: Optional[list[str]] = None
    args: Optional[list[str]] = None
    shell: Optional[bool] = None
    action: Optional[list[str]] = None
    text_contains: Optional[list[str]] = None
    failures: Optional[int] = None
    quiet_seconds: Optional[int] = None
    before_seconds: Optional[int] = None


class HitlDecider(_Model):
    # a gate's decide webhook. secret names an entry in the create request's
    # secrets map; its value signs each call (Fuse-Signature).
    url: str
    secret: str


class HitlGate(_Model):
    # one enforced gate. on is HITL_KIND_EXEC, HITL_KIND_LIFECYCLE,
    # HITL_KIND_COMPUTER, HITL_KIND_STALL or HITL_KIND_EXPIRY. mode is
    # GATE_MODE_ASK (the default), GATE_MODE_NOTIFY or GATE_MODE_DENY. grant
    # is the default reach of an approval: "once", "env" or "ttl:<duration>".
    # reason is returned on a deny and shown to the human on an ask. decide
    # is a webhook asked allow, deny or ask before a human sees an ask gate's
    # action; a failure or timeout means ask.
    name: str
    on: str
    match: HitlGateMatch = Field(default_factory=HitlGateMatch)
    mode: Optional[str] = None
    grant: Optional[str] = None
    timeout_seconds: Optional[int] = None
    reason: Optional[str] = None
    decide: Optional[HitlDecider] = None


class HitlExecPolicy(_Model):
    # allowlist mode for exec: a program not in allow is held with default
    # ("ask" or "deny"), interpreters and shells included.
    default: str
    allow: list[str] = Field(default_factory=list)


class HitlSpec(_Model):
    # the human-in-the-loop policy an environment is created with.
    #
    # timeout_seconds is the default deadline for every intervention, 10 to
    # 86400; none means 600. on_timeout is "reject" (the default) or "allow".
    # require_actor refuses decisions that do not name an actor. notify lists
    # webhooks to call when an intervention is raised or resolved.
    timeout_seconds: Optional[int] = None
    on_timeout: Optional[str] = None
    requests: HitlRequests = Field(default_factory=HitlRequests)
    require_actor: Optional[bool] = None
    notify: Optional[list[HitlWebhook]] = None
    # gates hold, record or deny exec, lifecycle and computer actions, and
    # raise stall and expiry signals. exec is allowlist mode for exec.
    gates: Optional[list[HitlGate]] = None
    exec: Optional[HitlExecPolicy] = None


class CreateRequest(_Model):
    # body for client.environments.create.
    task_id: str
    spec: Spec = Field(default_factory=Spec)
    manifest_inline: Optional[str] = None
    secrets: Optional[dict[str, str]] = None
    startup_script: Optional[str] = None
    # guest files written before the startup script runs, keyed by absolute
    # guest path with base64-encoded content. what a fusefile's `copy` block
    # compiles to. paths under /fuse are rejected, the total decoded size is
    # capped at 512 KiB, and permissions are not carried.
    files: Optional[dict[str, str]] = None
    gateway_url: Optional[str] = None
    gateway_token: Optional[str] = None
    expose: Optional[list[ExposeSpec]] = None
    # environment-level readiness probe. none for an environment with no probe,
    # in which case EnvironmentInfo.health is never populated. it is not
    # evaluated inside create: the call returns as soon as the vm is up, and
    # the verdict arrives on later reads.
    healthcheck: Optional[HealthcheckSpec] = None
    # routes the environment's outbound traffic. none for direct egress.
    egress: Optional[EgressSpec] = None
    # graphical session geometry. none for an environment with no desktop, in
    # which case a desktop image keeps its baked default geometry.
    desktop: Optional[DesktopSpec] = None
    # human-in-the-loop policy. none means hitl is off and the environment
    # cannot raise interventions.
    hitl: Optional[HitlSpec] = None


class EnvironmentInfo(_Model):
    # the server's view of a single microvm.
    id: str = ""
    state: str = ""
    task_id: str = ""
    host_id: str = ""
    url: str = ""
    spec: Spec = Field(default_factory=Spec)
    created_at: Optional[datetime] = None
    updated_at: Optional[datetime] = None
    error: str = ""
    endpoints: list[Endpoint] = Field(default_factory=list)
    # last verdict of the environment-level healthcheck. none when the
    # environment declared no healthcheck, and none until the first verdict has
    # been read back from the guest. the server refreshes it on its reconcile
    # tick (30s by default), so it lags the guest by up to a tick.
    health: Optional[Health] = None
    # how outbound traffic is actually routed. none only from a server that
    # predates the field; treat that as direct.
    egress: Optional[EgressStatus] = None


class ForkOptions(_Model):
    # optional body for environments.fork.
    reuse_snapshot_id: Optional[str] = None
    comment: Optional[str] = None


class MigrateOptions(_Model):
    # optional body for environments.migrate.
    #
    # target_host_id empty means the orchestrator picks a host other than the
    # source, preferring the environment's checkpoint standby; if no other host
    # can take it the call fails with 503 and the source keeps running.
    #
    # live resumes the guest from its memory on the target, so processes
    # survive the move, instead of cold-booting it. it never falls back to a
    # cold migration: a target that cannot resume the guest answers 409 and the
    # source keeps running.
    #
    # lazy resumes the guest before its memory has arrived and pages it in from
    # the source afterwards. it requires live and an environment created with
    # huge_pages; until the vm.memory_resident event the guest still depends on
    # the source host.
    target_host_id: Optional[str] = None
    live: Optional[bool] = None
    lazy: Optional[bool] = None


class ExecRequest(_Model):
    # body for environments.exec. exactly one of cmd or shell must be set.
    #
    # cmd is argv: it needs no quoting rules and interpolating a value into it
    # cannot turn into an injection, so prefer it. shell runs the string under
    # `sh -lc` and is only for what argv cannot express: pipelines, redirects,
    # and globs. timeout_ms bounds the command inside the guest; unset or 0
    # takes the server default and the server clamps anything above its ceiling.
    cmd: Optional[list[str]] = None
    shell: Optional[str] = None
    timeout_ms: Optional[int] = None


class ExecResult(_Model):
    # the outcome of a guest command.
    #
    # a non-zero exit_code is a successful call: the command ran and failed.
    # only a raised ApiError means the command could not be run at all.
    exit_code: int = 0
    stdout: str = ""
    stderr: str = ""


class Event(BaseModel):
    # one item from EnvironmentsService.events. err is set only on a
    # stream-level failure, as the final event before the iterator ends.
    model_config = ConfigDict(populate_by_name=True, extra="ignore", arbitrary_types_allowed=True)

    id: str = ""
    kind: str = Field(default="", alias="event")
    vm_id: str = ""
    state: str = ""
    url: str = ""
    error: str = ""
    updated_at: Optional[datetime] = None
    err: Optional[Exception] = Field(default=None, exclude=True)

    # set only on "step" events, which report one setup step's boundary,
    # timing, and cache verdict. zero on state events.
    index: int = 0
    total: int = 0
    key: str = ""
    cached: bool = False
    miss_reason: str = ""
    miss_detail: list[str] = Field(default_factory=list)
    duration_ms: int = 0
    exit_code: int = 0


class SnapshotRequest(_Model):
    # optional body for snapshots.create.
    comment: Optional[str] = None
    mode: Optional[str] = None
    retention_seconds: Optional[int] = None
    metadata: Optional[dict[str, str]] = None
    export_ref: Optional[str] = None
    export_status: Optional[str] = None
    # labels this snapshot as the artifact of one cacheable setup step, which is
    # what makes it findable by recipe rather than by its random id. none for an
    # ordinary snapshot. the scope it is filed under comes from how the caller
    # authenticated and is not settable here.
    layer_key: Optional[str] = None
    # captures the guest's memory and vcpu state alongside the rootfs, so
    # restoring resumes the guest instead of cold-booting it. unset (or false)
    # is a disk-only snapshot, which is the default and the fallback.
    #
    # it costs the environment's full configured memory in bytes on every
    # create, pauses the guest for the length of the capture, and produces an
    # artifact pinned to the host that took it. it also cannot seed a new
    # environment: fork and CreateRequest.seed_snapshot_id read only a
    # snapshot's rootfs, and a live snapshot's rootfs is captured without
    # quiescing the guest filesystem, so it is consistent only alongside the
    # memory image. both reject a live snapshot with a 409 conflict.
    #
    # against a backend with no live-snapshot support this is a 501
    # unimplemented, not a conflict, so is_conflict reports false for it.
    #
    # it is a request, not a guarantee: read Snapshot.kind to find out what was
    # actually written.
    live: Optional[bool] = None


class SnapshotExport(_Model):
    # an optional exported snapshot artifact.
    destination: str = ""
    status: str = ""
    requested_at: Optional[datetime] = None
    updated_at: Optional[datetime] = None
    last_error: str = ""


class Snapshot(_Model):
    # a persisted snapshot record.
    id: str = ""
    vm_id: str = ""
    task_id: str = ""
    tenant_id: str = ""
    parent_snapshot_id: str = ""
    mode: str = ""
    state: str = ""
    comment: str = ""
    # the content-addressed cache key of the setup step this artifact was
    # taken after. empty on anything that is not a build layer, and an empty
    # key never matches a lookup.
    layer_key: str = ""
    # cpu architecture, in goarch vocabulary ("amd64", "arm64"), of the host
    # that actually built the artifact rather than of whoever asked for it. a
    # rootfs is not portable across architectures, so this is a real
    # constraint on whether the artifact can be booted; it is deliberately not
    # folded into layer_key, so a lookup has to filter on it separately.
    arch: str = ""
    # hex sha256 of the artifact rootfs. it verifies that a given copy of
    # those bytes is intact, and nothing more: it is not a cross-build
    # identity, because two builds of the same recipe produce different rootfs
    # bytes (timestamps, inode ordering, package caches). it can never be used
    # as a cache key.
    digest: str = ""
    # "disk" or "live": whether this artifact is the rootfs alone, or the
    # rootfs plus the guest's memory and vcpu state. restoring a disk snapshot
    # cold-boots the guest; restoring a live one resumes it.
    #
    # it is what the host agent reports it actually wrote, not what was
    # requested. a host running an agent too old to know about live snapshots
    # answers SnapshotRequest(live=True) with a disk snapshot, and this then
    # says "disk". branch on it rather than on your own request.
    #
    # a server that predates live snapshots omits it entirely, which reads back
    # as ""; treat that as "disk".
    kind: str = ""
    size_bytes: int = 0
    created_at: Optional[datetime] = None
    updated_at: Optional[datetime] = None
    retention_until: Optional[datetime] = None
    last_error: str = ""
    export_ref: str = ""
    exports: list[SnapshotExport] = Field(default_factory=list)


class GPUDevice(_Model):
    # per-device detail probed for a single gpu. every field is best-effort
    # and omitted when the host agent could not determine it.
    uuid: str = ""
    model: str = ""
    pci_bus_id: str = ""
    memory_mb: int = 0
    driver_version: str = ""
    cuda_version: str = ""
    compute_cap: str = ""
    mig_capable: bool = False
    mig_mode: str = ""
    iommu_group: str = ""


class MIGInstance(_Model):
    # one carved mig gpu instance probed from the host agent. the orchestrator
    # binds a specific instance uuid to a vm so it knows which instance went
    # to which vm.
    uuid: str = ""
    profile: str = ""
    kind: str = ""
    parent_gpu_uuid: str = ""


class HostCapacity(_Model):
    # a host's resource envelope.
    cpus: int = 0
    ram_mb: int = 0
    storage_gb: int = 0
    vm_count: int = 0
    # the host's cpu architecture in goarch vocabulary ("amd64", "arm64").
    # empty means amd64 (hosts registered before arch existed).
    arch: str = ""
    gpus: int = 0
    gpu_kind: str = ""
    # fractional gpu capacity: mig instance count by profile name. when the
    # host reports per-instance mig inventory (mig_instances), this map is a
    # derived summary; otherwise it is the scheduling unit.
    mig_profiles: Optional[dict[str, int]] = None
    # per-instance mig inventory probed from the host agent (one entry per
    # carved mig gpu instance). when non-empty, the orchestrator binds
    # specific instance uuids to vms. strictly additive: a host that reports
    # no instances falls back to mig_profiles. only populated on capacity
    # (not allocated) for qemu hosts.
    mig_instances: list[MIGInstance] = Field(default_factory=list)
    # the set of mig instance uuids currently bound to vms. populated only on
    # allocated, and only for hosts that report per-instance mig inventory.
    mig_instance_uuids: list[str] = Field(default_factory=list)
    # per-device gpu detail probed from the host agent. only populated on
    # capacity (not allocated) for qemu hosts.
    gpu_devices: list[GPUDevice] = Field(default_factory=list)


class RegisterHostRequest(_Model):
    # body for hosts.register.
    id: str
    url: str
    token: Optional[str] = None
    region: Optional[str] = None
    backend: Optional[str] = None
    # operator-declared key/value pairs matched against a spec's placement
    # label selectors. never probed from the host agent.
    labels: Optional[dict[str, str]] = None
    capacity: HostCapacity = Field(default_factory=HostCapacity)


class Host(_Model):
    # the server's view of a registered host.
    id: str = ""
    url: str = ""
    region: str = ""
    state: str = ""
    backend: str = ""
    labels: dict[str, str] = Field(default_factory=dict)
    capacity: HostCapacity = Field(default_factory=HostCapacity)
    allocated: HostCapacity = Field(default_factory=HostCapacity)
    last_seen: Optional[datetime] = None
    created_at: Optional[datetime] = None
    updated_at: Optional[datetime] = None
    # non-fatal registration notices (e.g. declared capacity exceeding the
    # probed value). only ever populated on the response to hosts.register.
    warnings: list[str] = Field(default_factory=list)


class EnvironmentPage(_Model):
    # one page of environments.list_page, plus the cursor to fetch the next
    # one. next_cursor is an opaque token; treat it as such rather than
    # parsing it. it is none once there are no more results.
    environments: list[EnvironmentInfo] = Field(default_factory=list)
    next_cursor: Optional[str] = None


class SnapshotPage(_Model):
    # one page of snapshots.list_page, plus the cursor to fetch the next
    # one. next_cursor is none once there are no more results.
    snapshots: list[Snapshot] = Field(default_factory=list)
    next_cursor: Optional[str] = None


class HostPage(_Model):
    # one page of hosts.list_page, plus the cursor to fetch the next one.
    # next_cursor is none once there are no more results.
    hosts: list[Host] = Field(default_factory=list)
    next_cursor: Optional[str] = None


class ActorLabel(_Model):
    # an actor as a caller names it: an opaque id (required, at most 128
    # bytes) and an optional display name.
    id: str
    name: Optional[str] = None


class APIKey(_Model):
    # a key's metadata. the raw secret appears only in CreatedAPIKey.key.
    # scopes is none for a full-access key.
    id: str = ""
    label: str = ""
    created_at: Optional[datetime] = None
    last_used_at: Optional[datetime] = None
    revoked_at: Optional[datetime] = None
    scopes: Optional[list[str]] = None
    env_id: str = ""
    expires_at: Optional[datetime] = None
    actor: Optional[ActorLabel] = None
    created_by: str = ""


class CreatedAPIKey(APIKey):
    # returned by api_keys.create. key is unrecoverable afterward.
    key: str = ""


class Actor(_Model):
    # who raised or decided an intervention: "master", "key:<id>", or
    # "system" for fuse itself (expiry, cancel on destroy).
    id: str = ""
    name: str = ""
    source: str = ""
    key_id: str = ""


class ExecPayload(_Model):
    # a held exec.
    argv: list[str] = Field(default_factory=list)
    shell: str = ""
    timeout_ms: int = 0


class LifecyclePayload(_Model):
    # a held environment action, one of the LIFECYCLE_* constants.
    action: str = ""


class ComputerPayload(_Model):
    # a held computer action. input is the action body as sent.
    action: str = ""
    text: str = ""
    input: dict[str, Any] = Field(default_factory=dict)


class InterventionReview(_Model):
    # a review request. content is the file as fuse read it at request time
    # when path is set.
    path: str = ""
    content: str = ""
    format: str = ""
    message: str = ""
    truncated: bool = False


class InterventionDecider(_Model):
    # what a gate's decider said when it sent an action on to a human:
    # verdict "ask" with its reason, or the error fuse fell through on.
    verdict: str = ""
    reason: str = ""
    error: str = ""


class InterventionSecret(_Model):
    # a secret request. label comes from the fusefile and is what a human
    # should read first; message is the agent's own text. the value is never
    # part of any intervention.
    target: str = ""
    label: str = ""
    message: str = ""


class InterventionTakeover(_Model):
    # a takeover. holder and started_at are set while a human holds the
    # environment, which is also when agent exec and computer calls get 423.
    via: str = ""
    reason: str = ""
    holder: Optional[Actor] = None
    started_at: Optional[datetime] = None


class InterventionSignal(_Model):
    # what fuse noticed on its own. a stall carries the failure run or the
    # quiet time; an expiry carries the reaper due and when, and once
    # approved the time the environment is kept until.
    failures: int = 0
    last_exit_code: int = 0
    quiet_seconds: int = 0
    reaper: str = ""
    reap_at: Optional[datetime] = None
    extended_until: Optional[datetime] = None


class InterventionPayload(_Model):
    # what was asked, attempted or noticed: a question for an ask, the held
    # exec, lifecycle or computer action for a gate, the target for a secret,
    # the takeover, what is up for review, or the signal fuse saw for a stall
    # or expiry. decider is the gate decider's note on an action it escalated.
    question: str = ""
    choices: list[str] = Field(default_factory=list)
    exec: Optional[ExecPayload] = None
    lifecycle: Optional[LifecyclePayload] = None
    secret: Optional[InterventionSecret] = None
    takeover: Optional[InterventionTakeover] = None
    signal: Optional[InterventionSignal] = None
    computer: Optional[ComputerPayload] = None
    review: Optional[InterventionReview] = None
    decider: Optional[InterventionDecider] = None


class InterventionDecision(_Model):
    # how an intervention resolved. an expired one carries the on_timeout
    # outcome and a cancelled one a reject.
    outcome: str = ""
    answer: str = ""
    reason: str = ""
    decided_by: Actor = Field(default_factory=Actor)
    decided_at: Optional[datetime] = None
    # the argv the approver replaced a held exec's with.
    edited_argv: list[str] = Field(default_factory=list)
    # the text the approver replaced a held computer type's with.
    edited_text: Optional[str] = None
    # how far an approval of a gated action reaches.
    grant: str = ""


class InterventionResult(_Model):
    # what running an approved gated action produced.
    exec: Optional[ExecResult] = None
    computer: Optional[ComputerResult] = None
    env_id: str = ""
    snapshot_id: str = ""
    error: str = ""
    finished_at: Optional[datetime] = None


class Intervention(_Model):
    # one request for a human. gate names the gate that raised it (empty for
    # a request) and mode is that gate's mode; notify and deny records are
    # born terminal.
    id: str = ""
    env_id: str = ""
    kind: str = ""
    gate: str = ""
    mode: str = ""
    state: str = ""
    payload: InterventionPayload = Field(default_factory=InterventionPayload)
    triggered_by: Actor = Field(default_factory=Actor)
    deadline: Optional[datetime] = None
    on_timeout: str = ""
    decision: Optional[InterventionDecision] = None
    # set once an approved gated action has been performed.
    result: Optional[InterventionResult] = None
    created_at: Optional[datetime] = None
    updated_at: Optional[datetime] = None

    @property
    def terminal(self) -> bool:
        # reports whether the intervention is resolved.
        return self.state != "" and self.state != INTERVENTION_PENDING

    @property
    def approved(self) -> bool:
        # reports whether the intervention resolved as approve, which includes
        # an expiry under on_timeout: allow.
        return self.decision is not None and self.decision.outcome == OUTCOME_APPROVE

    @property
    def settled(self) -> bool:
        # reports whether a caller has its final answer: resolved, and for an
        # approved gated action that fuse performs, performed.
        if not self.terminal:
            return False
        # a decider's allow lets the caller perform the action itself.
        if self.decision is not None and self.decision.decided_by.source == ACTOR_SOURCE_DECIDER:
            return True
        runs = self.mode == GATE_MODE_ASK and (
            self.payload.exec is not None
            or self.payload.lifecycle is not None
            or self.payload.computer is not None
        )
        return not runs or not self.approved or self.result is not None


class HumanRequest(_Model):
    # body for environments.request_human. timeout_seconds shortens the
    # policy's deadline; it can never extend it. question and choices are for
    # an ask, target and message for a secret, via and reason for a takeover.
    # a review takes path (an absolute guest file) or content (inline text or
    # a diff), format ("text" or "diff", none to guess) and message.
    kind: str = HITL_KIND_ASK
    question: Optional[str] = None
    choices: Optional[list[str]] = None
    target: Optional[str] = None
    message: Optional[str] = None
    via: Optional[str] = None
    reason: Optional[str] = None
    path: Optional[str] = None
    content: Optional[str] = None
    format: Optional[str] = None
    timeout_seconds: Optional[int] = None


class TakeoverRequest(_Model):
    # body for environments.takeover. via is terminal (the default) or
    # desktop; reason is recorded when no agent request is pending to claim.
    via: Optional[str] = None
    reason: Optional[str] = None


class ReleaseRequest(_Model):
    # body for environments.release. note is handed back to the agent as the
    # takeover's answer.
    note: Optional[str] = None


class ReviewResult(_Model):
    # a finished review. comments are the reviewer's, on either outcome.
    approved: bool = False
    comments: str = ""
    intervention: Intervention = Field(default_factory=Intervention)


class InterventionPage(_Model):
    # one page of interventions.list_page, plus the cursor to fetch the next
    # one. next_cursor is none once there are no more results.
    interventions: list[Intervention] = Field(default_factory=list)
    next_cursor: Optional[str] = None


class AuditEntry(_Model):
    # one row of the hash-chained hitl audit log. it records who did what and
    # from where, never content.
    seq: int = 0
    event: str = ""
    intervention_id: str = ""
    env_id: str = ""
    actor: Actor = Field(default_factory=Actor)
    remote_addr: str = ""
    # set on a takeover_session row; fetch the cast with
    # client.interventions.get_recording.
    recording_id: str = ""
    at: Optional[datetime] = None
    prev_hash: str = ""
    hash: str = ""


class AuditPage(_Model):
    # one page of client.interventions.audit_page. pass next_cursor back to
    # fetch the next page; none means this was the last one.
    entries: list[AuditEntry] = Field(default_factory=list)
    next_cursor: Optional[str] = None

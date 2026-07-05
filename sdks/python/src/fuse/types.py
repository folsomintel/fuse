from __future__ import annotations

from datetime import datetime
from typing import Optional

from pydantic import BaseModel, ConfigDict, Field

# lifecycle states for EnvironmentInfo.state and Event.state.
STATE_PROVISIONING = "provisioning"
STATE_RUNNING = "running"
STATE_DRAINING = "draining"
STATE_DESTROYING = "destroying"
STATE_DESTROYED = "destroyed"
STATE_FAILED = "failed"


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
    region: Optional[str] = None
    max_runtime_seconds: Optional[int] = None
    image: Optional[str] = None


class ExposeSpec(_Model):
    # a port to publish from the microvm. as_ maps to the wire key "as"
    # because as is a reserved python keyword.
    port: int
    as_: Optional[str] = Field(default=None, alias="as")


class Endpoint(_Model):
    # a published endpoint reported by the server. as_ maps to the wire
    # key "as" because as is a reserved python keyword.
    as_: Optional[str] = Field(default=None, alias="as")
    url: str = ""
    port: int = 0


class CreateRequest(_Model):
    # body for client.environments.create.
    task_id: str
    spec: Spec = Field(default_factory=Spec)
    manifest_inline: Optional[str] = None
    secrets: Optional[dict[str, str]] = None
    startup_script: Optional[str] = None
    gateway_url: Optional[str] = None
    gateway_token: Optional[str] = None
    expose: Optional[list[ExposeSpec]] = None


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


class ForkOptions(_Model):
    # optional body for environments.fork.
    reuse_snapshot_id: Optional[str] = None
    comment: Optional[str] = None


class Event(BaseModel):
    # one item from EnvironmentsService.events. err is set only on a
    # stream-level failure, as the final event before the iterator ends.
    model_config = ConfigDict(
        populate_by_name=True, extra="ignore", arbitrary_types_allowed=True
    )

    id: str = ""
    kind: str = Field(default="", alias="event")
    vm_id: str = ""
    state: str = ""
    url: str = ""
    error: str = ""
    updated_at: Optional[datetime] = None
    err: Optional[Exception] = Field(default=None, exclude=True)


class SnapshotRequest(_Model):
    # optional body for snapshots.create.
    comment: Optional[str] = None
    mode: Optional[str] = None
    retention_seconds: Optional[int] = None
    metadata: Optional[dict[str, str]] = None
    export_ref: Optional[str] = None
    export_status: Optional[str] = None


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
    size_bytes: int = 0
    created_at: Optional[datetime] = None
    updated_at: Optional[datetime] = None
    retention_until: Optional[datetime] = None
    last_error: str = ""
    export_ref: str = ""
    exports: list[SnapshotExport] = Field(default_factory=list)


class HostCapacity(_Model):
    # a host's resource envelope.
    cpus: int = 0
    ram_mb: int = 0
    storage_gb: int = 0
    vm_count: int = 0
    gpus: int = 0
    gpu_kind: str = ""


class RegisterHostRequest(_Model):
    # body for hosts.register.
    id: str
    url: str
    token: Optional[str] = None
    region: Optional[str] = None
    backend: Optional[str] = None
    capacity: HostCapacity = Field(default_factory=HostCapacity)


class Host(_Model):
    # the server's view of a registered host.
    id: str = ""
    url: str = ""
    region: str = ""
    state: str = ""
    backend: str = ""
    capacity: HostCapacity = Field(default_factory=HostCapacity)
    allocated: HostCapacity = Field(default_factory=HostCapacity)
    last_seen: Optional[datetime] = None
    created_at: Optional[datetime] = None
    updated_at: Optional[datetime] = None


class APIKey(_Model):
    # a key's metadata. the raw secret appears only in CreatedAPIKey.key.
    id: str = ""
    label: str = ""
    created_at: Optional[datetime] = None
    last_used_at: Optional[datetime] = None
    revoked_at: Optional[datetime] = None


class CreatedAPIKey(APIKey):
    # returned by api_keys.create. key is unrecoverable afterward.
    key: str = ""

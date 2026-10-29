// Public entry point for @folsom/fuse.

export { FuseClient } from "./client.js";
export type { FuseClientOptions } from "./client.js";
export { VERSION } from "./version.js";

// Services and their option types.
export { EnvironmentsService } from "./environments.js";
export type { ListEnvironmentsOptions, HumanRequest } from "./environments.js";
export { SnapshotsService } from "./snapshots.js";
export type { ListSnapshotsOptions } from "./snapshots.js";
export { HostsService } from "./hosts.js";
export { ApiKeysService } from "./apiKeys.js";
export type { CreateApiKeyOptions } from "./apiKeys.js";
export { InterventionsService } from "./interventions.js";
export type {
  ListInterventionsOptions,
  InterventionPage,
  ApproveOptions,
  RejectOptions,
  AuditOptions,
  AuditPage,
} from "./interventions.js";

// Per-call options and the fetch type.
export type { CallOptions, FetchLike } from "./transport.js";

// Wire types, state constants, and the terminal-state helper.
export type {
  Spec,
  ExposeSpec,
  Endpoint,
  CreateRequest,
  EnvironmentInfo,
  HealthcheckSpec,
  HealthcheckHttp,
  Health,
  DesktopSpec,
  EgressSpec,
  EgressStatus,
  EgressHealth,
  ComputerAction,
  ComputerResult,
  ComputerDisplay,
  ComputerToolResult,
  ToolResultBlock,
  ToolResultImageSource,
  ForkOptions,
  MigrateOptions,
  ExecRequest,
  ExecResult,
  Event,
  SnapshotRequest,
  SnapshotExport,
  Snapshot,
  HostCapacity,
  GPUDevice,
  MIGInstance,
  RegisterHostRequest,
  Host,
  APIKey,
  CreatedAPIKey,
  ActorLabel,
  HitlSpec,
  HitlWebhook,
  AuditEntry,
  HitlRequests,
  HitlAskRequests,
  Actor,
  InterventionPayload,
  InterventionDecision,
  Intervention,
} from "./types.js";
export {
  State,
  isTerminalState,
  HealthState,
  HitlKind,
  HitlScope,
  AuditEvent,
  InterventionState,
  InterventionOutcome,
  isInterventionTerminal,
  isInterventionApproved,
} from "./types.js";

// Errors and code helpers.
export {
  FuseApiError,
  FuseError,
  ForbiddenError,
  ForbiddenReason,
  ErrorCode,
  isFuseApiError,
  isNotFound,
  isConflict,
  isUnauthorized,
  isForbidden,
  isInvalidArgument,
  isUnavailable,
} from "./errors.js";
export type { FuseApiErrorInit } from "./errors.js";

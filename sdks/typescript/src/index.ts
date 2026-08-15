// Public entry point for @folsom/fuse.

export { FuseClient } from "./client.js";
export type { FuseClientOptions } from "./client.js";
export { VERSION } from "./version.js";

// Services and their option types.
export { EnvironmentsService } from "./environments.js";
export type { ListEnvironmentsOptions } from "./environments.js";
export { SnapshotsService } from "./snapshots.js";
export type { ListSnapshotsOptions } from "./snapshots.js";
export { HostsService } from "./hosts.js";
export { ApiKeysService } from "./apiKeys.js";

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
  ComputerAction,
  ComputerResult,
  ComputerDisplay,
  ForkOptions,
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
} from "./types.js";
export { State, isTerminalState, HealthState } from "./types.js";

// Errors and code helpers.
export {
  FuseApiError,
  FuseError,
  ErrorCode,
  isFuseApiError,
  isNotFound,
  isConflict,
  isUnauthorized,
  isInvalidArgument,
  isUnavailable,
} from "./errors.js";
export type { FuseApiErrorInit } from "./errors.js";

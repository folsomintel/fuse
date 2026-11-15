import { combineSignals, type CallOptions, type Transport } from "./transport.js";
import type { Operation } from "./types.js";
import { requireArg } from "./validate.js";

/** Options for operations.get. */
export interface GetOperationOptions {
  /** Attach until the operation finishes, for at most this long, as a
   * duration such as "30s" (capped by the server). Unset answers at once. */
  wait?: string;
}

/** Options for operations.wait. */
export interface WaitOperationOptions {
  /** Stop waiting after this many milliseconds. The operation keeps running. */
  timeout?: number;
}

/** OperationsService reads the operations mutating calls run as. */
export class OperationsService {
  constructor(private readonly t: Transport) {}

  /** Fetch an operation, finished or still running. */
  async get(
    id: string,
    options: GetOperationOptions = {},
    opts: CallOptions = {},
  ): Promise<Operation> {
    requireArg(id, "operation id");
    return this.t.getOperation(id, options.wait, opts.signal);
  }

  /**
   * Block until the operation finishes and return it. A failed operation is
   * returned, not thrown: its error and status_code say why. `timeout` and
   * `opts.signal` bound the wait; the operation keeps running if either
   * ends it first.
   */
  async wait(
    id: string,
    options: WaitOperationOptions = {},
    opts: CallOptions = {},
  ): Promise<Operation> {
    requireArg(id, "operation id");
    const { signal, cleanup } = combineSignals([
      opts.signal,
      options.timeout && options.timeout > 0
        ? AbortSignal.timeout(options.timeout)
        : undefined,
    ]);
    try {
      return await this.t.waitOperation(id, signal);
    } finally {
      cleanup();
    }
  }
}

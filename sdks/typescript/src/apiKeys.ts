import type { CallOptions, Transport } from "./transport.js";
import type { ActorLabel, APIKey, CreatedAPIKey } from "./types.js";
import { requireArg } from "./validate.js";

/** Options for apiKeys.create. Naming scopes mints a key limited to the hitl
 * routes; it must expire (ttlSeconds, at most 86400). */
export interface CreateApiKeyOptions {
  label?: string;
  /** One or more of HitlScope. Omit for a full-access key. */
  scopes?: string[];
  /** Limit a scoped key to this environment and its forks. */
  envId?: string;
  /** Lifetime in seconds. */
  ttlSeconds?: number;
  /** Pin the key to this actor. */
  actor?: ActorLabel;
}

interface APIKeyList {
  api_keys: APIKey[];
}

/**
 * ApiKeysService manages revocable API keys. These endpoints require the
 * master token; the server enforces this.
 */
export class ApiKeysService {
  constructor(private readonly t: Transport) {}

  /**
   * Issue a new API key. Pass a label, or options for a scoped key. The raw
   * secret is returned once in `key` and is unrecoverable afterward.
   */
  async create(
    labelOrOptions?: string | CreateApiKeyOptions,
    opts: CallOptions = {},
  ): Promise<CreatedAPIKey> {
    const o: CreateApiKeyOptions =
      typeof labelOrOptions === "string"
        ? { label: labelOrOptions }
        : (labelOrOptions ?? {});
    return this.t.json<CreatedAPIKey>("POST", "/v1/api-keys", {
      body: {
        label: o.label || undefined,
        scopes: o.scopes,
        env_id: o.envId || undefined,
        ttl_seconds: o.ttlSeconds || undefined,
        actor: o.actor,
      },
      signal: opts.signal,
    });
  }

  /** List API key metadata. */
  async list(opts: CallOptions = {}): Promise<APIKey[]> {
    const out = await this.t.json<APIKeyList>("GET", "/v1/api-keys", {
      signal: opts.signal,
    });
    return out.api_keys ?? [];
  }

  /** Revoke the API key with the given id. */
  async revoke(id: string, opts: CallOptions = {}): Promise<void> {
    requireArg(id, "id");
    await this.t.noContent("DELETE", `/v1/api-keys/${encodeURIComponent(id)}`, {
      signal: opts.signal,
    });
  }
}

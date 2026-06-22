import type { CallOptions, Transport } from "./transport.js";
import type { APIKey, CreatedAPIKey } from "./types.js";
import { requireArg } from "./validate.js";

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
   * Issue a new API key. The raw secret is returned once in `key` and is
   * unrecoverable afterward.
   */
  async create(label?: string, opts: CallOptions = {}): Promise<CreatedAPIKey> {
    return this.t.json<CreatedAPIKey>("POST", "/v1/api-keys", {
      body: label ? { label } : {},
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

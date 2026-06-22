import { createServer, type IncomingMessage, type ServerResponse } from "node:http";
import type { AddressInfo } from "node:net";
import { FuseClient } from "../src/index.js";

export interface TestServer {
  client: FuseClient;
  close: () => Promise<void>;
}

type Handler = (req: IncomingMessage, res: ServerResponse) => void | Promise<void>;

/**
 * serve spins up a real local HTTP server (the analog of Go's
 * httptest.NewServer) and returns a FuseClient pointed at it.
 */
export async function serve(handler: Handler): Promise<TestServer> {
  const server = createServer((req, res) => {
    Promise.resolve(handler(req, res)).catch(() => {
      if (!res.headersSent) res.statusCode = 500;
      res.end();
    });
  });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", () => resolve()));
  const address = server.address() as AddressInfo;
  const client = new FuseClient({
    baseUrl: `http://127.0.0.1:${address.port}`,
    token: "tok",
  });
  const close = () => new Promise<void>((resolve) => server.close(() => resolve()));
  return { client, close };
}

/** readBody collects the full request body as a UTF-8 string. */
export async function readBody(req: IncomingMessage): Promise<string> {
  const chunks: Buffer[] = [];
  for await (const chunk of req) chunks.push(chunk as Buffer);
  return Buffer.concat(chunks).toString("utf-8");
}

/** pathOf returns the request pathname. */
export function pathOf(req: IncomingMessage): string {
  return new URL(req.url ?? "", "http://localhost").pathname;
}

/** queryOf returns the request query string (without the leading "?"). */
export function queryOf(req: IncomingMessage): string {
  return new URL(req.url ?? "", "http://localhost").search.replace(/^\?/, "");
}

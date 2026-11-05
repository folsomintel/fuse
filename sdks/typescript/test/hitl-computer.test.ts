import { afterEach, describe, expect, it } from "vitest";
import {
  ACTOR_SOURCE_DECIDER,
  GateMode,
  HitlKind,
  InterventionRejectedError,
  PendingInterventionError,
  ReviewFormat,
  isInterventionSettled,
  type Intervention,
} from "../src/index.js";
import { pathOf, queryOf, readBody, serve, type TestServer } from "./server.js";

let current: TestServer | undefined;
afterEach(async () => {
  await current?.close();
  current = undefined;
});

const heldComputerBody =
  `{"intervention_id":"int_c","intervention":{"id":"int_c","env_id":"vm-1","kind":"computer",` +
  `"gate":"typing","mode":"ask","state":"pending",` +
  `"payload":{"computer":{"action":"type","text":"rm -rf ~","input":{"action":"type","text":"rm -rf ~"}}}}}`;

function action(query: string): string | null {
  return new URLSearchParams(query).get("action");
}

describe("hitl computer gates", () => {
  it("computer waits on a held action and returns the result", async () => {
    let polls = 0;
    current = await serve((req, res) => {
      res.setHeader("Content-Type", "application/json");
      const path = pathOf(req);
      if (req.method === "POST" && path === "/v1/environments/vm-1/computer") {
        res.statusCode = 202;
        res.end(heldComputerBody);
        return;
      }
      if (req.method === "GET" && path === "/v1/interventions/int_c") {
        polls++;
        if (polls === 1) {
          // approved, but not performed yet: not settled.
          res.end(
            `{"id":"int_c","kind":"computer","mode":"ask","state":"approved","payload":{"computer":{"action":"type"}},` +
              `"decision":{"outcome":"approve","edited_text":"ls ~"}}`,
          );
        } else {
          res.end(
            `{"id":"int_c","kind":"computer","mode":"ask","state":"approved","payload":{"computer":{"action":"type"}},` +
              `"decision":{"outcome":"approve","edited_text":"ls ~"},"result":{"computer":{"screenshot":"UE5H"}}}`,
          );
        }
        return;
      }
      res.statusCode = 500;
      res.end();
    });

    let held = "";
    const out = await current.client.environments.computer(
      "vm-1",
      { action: "type", text: "rm -rf ~" },
      {
        onHeld: (i) => {
          held = i.payload.computer?.text ?? "";
        },
      },
    );

    expect(out.screenshot).toBe("UE5H");
    expect(held).toBe("rm -rf ~");
    expect(polls).toBe(2);
  });

  it("computer wait false returns pending and a rejection throws", async () => {
    current = await serve((req, res) => {
      res.setHeader("Content-Type", "application/json");
      if (req.method === "POST") {
        res.statusCode = 202;
        res.end(heldComputerBody);
        return;
      }
      res.end(
        `{"id":"int_c","kind":"computer","mode":"ask","state":"rejected","payload":{"computer":{"action":"type"}},` +
          `"decision":{"outcome":"reject","reason":"not that"}}`,
      );
    });
    const envs = current.client.environments;

    const pending = await envs
      .computer("vm-1", { action: "type", text: "x" }, { wait: false })
      .catch((e: unknown) => e);
    expect(pending).toBeInstanceOf(PendingInterventionError);
    expect((pending as PendingInterventionError).intervention.kind).toBe(
      HitlKind.Computer,
    );

    const rejected = await envs
      .computer("vm-1", { action: "type", text: "x" })
      .catch((e: unknown) => e);
    expect(rejected).toBeInstanceOf(InterventionRejectedError);
    expect((rejected as InterventionRejectedError).intervention.decision?.reason).toBe(
      "not that",
    );

    // the tool helper hands a rejection to the model instead of failing.
    const tool = await envs.computerToolResult("vm-1", { action: "type", text: "x" });
    expect(tool.is_error).toBe(true);
  });

  it("computer tool result turns a hitl deny into an is_error result", async () => {
    current = await serve((_req, res) => {
      res.statusCode = 403;
      res.setHeader("Content-Type", "application/json");
      res.end(
        `{"error":{"code":"forbidden","message":"typing secrets is denied","details":{"reason":"hitl_denied"}}}`,
      );
    });

    const tool = await current.client.environments.computerToolResult("vm-1", {
      action: "type",
      text: "x",
    });
    expect(tool.is_error).toBe(true);
  });

  it("requestReview returns the reviewer's comments", async () => {
    let sent: Record<string, unknown> = {};
    current = await serve(async (req, res) => {
      res.setHeader("Content-Type", "application/json");
      if (req.method === "POST" && action(queryOf(req)) === "request") {
        sent = JSON.parse(await readBody(req));
        res.statusCode = 201;
        res.end(
          `{"id":"int_r","kind":"review","state":"pending","payload":{"review":{"content":"diff --git a b","format":"diff"}}}`,
        );
        return;
      }
      res.end(
        `{"id":"int_r","kind":"review","state":"rejected","payload":{"review":{"content":"diff --git a b","format":"diff"}},` +
          `"decision":{"outcome":"reject","reason":"hunk 2 drops a check"}}`,
      );
    });

    const out = await current.client.environments.requestReview("vm-1", {
      content: "diff --git a b",
      format: ReviewFormat.Diff,
    });

    expect(sent).toMatchObject({
      kind: "review",
      content: "diff --git a b",
      format: "diff",
    });
    expect(out.approved).toBe(false);
    expect(out.comments).toBe("hunk 2 drops a check");
    expect(out.intervention.payload.review?.format).toBe("diff");
  });

  it("a decider allow is settled, a human approval waits for its result", () => {
    const i = {
      id: "int_c",
      kind: HitlKind.Computer,
      mode: GateMode.Ask,
      state: "approved",
      payload: { computer: { action: "type", input: {} } },
      decision: {
        outcome: "approve",
        decided_by: { id: "decider:typing", source: ACTOR_SOURCE_DECIDER },
        decided_at: "",
      },
    } as unknown as Intervention;
    expect(isInterventionSettled(i)).toBe(true);
    i.decision!.decided_by = { id: "alice", source: "key" };
    expect(isInterventionSettled(i)).toBe(false);
  });

  it("approve sends edited_text", async () => {
    let sent: Record<string, unknown> = {};
    let act: string | null = null;
    current = await serve(async (req, res) => {
      act = action(queryOf(req));
      sent = JSON.parse(await readBody(req));
      res.setHeader("Content-Type", "application/json");
      res.end(
        `{"id":"int_c","state":"approved","decision":{"outcome":"approve","edited_text":"ls"}}`,
      );
    });

    const i = await current.client.interventions.approve("int_c", { editedText: "ls" });

    expect(act).toBe("approve");
    expect(sent.edited_text).toBe("ls");
    expect(i.decision?.edited_text).toBe("ls");
  });
});

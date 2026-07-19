import { defineConfig } from "blume";

// pages moved into diataxis quadrants; keep the old urls alive.
const movedGuides = [
  "agent-sandboxes",
  "ci-runners",
  "gpu-workloads",
  "interactive-debugging",
];

const movedConcepts = [
  "fusefile",
  "providers",
  "hosts",
  "environments",
  "snapshots",
  "artifacts",
  "scheduling",
  "reconcile",
  "state-and-recovery",
];

// the reference tree moved from /docs/reference to /reference (its own tab),
// and the sdks/ level was flattened away. keep every old url alive.
const sdkPages = [
  "",
  "/quickstart",
  "/environments",
  "/hosts",
  "/snapshots",
  "/api-keys",
  "/errors",
  "/advanced",
];

const cliPages = [
  "",
  "/up",
  "/init",
  "/metrics",
  "/contexts",
  "/contexts/connect",
  "/contexts/list",
  "/contexts/use",
  "/contexts/remove",
  "/contexts/current",
  "/environment",
  "/environment/list",
  "/environment/get",
  "/environment/create",
  "/environment/destroy",
  "/environment/drain",
  "/environment/fork",
  "/environment/rotate-token",
  "/environment/watch",
  "/environment/exec",
  "/environment/shell",
  "/hosts",
  "/hosts/list",
  "/hosts/register",
  "/hosts/get",
  "/hosts/cordon",
  "/hosts/uncordon",
  "/hosts/remove",
  "/hosts/metrics",
  "/snapshot",
  "/snapshot/create",
  "/snapshot/list",
  "/snapshot/get",
  "/snapshot/delete",
  "/snapshot/restore",
  "/apikeys",
  "/apikeys/create",
  "/apikeys/list",
  "/apikeys/revoke",
];

// hand-written api pages were replaced by the generated openapi reference.
const retiredApiPages = [
  "",
  "/environments",
  "/hosts",
  "/snapshots",
  "/api-keys",
  "/health",
  "/auth-endpoints",
];

export default defineConfig({
  title: "Fuse",
  description:
    "A control plane for provisioning sandboxed compute environments on your own hosts.",
  // content lives in content/docs, so the "docs" folder becomes the /docs url
  // segment and every existing link keeps working unchanged.
  content: {
    root: "content",
    sources: [
      { type: "filesystem", root: "content" },
      // /changelog is generated from github releases (release-please publishes
      // one per version). builds read GITHUB_TOKEN when set; the public repo
      // works without it.
      {
        type: "github-releases",
        owner: "folsomintel",
        repo: "fuse",
        prefix: "changelog",
      },
    ],
  },
  github: {
    owner: "folsomintel",
    repo: "fuse",
    branch: "main",
    dir: "docs",
  },
  navigation: {
    tabs: [
      { label: "Docs", path: "/docs", icon: "book-open" },
      { label: "Reference", path: "/reference", icon: "file-code" },
      { label: "Changelog", path: "/changelog", icon: "history" },
    ],
  },
  // the rest api reference is generated from the orchestrator's spec; each
  // operation becomes a page under /reference/api.
  openapi: {
    enabled: true,
    route: "/reference/api",
    sources: [{ label: "API", spec: "../internal/api/openapi.yaml" }],
  },
  redirects: [
    // no page is authored at / or /docs, so both 404 without these.
    // temporary (302) so a future real landing page isn't shadowed by
    // browser-cached permanent redirects.
    { from: "/", to: "/docs/learn", status: 302 as const },
    { from: "/docs", to: "/docs/learn", status: 302 as const },
    ...movedGuides.map((slug) => ({
      from: `/docs/learn/use-cases/${slug}`,
      to: `/docs/guides/${slug}`,
      status: 308 as const,
    })),
    ...movedConcepts.map((slug) => ({
      from: `/docs/learn/concepts/${slug}`,
      to: `/docs/concepts/${slug}`,
      status: 308 as const,
    })),
    { from: "/docs/reference", to: "/reference", status: 308 as const },
    {
      from: "/docs/reference/auth",
      to: "/reference/auth",
      status: 308 as const,
    },
    ...["go", "python", "typescript"].flatMap((lang) =>
      sdkPages.map((page) => ({
        from: `/docs/reference/sdks/${lang}${page}`,
        to: `/reference/${lang}${page}`,
        status: 308 as const,
      })),
    ),
    ...cliPages.map((page) => ({
      from: `/docs/reference/cli${page}`,
      to: `/reference/cli${page}`,
      status: 308 as const,
    })),
    ...retiredApiPages.map((page) => ({
      from: `/docs/reference/api${page}`,
      to: "/reference/api",
      status: 308 as const,
    })),
  ],
});

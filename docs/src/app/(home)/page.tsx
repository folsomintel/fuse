import Link from "next/link";

export default function HomePage() {
  return (
    <div className="flex flex-col justify-center text-center flex-1 max-w-2xl mx-auto px-4 py-16">
      <h1 className="text-3xl font-bold mb-4">A control plane for agents</h1>
      <p className="text-lg text-fd-muted-foreground mb-4">
        Deploy Firecracker microVMs anywhere, with one script.
      </p>
      <p className="mb-8">
        Fuse lets you stand up isolated microVMs on your own hosts and drive
        their full lifecycle through a REST API, scheduling, snapshots, host
        management, and live event streams, built on Firecracker and entirely
        open source.
      </p>
      <ul className="text-left mb-8 space-y-2">
        <li>
          <strong>One-script Firecracker setup</strong>: host, agent, and a
          baked rootfs, ready to boot.
        </li>
        <li>
          <strong>Scheduling across hosts</strong>: register hosts, and Fuse
          places microVMs for you.
        </li>
        <li>
          <strong>Optional whole-GPU environments</strong>: route GPU requests
          to QEMU/VFIO hosts.
        </li>
        <li>
          <strong>Full VM lifecycle</strong>: provision, running, drain,
          destroy, tracked and reconciled.
        </li>
        <li>
          <strong>Snapshots</strong>: capture a running microVM and restore from
          it.
        </li>
        <li>
          <strong>Live event streams</strong>: tail a microVM&apos;s events over
          SSE.
        </li>
        <li>
          <strong>Durable state</strong>: backed by Postgres, or in-memory for
          local runs.
        </li>
        <li>
          <strong>Per-VM secrets</strong>: credentials generated per microVM,
          tokens encrypted at rest.
        </li>
        <li>
          <strong>REST API and Prometheus metrics</strong>: drive everything
          over HTTP, observe it out of the box.
        </li>
      </ul>
      <div className="flex justify-center gap-4">
        <Link href="/docs/learn" className="font-medium underline">
          Learn
        </Link>
        <Link href="/docs/learn/quickstart" className="font-medium underline">
          Quickstart
        </Link>
      </div>
    </div>
  );
}

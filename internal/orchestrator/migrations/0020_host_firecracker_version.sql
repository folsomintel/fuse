-- firecracker version per host (issue #283): what the host agent reports it
-- runs, and the release its fc-agent.py pins, both from the capacity probe at
-- registration. live migration needs both ends on the same release, so the
-- operator can see skew before a migrate is refused. '' is a qemu host, an
-- agent older than the fields, or a row from before this column.
ALTER TABLE orchestrator_hosts
    ADD COLUMN IF NOT EXISTS firecracker TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS firecracker_pinned TEXT NOT NULL DEFAULT '';

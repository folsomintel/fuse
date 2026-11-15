# fuse python sdk

python client for the fuse microvm control plane. mirrors the go sdk.

## install

```sh
uv add folsom-fuse
```

## usage

```python
import fuse

with fuse.Client("https://orchestrator.example.com", token="...") as client:
    env = client.environments.create(
        fuse.CreateRequest(task_id="t-1", spec=fuse.Spec(cpus=2, ram_mb=2048))
    )

    for event in client.environments.events(env.id):
        if event.err:
            raise event.err
        print(event.state)
        if fuse.is_terminal_state(event.state):
            break
```

services hang off the client: `client.environments`, `client.snapshots`,
`client.hosts`, `client.api_keys`, `client.interventions`, `client.operations`.

the mutating calls (`environments.create`, `destroy`, `drain`, `fork`,
`migrate`, `snapshots.create`, `snapshots.restore`) take an optional
`idempotency_key=`; a retry with the same key and request returns the first
call's result instead of running again. without one each call sends a fresh
random key. a call that outlasts the server's wait is followed through
`client.operations` until it finishes, so it returns or raises as usual;
`client.operations.get(id, wait=None)` and `client.operations.wait(id,
timeout=None)` read an operation directly.

errors raise `fuse.ApiError`; use predicates like `fuse.is_not_found(err)` to
branch on the server error code.

## development

```sh
uv sync
uv run pytest
uv run ruff check .
uv run mypy
```

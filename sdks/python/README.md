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
`client.hosts`, `client.api_keys`.

errors raise `fuse.ApiError`; use predicates like `fuse.is_not_found(err)` to
branch on the server error code.

## development

```sh
uv sync
uv run pytest
uv run ruff check .
uv run mypy
```

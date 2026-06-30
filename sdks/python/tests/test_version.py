from importlib.metadata import version

import fuse


def test_version_matches_installed_metadata() -> None:
    assert fuse.VERSION == version("folsom-fuse")

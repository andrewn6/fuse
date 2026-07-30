# /// script
# requires-python = ">=3.9"
# dependencies = ["folsom-fuse", "python-dotenv"]
# ///
import os

from dotenv import load_dotenv

import fuse

# reads .env sitting next to this file, so it works from any cwd
load_dotenv(os.path.join(os.path.dirname(os.path.abspath(__file__)), ".env"))

base_url = os.environ["FUSE_URL"]
token = os.environ["FUSE_TOKEN"]

with fuse.Client(base_url=base_url, token=token) as client:
    hosts = client.hosts.list()
    envs = client.environments.list()

    print(f"sdk {fuse.VERSION} -> {base_url}")
    print(f"hosts: {len(hosts)}")
    for h in hosts:
        print(f"  {h.id} {h.state} {h.backend} cpus={h.capacity.cpus}")

    print(f"environments: {len(envs)}")
    for e in envs:
        print(f"  {e.id} {e.state} cpus={e.spec.cpus} ram={e.spec.ram_mb}mb")

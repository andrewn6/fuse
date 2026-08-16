"""drive a fuse desktop environment with claude's computer use tool.

computer use is a client-side tool: claude never connects to the sandbox.
claude emits a tool_use block, this loop executes it against the environment
through fuse's computer endpoint, and feeds the screenshot back as the
tool_result. the translation is computer_tool_result(), so the loop below is
all there is to it.

usage:
    export FUSE_URL=... FUSE_TOKEN=... ANTHROPIC_API_KEY=...
    uv run --with anthropic --with folsom-fuse computer_use.py \
        "open firefox and tell me the title of the page it starts on"

requires a host with a desktop image baked (see the desktop environments
guide) and a model that takes the computer_20251124 tool.
"""

from __future__ import annotations

import os
import secrets
import sys

import anthropic

import fuse

task = sys.argv[1] if len(sys.argv) > 1 else "take a screenshot and describe the desktop"

client = fuse.Client(os.environ["FUSE_URL"], os.environ["FUSE_TOKEN"])
claude = anthropic.Anthropic()

env = client.environments.create(
    fuse.CreateRequest(
        task_id=f"computer-use-{secrets.token_hex(3)}",
        spec=fuse.Spec(cpus=2, ram_mb=4096, image="desktop"),
        desktop=fuse.DesktopSpec(width=1280, height=800),
    )
)
print(f"environment {env.id} ({env.state})")

try:
    # size the tool definition from the live display rather than hardcoding it
    display = client.environments.computer_display(env.id)
    tool = {
        "type": "computer_20251124",
        "name": "computer",
        "display_width_px": display.width or 1280,
        "display_height_px": display.height or 800,
        "display_number": 1,
        "enable_zoom": True,
    }

    messages: list[dict] = [{"role": "user", "content": task}]
    while True:
        msg = claude.beta.messages.create(
            model="claude-opus-5",
            max_tokens=4096,
            tools=[tool],
            betas=["computer-use-2025-11-24"],
            messages=messages,
        )
        messages.append({"role": "assistant", "content": msg.content})

        if msg.stop_reason != "tool_use":
            for block in msg.content:
                if block.type == "text":
                    print(block.text)
            break

        results = []
        for block in msg.content:
            if block.type == "tool_use" and block.name == "computer":
                r = client.environments.computer_tool_result(env.id, dict(block.input))
                results.append(
                    {
                        "type": "tool_result",
                        "tool_use_id": block.id,
                        "content": r.content,
                        "is_error": r.is_error,
                    }
                )
        messages.append({"role": "user", "content": results})
finally:
    # drain is phase-1 only and does not destroy the vm (see the drain()
    # docstring) - a real agent loop might drain first to inspect output,
    # but this example is done with the environment, so it destroys outright.
    client.environments.destroy(env.id)
    print(f"destroyed {env.id}")

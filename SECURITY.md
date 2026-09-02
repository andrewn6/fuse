# Security Policy

## Supported Versions

Fuse is pre-1.0. Only the latest release receives security fixes.

| Version | Supported          |
| ------- | ------------------ |
| 0.29.x  | :white_check_mark: |
| < 0.29  | :x:                |

## Reporting a Vulnerability

Please do not open a public issue for security reports.

Report privately through GitHub: open a
[new draft advisory](https://github.com/folsomintel/fuse/security/advisories/new)
from the Security tab. Only you and the maintainers can see it.

Include what you can:

- what the issue is and which component it affects (orchestrator, fused, host agent, CLI, SDKs)
- steps to reproduce, ideally a minimal `Fusefile` or API call
- what an attacker gains

What to expect:

- acknowledgement within 3 business days
- a status update within 7 days
- a fix and a published advisory once a patched release is out

Because Fuse runs workloads in microVMs on hosts you control, we are especially
interested in guest-to-host escapes, orchestrator or host-agent auth bypass, and
anything that lets one environment reach another's data.

Fuse is MIT licensed and provided as is. There is no bug bounty.

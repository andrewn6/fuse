# Fusefile examples

These are reference Fusefile fixtures, not documentation pages — they are not
served by the docs site. They cover the canonical scenarios from
[Your first Fusefile](/docs/learn/first-fusefile) and
[Fusefile](/docs/concepts/fusefile): a CPU-only baseline, whole-GPU and MIG
fractional-GPU requests, and a services-with-secrets manifest.

Each file is self-describing; the inline comments explain what scenario it
exercises and which doc it corresponds to. Use them as starting points with
[`fuse validate`](/reference/cli/validate) / [`fuse compile`](/reference/cli/compile)
and [`fuse build`](/reference/cli/build).

| File | Scenario |
| --- | --- |
| `Fusefile.cpu` | CPU-only baseline — scheduling, boot, startup script. |
| `Fusefile.gpu-whole` | Whole-device GPU passthrough (vfio). |
| `Fusefile.gpu-mig` | Fractional GPU via MIG profile (`1g.10gb`). |
| `Fusefile.services` | Services + secrets — manifest compilation and piping. |

# fuse-attach/1

`fuse-attach/1` is the wire protocol behind `fuse environment shell`. It carries an
interactive pty session between the CLI and a process inside a guest, across two HTTP
hops, as a stream of length-prefixed binary frames.

This document is the specification. The implementations that exist today are:

| Role | File |
| --- | --- |
| Client (frame codec, dialer) | [`sdks/go/exec.go`](../sdks/go/exec.go) |
| Relay (hijack + byte pump) | [`internal/api/exec.go`](../internal/api/exec.go) |
| Relay (dial the host agent) | [`internal/hostwire/dial.go`](../internal/hostwire/dial.go), [`internal/hostwire/attach.go`](../internal/hostwire/attach.go) |
| Server (pty, frame codec) | [`host-agent/firecracker/fc-agent.py`](../host-agent/firecracker/fc-agent.py), [`host-agent/qemu/qemu-agent.py`](../host-agent/qemu/qemu-agent.py) |

## Why it exists

**The orchestrator cannot reach a guest.** Each VM sits on its own `/30` TAP on its
host (`10.200.<idx>.1` for the host end, `10.200.<idx>.2` for the guest), routable only
from that host. The orchestrator's only route to guest-adjacent anything is the host
agent's HTTP API. So an interactive session cannot be a direct connection to the VM: every
byte in both directions has to relay through the host agent, which is the only process
that can open a pty onto the guest.

**There is no WebSocket in the tree, and there will not be one for this.** The Go SDK is
deliberately dependency-free (`sdks/go/go.mod` has no `require` block), and the host agents
are single-file stdlib Python. A raw HTTP/1.1 upgrade is stdlib on all three ends: Go's
`http.NewResponseController(...).Hijack()` on the server, `net.Conn` plus `http.ReadResponse`
on the client, `BaseHTTPRequestHandler`'s `rfile`/`wfile` on the host agent. A WebSocket
would mean a dependency in the SDK and a hand-rolled RFC 6455 framer in Python, in exchange
for masking and ping frames that nothing here needs.

**The same protocol runs on both hops.** That is the load-bearing design choice: the
orchestrator never encodes or decodes a frame. Once both upgrades are complete it is an
`io.Copy` in each direction and nothing else, so frames can change without the orchestrator
knowing.

## Topology

```
  fuse CLI  /  Go SDK
      |
      |  hop 1:  GET /v1/environments/{vmId}/attach       Upgrade: fuse-attach/1
      |          Authorization: Bearer <master token>
      v
  orchestrator                          (parses the spec, opens hop 2, then hijacks)
      |
      |  hop 2:  GET /v1/vm/{vmId}/attach                 Upgrade: fuse-attach/1
      |          Authorization: Bearer <host agent token>
      v
  host agent                            (pty.fork -> ssh -tt root@10.200.N.2 [cmd...])
      |
      v
  guest
```

The orchestrator does not forward the client's request. It parses the attach spec out of
the query string (`hostwire.ParseAttachQuery`), re-encodes it (`hostwire.AttachQuery`), and
issues a fresh request to the host agent with the host agent's own token. Only the byte
stream *after* the two `101` responses is relayed verbatim.

## Handshake

### Hop 1: client to orchestrator

The client writes the request head onto a raw TCP (or TLS) connection. It is a normal
HTTP/1.1 request with no body:

```
GET /v1/environments/vm-1/attach?cmd=sh&cmd=-c&cmd=echo+hi&cols=80&rows=24&tty=1 HTTP/1.1
Host: orchestrator.example:8080
Authorization: Bearer <master token>
Connection: Upgrade
Upgrade: fuse-attach/1
User-Agent: fuse-go/<version>
```

Header order is not significant. What the orchestrator actually requires:

- method `GET` on `/v1/environments/{vmId}/attach`.
- `Upgrade: fuse-attach/1`, compared case-insensitively on the value. Anything else is
  `400 invalid_argument`.
- a master-token `Authorization` header. API keys are refused with `403 unauthorized`.
  Attach is a root shell in the guest, and API keys carry no scopes today.

`Connection: Upgrade` is not checked by the orchestrator, but send it anyway: HTTP/1.1
requires it and intermediaries key on it.

The response is exactly these bytes, written straight onto the hijacked socket. There is
no `Date`, no `Content-Length`, no body:

```
HTTP/1.1 101 Switching Protocols\r\n
Upgrade: fuse-attach/1\r\n
Connection: Upgrade\r\n
\r\n
```

Everything after that final `\r\n` is frames. The connection is no longer an HTTP
conversation and is never reused.

### Hop 2: orchestrator to host agent

Identical protocol, different path and token:

```
GET /v1/vm/vm-1/attach?cmd=sh&cmd=-c&cmd=echo+hi&cols=80&rows=24&tty=1 HTTP/1.1
Host: fc-host.internal:8080
Authorization: Bearer <host agent token>
Connection: Upgrade
Upgrade: fuse-attach/1
```

The host agent requires the bearer token and `tty=1`; it does not inspect the `Upgrade`
header (it routes on the path and method), but a client that omits it is out of spec. It
answers with the same three-line `101` and then hijacks its own socket.

### Ordering, and what can still fail

The orchestrator opens hop 2 **before** it hijacks the client connection. That ordering is
what lets failures be reported as HTTP: once the socket is hijacked there is no
`ResponseWriter` left to write an error through.

Errors before the `101` use the standard envelope, `{"error":{"code":"...","message":"..."}}`:

| Status | Code | Cause |
| --- | --- | --- |
| 400 | `invalid_argument` | missing or wrong `Upgrade` header |
| 403 | `unauthorized` | not the master token |
| 404 | `not_found` | unknown VM |
| 409 | `conflict` | VM is not in state `running` (draining, provisioning, ...) |
| 501 | `unimplemented` | the provider has no guest to attach to (e.g. the stub) |
| 500 | `internal` | host agent unreachable, or it refused the upgrade |

That last row includes `tty=1` being absent: the host agent is the party that enforces it,
so its `400` arrives on hop 2 and surfaces to the client as a `500 internal` whose message
carries the host agent's own text. Send `tty=1`.

Handshake deadlines: the Go client bounds the exchange at 30s (or the context deadline);
the orchestrator bounds hop 2 at 15s. **Both clear the socket deadline entirely once the
`101` lands**, because the stream that follows idles for as long as a human stares at a
prompt.

## Frame format

Every byte after the `101`, in both directions, is part of a frame.

```
 0       1       2       3       4       5       6       7       8
 +-------+-------+-------+-------+-------+-------+-------+-------+---------------+
 | type  |         reserved      |        length (u32, BE)       |    payload    |
 +-------+-------+-------+-------+-------+-------+-------+-------+---------------+
 byte 0    bytes 1..3              bytes 4..7                      bytes 8..8+len
```

- **type** (byte 0): one of the values in the table below.
- **reserved** (bytes 1 to 3): senders MUST write zeros. Receivers MUST ignore them. They
  round the header to 8 bytes and leave room to grow.
- **length** (bytes 4 to 7): unsigned 32-bit, big-endian, the payload length in bytes.
  Zero is legal. Maximum is `1 << 20` (1 MiB). A larger value MUST be rejected without
  allocating (see Constraints).
- **payload**: exactly `length` bytes.

The header is always 8 bytes. There is no checksum, no stream id, no continuation bit: the
connection carries exactly one session.

| Type | Name | Direction | Payload |
| --- | --- | --- | --- |
| `0` | stdin | client to server | raw bytes, written to the pty master |
| `1` | stdout | server to client | raw bytes read from the pty master |
| `2` | stderr | server to client | raw bytes. Unused today: a pty merges the guest's stdout and stderr into one stream, so the host agent only ever emits type `1`. Reserved so a future non-pty mode can split them. |
| `3` | resize | client to server | JSON `{"rows":24,"cols":80}` |
| `4` | exit | server to client | JSON `{"exit_code":0}` |

Rules that every implementation follows and a third one must:

- **Receivers ignore frames they do not handle**, rather than erroring. The host agent
  drops inbound `1`/`2`/`4`; the CLI drops inbound `0`/`3`. This is what makes a new frame
  type additive.
- **JSON payloads are parsed, not byte-compared.** The Go client emits
  `{"cols":80,"rows":24}` (Go sorts map keys); the Python agent emits `{"exit_code": 0}`
  (with a space, because that is `json.dumps`'s default separator). Key order and
  whitespace are insignificant.
- **A TCP read has no relationship to a frame boundary.** One read can deliver half a
  header, or three whole frames plus a fragment. Decode incrementally: buffer, take frames
  while the buffer holds a full header plus its payload, keep the remainder.
- Frame size in practice: the host agent reads the pty in 64 KiB chunks, and the Go client
  frames stdin in `io.Copy`-sized (32 KiB) chunks. Nothing depends on this. A conforming
  receiver handles anything up to 1 MiB.

### Worked bytes

Stdin frame carrying `ls\n`:

```
00 00 00 00  00 00 00 03  6c 73 0a
^type        ^length=3    ^payload
```

Resize frame from the Go client, 24 rows by 80 cols (21-byte payload):

```
03 00 00 00  00 00 00 15  7b 22 63 6f 6c 73 22 3a 38 30 2c 22 72 6f 77 73 22 3a 32 34 7d
^type        ^length=21   ^{"cols":80,"rows":24}
```

Exit frame from the Python host agent, exit code 0 (16-byte payload, note the space):

```
04 00 00 00  00 00 00 10  7b 22 65 78 69 74 5f 63 6f 64 65 22 3a 20 30 7d
^type        ^length=16   ^{"exit_code": 0}
```

## The attach spec (query string)

An upgrade is a `GET`, so there is no request body to carry the spec. It rides in the query
string instead. Inventing a pre-upgrade handshake frame would buy nothing.

| Param | Required | Meaning |
| --- | --- | --- |
| `tty` | yes | Must be `1` (`true` is also accepted). This protocol is pty-only; the host agent rejects anything else with `400`. |
| `rows` | no | Initial pty rows. Parsed as an integer in `1..65535`; anything else is treated as unset, and an unset or zero value leaves the pty at its default size. |
| `cols` | no | Initial pty cols. Same parsing. |
| `cmd` | no | argv to run. **Repeats.** Absent means the guest's login shell. |

`cmd` repeats once per argv element rather than arriving as one string, because that
preserves argv boundaries: `?cmd=sh&cmd=-c&cmd=echo+hi` is unambiguously
`["sh", "-c", "echo hi"]`. A single flattened string would need a quoting convention,
and a quoting convention is an injection bug waiting for a caller to interpolate a value
into it. Standard form encoding applies: values are percent-encoded and a space is `+`.

One sharp edge: **do not send empty-string argv elements.** The host agent parses the query
with Python's `parse_qs`, which drops blank values by default, so an empty argument would
silently vanish and shift the argv.

Rows and cols are only the *seed*. Every subsequent terminal resize is a type `3` frame on
the stream itself.

## Session lifecycle

1. **Client dials and writes the request head.** It may write stdin or resize frames
   immediately afterwards, without waiting for the `101`. A correct server does not lose
   them (see Constraints).
2. **Orchestrator authenticates, parses the spec, opens hop 2, hijacks, writes the `101`.**
3. **Host agent** validates `tty=1`, drains anything already buffered, writes its own `101`,
   then `pty.fork()`s a child running `ssh -tt root@<guest_ip> [cmd...]`. `-tt` forces a pty
   on the guest side even when a command is given. `pty.fork` rather than `subprocess`
   because the child needs the pty as its *controlling terminal in a new session*, which is
   what makes `SIGWINCH` delivery, and therefore resizing, work at all.
4. **Initial window size** is applied with `TIOCSWINSZ` from `rows`/`cols`.
5. **Steady state.** The client sends type `0` (stdin) and type `3` (resize). The host agent
   sends type `1` (stdout). The orchestrator copies bytes. A resize frame becomes another
   `TIOCSWINSZ`, and the kernel delivers `SIGWINCH` to the pty's foreground process group.
   There are no heartbeat or ping frames; an idle session is held open by TCP alone.

A session ends in one of two ways:

- **The guest process exits.** The pty reports EOF, the host agent reaps the child, and
  sends a final type `4` frame: `{"exit_code": N}` where `N` is `WEXITSTATUS(status)`, or
  `128 + signal` if it was killed. Then it closes the socket. The client sees the exit frame
  followed by EOF; the CLI adopts `exit_code` as its own process exit status.
- **The client goes away.** The host agent `SIGKILL`s the pty child and reaps it. The exit
  frame is best effort: there is nobody left to send it to, and that is not a failure.

Either side closing tears down the other. In the orchestrator's relay, whichever direction
finishes its `io.Copy` first ends the session; the deferred `Close` on both sockets unblocks
the surviving goroutine's pending read.

Note the kill is conditional. On a normal exit the pty EOFs a hair before the child is
reaped, so an unconditional `SIGKILL` would race a process that was already exiting cleanly
and report `137` in place of its real status.

## Constraints you will trip over

Each of these is a bug someone already had.

**1. HTTP/1.1, on a raw connection. Not `http.Client`.**
HTTP/2 has no connection upgrade at all, and an `http.Client` speaking TLS may negotiate h2
via ALPN and silently break the handshake. Worse, `net/http` gives a *client* no way to
reclaim the socket after a response: only servers get `Hijack`. Both the Go SDK and the
orchestrator therefore dial a `net.Conn` themselves, `req.Write(conn)` the head, and
`http.ReadResponse` the `101` off a `bufio.Reader`. That pins the exchange to HTTP/1.1,
where an upgrade is well defined.

**2. Drain the reader's buffer after the request head.**
A client that writes its head and immediately writes a frame leaves those frame bytes
sitting in the *server's* read buffer. `select()`/`poll()` on the socket will never report
them: they are already out of the kernel. Every hop drains explicitly.

- Orchestrator: `Hijack()` returns a `*bufio.ReadWriter`; the relay reads from
  `buf.Reader`, never the raw conn.
- Host agent: `drain_buffered()` puts the socket in non-blocking mode and `peek(0)`s
  `rfile` before entering the selector loop.
- Go client: reads go through the same `bufio.Reader` that `http.ReadResponse` used, because
  it may have pulled stream bytes in along with the response head.

Skip this and the first keystrokes of a fast client, or the first output of a fast guest,
are silently eaten. The session still works, which is what makes it hard to find.

**3. Clear the write deadline on the hijacked socket.**
The orchestrator's `http.Server` has a `WriteTimeout` (`--write-timeout`, 60s by default),
and `Hijack` hands back a socket with that deadline still armed. An interactive shell idles
for as long as a human stares at a prompt, so the handler calls `SetDeadline(time.Time{})`
immediately after hijacking. Without it the session dies exactly one write-timeout after it
starts. The client does the same after its `101`.

**4. Bound the length before you allocate.**
`length` is attacker-controlled: it is four bytes off a socket. Check it against the 1 MiB
maximum *before* allocating a buffer of that size, or a peer that sends `ff ff ff ff` asks
you for 4 GiB. The Go client returns an error; the Python agent raises and tears the session
down.

**5. Write to the peer unbuffered.**
The relay writes straight to the socket, not through the hijacked `bufio.Writer`. A buffered
writer would sit on a keystroke until something else happened to flush it, which on an
interactive terminal reads as the connection being broken.

**6. Serialize writes on the client.**
The resize handler fires from a signal (`SIGWINCH`) while an `io.Copy` from stdin is running.
Without a write mutex a resize frame interleaves into the middle of a stdin frame's bytes and
desynchronizes the stream permanently. `AttachStream` takes a mutex around every `WriteFrame`
for exactly this reason.

## What this is not

- **It is not SSH.** SSH is an implementation detail *inside* the host agent, one hop away
  from anything a client can see. There is no `scp`/`sftp`, no port forwarding, no
  `-o` options, no agent forwarding, no key material anywhere near the client. The client
  speaks frames.
- **It is tty-only.** `tty=1` is mandatory. For a non-interactive command use exec:
  `POST /v1/environments/{vmId}?action=exec` with `{"cmd":["ls","-l"]}` or
  `{"shell":"ls | wc -l"}`, which returns `{"exit_code":N,"stdout":"...","stderr":"..."}`
  with stdout and stderr kept separate. A non-zero `exit_code` there is still HTTP `200`:
  the command ran and it failed, which is an answer, not an error.
- **It has no half-close.** There is no "stdin is done" frame. A command that reads to EOF
  on stdin will not see one; that is what exec is for.
- **It is Go-only.** The TypeScript and Python SDKs implement exec but not attach, because
  the CLI is the only consumer of attach and the CLI is Go.

## Versioning

The version is the `Upgrade` token. Adding a frame type is backwards compatible, because
receivers ignore types they do not handle and the orchestrator never looks at one. Anything
that would change the meaning of an existing byte gets a new token, `fuse-attach/2`, and the
old one keeps working until it does not need to.

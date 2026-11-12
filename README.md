# fsdeck

A web console for operating FreeSWITCH. Point it at the Event Socket of any
running FreeSWITCH and get status, SIP, calls, effective configuration, live
logs and a command console in the browser.

- Nothing to install on the FreeSWITCH side: only `mod_event_socket`, which
  the stock configuration already loads.
- Does not own your configuration. fsdeck reads what FreeSWITCH reports; it
  has no database schema and no XML generator.
- One static binary, standard library only, frontend embedded.
- Several FreeSWITCH instances in one console.

## Features

| Area | What you get | ESL command behind it |
|---|---|---|
| Overview | status, version, uptime, sessions, session rate, idle CPU | `json status`, `HEARTBEAT` events |
| SIP | profiles, gateways, aliases, registrations; profile rescan | `sofia xmlstatus`, `show registrations` |
| Calls | channels and calls, hang up | `show channels`, `show calls`, `uuid_kill` |
| Config | effective configuration as FreeSWITCH parsed it, global variables, modules | `xml_locate`, `global_getvar`, `show modules` |
| Logs | live log tail with level filter | `log <level>` |
| Console | any API command, like `fs_cli` | `api <command>` |

Passwords, DSNs and similar values are masked in the configuration and
variable views.

## Quick start

```sh
go build -o fsdeck ./cmd/fsdeck
FSDECK_ADMIN_PASSWORD='choose-one' ./fsdeck
```

Open <http://127.0.0.1:8080>, log in as `admin`, add an instance (host, ESL
port, ESL password).

With Docker:

```sh
docker build -t fsdeck .
docker run --rm -p 127.0.0.1:8080:8080 -v fsdeck-data:/data \
  -e FSDECK_ADMIN_PASSWORD='choose-one' fsdeck
```

## Connecting FreeSWITCH

fsdeck connects to `mod_event_socket` (default port 8021). Out of the box
FreeSWITCH only accepts Event Socket connections from the local host and uses
the well-known password `ClueCon`. To reach it from another host, edit
`autoload_configs/event_socket.conf.xml`:

```xml
<configuration name="event_socket.conf" description="Socket Client">
  <settings>
    <param name="listen-ip" value="0.0.0.0"/>
    <param name="listen-port" value="8021"/>
    <param name="password" value="a-long-random-password"/>
    <param name="apply-inbound-acl" value="fsdeck"/>
  </settings>
</configuration>
```

and allow the fsdeck host in `autoload_configs/acl.conf.xml`:

```xml
<list name="fsdeck" default="deny">
  <node type="allow" cidr="192.0.2.10/32"/>
</list>
```

Then `reloadacl` and `reload mod_event_socket`. Change the default password
before the port is reachable from anywhere else: the Event Socket gives full
control of the switch. An SSH tunnel to `127.0.0.1:8021` works without any of
these changes.

## Configuration

| Flag | Environment | Default | Purpose |
|---|---|---|---|
| `-listen` | `FSDECK_LISTEN` | `127.0.0.1:8080` | HTTP listen address |
| `-data` | `FSDECK_DATA` | `fsdeck.json` | instance list file (holds ESL passwords, mode 0600) |
| | `FSDECK_ADMIN_PASSWORD` | random, printed at start | password of the `admin` account |
| | `FSDECK_VIEWER_PASSWORD` | unset (account disabled) | password of the read-only `viewer` account |

## Accounts

| | `admin` | `viewer` |
|---|---|---|
| Status, SIP, calls, config (masked) | yes | yes |
| Manage instances | yes | no |
| Hang up, rescan, reloadxml, reloadacl | yes | no |
| Logs and console | yes | no |

## Security notes

- The `admin` account can run any FreeSWITCH API command. Treat it like shell
  access to the switch.
- fsdeck serves plain HTTP. Keep it on a trusted network or put it behind a
  TLS-terminating reverse proxy that sets `X-Forwarded-Proto: https`, so the
  session cookie is marked `Secure`.
- ESL passwords are stored in clear in the data file. Protect that file.
- Log lines are not masked and can contain secrets; that is why logs are
  admin-only.

## Not in scope (yet)

- Editing configuration files. The Event Socket has no file API; the plan is
  an optional agent on the FreeSWITCH host.
- Dialplan and directory browsers.
- SIP capture and metrics history. Homer and Prometheus cover those.

## Development

```sh
go vet ./... && go test -race ./...
```

No third-party Go modules and no frontend build step: `web/static` is served
as is.

## License

MIT. FreeSWITCH is a trademark of its owner; fsdeck is an independent project
and is not affiliated with or endorsed by the FreeSWITCH project or SignalWire.

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
| Instances | one card per FreeSWITCH: connection, sessions, idle CPU, uptime, version | `json status` |
| Overview | sessions, session rate, idle CPU and capacity over the last two hours as a chart, SIP and call summary | `json status`, `HEARTBEAT` events |
| SIP | profiles, gateways, aliases, registrations; profile rescan | `sofia xmlstatus`, `show registrations` |
| Calls | channels and bridged calls with live durations, every field in a side panel, hang up | `show channels`, `show calls`, `uuid_kill` |
| Configuration | effective configuration as FreeSWITCH parsed it (highlighted XML with find), global variables, modules | `xml_locate`, `global_getvar`, `show modules` |
| Dialplan | contexts and their extensions read as "if this matches, run these actions", or as XML | `xml_locate dialplan` |
| Directory | domains with a users table (groups, caller id, context, whether a password is set), or as XML | `xml_locate directory` |
| Logs | live log tail with level, severity and text filters | `log <level>` |
| Console | any API command, like `fs_cli` | `api <command>` |

Tables sort by any column and filter as you type. Light and dark themes; the
toggle in the sidebar cycles system, light, dark. When an instance cannot
connect, the card says why in plain words (wrong password, ACL, nothing
listening) next to the raw error.

Passwords, DSNs and similar values are masked in the configuration, dialplan,
directory and variable views, including secrets assigned inside a value such
as `sip_auth_password=...` in a dial string. The load history is kept in
memory, one sample per FreeSWITCH heartbeat (every 20 seconds by default).

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
| `-listen` | `FSDECK_LISTEN` | `127.0.0.1:8080` | listen address |
| `-data` | `FSDECK_DATA` | `fsdeck.json` | data file: instances with their ESL passwords, and accounts (mode 0600) |
| `-audit` | `FSDECK_AUDIT` | `fsdeck-audit.log` beside the data file | audit log; an empty value disables it |
| `-log-backlog` | `FSDECK_LOG_BACKLOG` | `2000` | log lines kept per instance and replayed when the Logs view opens; `0` disables |
| `-log-backlog-level` | `FSDECK_LOG_BACKLOG_LEVEL` | `6` (INFO) | highest log level kept in the backlog, `0` to `7` |
| `-tls-cert`, `-tls-key` | `FSDECK_TLS_CERT`, `FSDECK_TLS_KEY` | unset | serve HTTPS with this PEM certificate and key; both or neither |
| | `FSDECK_SECRET_KEY` | unset | encrypt the ESL passwords in the data file; at least 16 characters |
| | `FSDECK_ADMIN_PASSWORD` | unset | password of the managed `admin` account |
| | `FSDECK_VIEWER_PASSWORD` | unset | password of the managed `viewer` account |
| `-version` | | | print the version and exit |

With a backlog, fsdeck keeps the ESL log level of every instance at least at
`-log-backlog-level`, so FreeSWITCH sends INFO lines even while nobody watches
the Logs view. `GET /healthz` answers `{"status":"ok"}` without signing in, for
load balancers and container health checks.

## Accounts

| | `viewer` | `operator` | `admin` |
|---|---|---|---|
| Status, SIP, calls, configuration, dialplan, directory (masked), load history | yes | yes | yes |
| Logs | no | yes | yes |
| Hang up, rescan, reloadxml, reloadacl | no | yes | yes |
| Console | no | no | yes |
| Manage instances | no | no | yes |
| Manage accounts, read the audit log | no | no | yes |

Accounts live in the data file and admins manage them in the console.
Usernames are 2 to 32 lowercase letters, digits, `.`, `_` or `-`; passwords are
10 to 256 characters and stored as PBKDF2-SHA256 hashes (600000 iterations).
Everyone can change their own password. Changing an account's role, password
or disabled flag, or deleting it, signs it out everywhere. Nobody can delete,
disable or demote the account they are signed in with, and the last enabled
admin stays.

On the first start without accounts and without `FSDECK_ADMIN_PASSWORD`,
fsdeck creates `admin` with a random password and prints it once.

`FSDECK_ADMIN_PASSWORD` and `FSDECK_VIEWER_PASSWORD` keep the accounts `admin`
(role admin) and `viewer` (role viewer) with that password, whatever the data
file says. The console shows them as managed and cannot change or delete
them; unset the variable to remove the account.

## Security notes

- The `admin` role can run any FreeSWITCH API command. Treat it like shell
  access to the switch.
- Without `-tls-cert` and `-tls-key` fsdeck serves plain HTTP. Then keep it on
  a trusted network or behind a TLS-terminating reverse proxy that sets
  `X-Forwarded-Proto: https`, so the session cookie is marked `Secure`. With
  built-in TLS fsdeck also sends `Strict-Transport-Security`.
- ESL passwords are stored in clear in the data file unless
  `FSDECK_SECRET_KEY` is set; fsdeck warns at start when it is not. With the
  key they are encrypted with AES-256-GCM; existing clear values are encrypted
  at the next change to the data file. fsdeck refuses to start when the file
  holds encrypted values and the key is missing or wrong. Keep the key outside
  the data directory, and protect the data file either way: it also holds the
  password hashes.
- Five failed sign-ins for an account from one address within five minutes
  block that pair for a minute. Behind a reverse proxy every client shares the
  proxy's address, so the block then applies to the account as a whole.
- The audit log records sign-ins (also failed and blocked ones), sign-outs,
  instance and account changes, password changes, hang-ups, actions, refused
  requests and every console command in full, with the account and client
  address. It never records passwords or session tokens, but console commands
  can carry secrets: the file has mode 0600 and only admins can read it in
  the console. At 10 MiB it is renamed to `<name>.1` and a new file starts.
- Log lines are not masked and can contain secrets; that is why logs need the
  `operator` or `admin` role.

## Not in scope (yet)

- Editing configuration files. The Event Socket has no file API; the plan is
  an optional agent on the FreeSWITCH host.
- SIP capture and metrics history. Homer and Prometheus cover those.

## Trying it against a stock FreeSWITCH in Docker

Vanilla FreeSWITCH needs three changes to be reachable from the Docker host:
listen on IPv4, an ACL that allows the host, and no STUN lookup (it fails
without outbound UDP and keeps `mod_sofia` from loading).

```sh
docker run -d --name dev-fsdeck-freeswitch -p 127.0.0.1:18021:8021 \
  --entrypoint sh safarov/freeswitch:1.11.3 -c '
set -e
C=/etc/freeswitch
mkdir -p $C && cp -a /usr/share/freeswitch/conf/vanilla/. $C/
sed -i -e "s|name=\"listen-ip\" value=\"::\"|name=\"listen-ip\" value=\"0.0.0.0\"|" \
  -e "s|<!--<param name=\"apply-inbound-acl\" value=\"loopback.auto\"/>-->|<param name=\"apply-inbound-acl\" value=\"fsdeck\"/>|" \
  $C/autoload_configs/event_socket.conf.xml
sed -i -e "s|<network-lists>|<network-lists>\n<list name=\"fsdeck\" default=\"deny\">\n<node type=\"allow\" cidr=\"127.0.0.0/8\"/>\n<node type=\"allow\" cidr=\"10.0.0.0/8\"/>\n<node type=\"allow\" cidr=\"172.16.0.0/12\"/>\n<node type=\"allow\" cidr=\"192.168.0.0/16\"/>\n</list>|" \
  $C/autoload_configs/acl.conf.xml
sed -i -e "s|cmd=\"stun-set\" data=\"external_rtp_ip=stun:stun.freeswitch.org\"|cmd=\"set\" data=\"external_rtp_ip=\$\${local_ip_v4}\"|" \
  -e "s|cmd=\"stun-set\" data=\"external_sip_ip=stun:stun.freeswitch.org\"|cmd=\"set\" data=\"external_sip_ip=\$\${local_ip_v4}\"|" \
  -e "s|default_password=1234|default_password=$(tr -dc A-Za-z0-9 < /dev/urandom | head -c 20)|" $C/vars.xml
exec /usr/bin/freeswitch -nc -nf -nonat'
```

Add it in fsdeck as host `127.0.0.1`, port `18021`, password `ClueCon`. For
some traffic to look at:

```sh
docker exec dev-fsdeck-freeswitch fs_cli -x 'bgapi originate loopback/9196/default &park()'
```

The image is amd64 only; on Apple silicon add `--platform linux/amd64`.

## Development

```sh
go vet ./... && go test -race ./...
```

No third-party Go modules and no frontend build step: `web/static` is served
as is. While working on the frontend, `./fsdeck -static web/static` serves the
files from disk, so a browser reload shows the change without rebuilding.

## License

MIT. FreeSWITCH is a trademark of its owner; fsdeck is an independent project
and is not affiliated with or endorsed by the FreeSWITCH project or SignalWire.

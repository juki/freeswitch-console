# FreeSWITCH hang-up causes: what they mean and what to check

English | [简体中文](hangup-causes.zh-CN.md)

Every channel in FreeSWITCH ends with a hang-up cause such as
`NORMAL_CLEARING` or `NO_ROUTE_DESTINATION`. The name tells you a lot, once
you know three things that are easy to miss:

1. **The cause belongs to one leg.** A bridged call has two channels, and
   each ends with its own cause. Read the leg that failed.
2. **Receiving and sending use different tables.** The cause FreeSWITCH
   reports after it *received* a SIP error is not always the cause that would
   make it *send* that same status. A 404 from the far end is
   `UNALLOCATED_NUMBER`; a number your own dialplan could not route is
   `NO_ROUTE_DESTINATION`, and the caller gets a 404 for it.
3. **`NORMAL_CLEARING` can hide a failure.** When a dialplan continues after
   a failed bridge (the stock one goes on to voicemail), the leg ends
   normally. The reason the bridge failed is in a variable, not in the cause.

The tables below are taken from the FreeSWITCH source (`sofia_glue.c`,
`mod_sofia.c`, `switch_types.h`) and checked on FreeSWITCH 1.11.3.

## Where to find the cause

- **The log.** The hang-up line names the state and the cause:
  `Hangup sofia/internal/1001@pbx.example [CS_EXECUTE] [NORMAL_CLEARING]`.
- **Channel variables**, in a CDR, a `CHANNEL_HANGUP_COMPLETE` event or
  `uuid_dump`:

  | Variable | What it holds |
  |---|---|
  | `hangup_cause` | the cause of this leg |
  | `hangup_cause_q850` | its number |
  | `sip_term_status` | the SIP status that ended the leg |
  | `sip_invite_failure_status` | the SIP status that failed an outbound INVITE |
  | `proto_specific_hangup_cause` | the same with its protocol, e.g. `sip:486` |
  | `last_bridge_hangup_cause` | why the other leg of the last bridge ended |
  | `originate_failed_cause` | why the last bridge or originate attempt failed |
  | `sip_hangup_disposition` | who ended the leg, see below |

## Who hung up

`sip_hangup_disposition` answers the question the cause cannot:

| Value | Meaning |
|---|---|
| `recv_bye` | the far end of this leg sent BYE: that side hung up |
| `send_bye` | FreeSWITCH sent BYE, usually because the other leg ended |
| `recv_cancel` | the caller cancelled before the call was answered |
| `send_cancel` | FreeSWITCH cancelled its outbound INVITE (timeout, or another leg won) |
| `recv_refuse` | the far end answered the INVITE with an error |
| `send_refuse` | FreeSWITCH answered an inbound INVITE with an error |

## A SIP status arrived: which cause do I get?

| SIP status received | Cause |
|---|---|
| 200 | `NORMAL_CLEARING` |
| 401, 402, 403, 407, 603, 608 | `CALL_REJECTED` |
| 404 | `UNALLOCATED_NUMBER` |
| 485, 604 | `NO_ROUTE_DESTINATION` |
| 408, 504 | `RECOVERY_ON_TIMER_EXPIRE` |
| 410 | `NUMBER_CHANGED` |
| 480 | `NO_USER_RESPONSE` |
| 400, 481, 500, 503 | `NORMAL_TEMPORARY_FAILURE` |
| 486, 600 | `USER_BUSY` |
| 484 | `INVALID_NUMBER_FORMAT` |
| 488, 606 | `INCOMPATIBLE_DESTINATION` |
| 502 | `NETWORK_OUT_OF_ORDER` |
| 482, 483 | `EXCHANGE_ROUTING_ERROR` |
| 487 | `ORIGINATOR_CANCEL` |
| 405 | `SERVICE_UNAVAILABLE` |
| 406, 415, 501 | `SERVICE_NOT_IMPLEMENTED` |
| 413, 414, 416, 420, 421, 423, 505, 513 | `INTERWORKING` |
| anything else | `NORMAL_UNSPECIFIED` |

## A leg ends with a cause: which SIP status does the caller get?

| Cause | SIP status sent |
|---|---|
| `NO_ROUTE_DESTINATION`, `UNALLOCATED_NUMBER` | 404 |
| `USER_BUSY` | 486 |
| `NO_USER_RESPONSE` | 408 |
| `NO_ANSWER`, `SUBSCRIBER_ABSENT`, `NORMAL_UNSPECIFIED` | 480 |
| `CALL_REJECTED` | 603 |
| `DESTINATION_OUT_OF_ORDER`, `INVALID_PROFILE` | 502 |
| `INVALID_NUMBER_FORMAT`, `INVALID_GATEWAY` | 484 |
| `NORMAL_TEMPORARY_FAILURE`, `NORMAL_CIRCUIT_CONGESTION`, `SWITCH_CONGESTION`, `GATEWAY_DOWN`, `NETWORK_OUT_OF_ORDER` | 503 |
| `INCOMPATIBLE_DESTINATION` | 488 |
| `RECOVERY_ON_TIMER_EXPIRE` | 504 |
| `ORIGINATOR_CANCEL` | 487 |
| `EXCHANGE_ROUTING_ERROR` | 483 |
| any cause not mapped | 480 |

## The causes, one by one

The number in brackets is `hangup_cause_q850`.

### The call ended normally

**`NORMAL_CLEARING` (16).** One side hung up. `sip_hangup_disposition` says
which. It is also what `uuid_kill` gives when no cause is named. If the
caller complains anyway, look at `originate_failed_cause` and
`last_bridge_hangup_cause`: a bridge may have failed before the leg went on
to voicemail or an announcement.

**`ATTENDED_TRANSFER` (801), `BLIND_TRANSFER` (800).** The leg ended because
the call was transferred. Nothing to fix.

**`LOSE_RACE` (702).** The leg was one of several rung at once and another
one answered first. Expected for every ring group.

**`PICKED_OFF` (805).** Another extension picked the call up.

### Nobody took the call

**`ORIGINATOR_CANCEL` (487).** The caller hung up while it was ringing.
Many of these right after dialling point at post-dial delay: check how long
the far end takes to answer the INVITE.

**`NO_ANSWER` (19).** The timeout ran out before an answer: `call_timeout`
or `leg_timeout` on a bridge, `originate_timeout` on an originate.
FreeSWITCH then cancels the INVITE (`send_cancel`). If the far end never
rang at all, it is unreachable rather than unattended.

**`NO_USER_RESPONSE` (18).** The far end answered 480, temporarily
unavailable: the device behind it is offline, not registered there, or in
do-not-disturb.

**`USER_BUSY` (17).** The far end answered 486 or 600.

**`CALL_REJECTED` (21).** The far end, or something in front of it, refused
with 401, 403, 407 or 603. Usual reasons: an ACL, failed authentication on
a gateway, a blocked caller id, or the user pressed reject. The SIP status
narrows it down; a SIP trace shows who sent it.

**`ALLOTTED_TIMEOUT` (802).** A time limit ended the call: `sched_hangup`
or a limit set on the bridge.

**`PROGRESS_TIMEOUT` (807).** No ringing or early media arrived within
`progress_timeout`.

### The number could not be routed

**`NO_ROUTE_DESTINATION` (3).** On an inbound leg: no dialplan extension
matched the dialled number in the context the call arrived in. Check the
context (`Caller-Context` in the log) and the regular expressions of its
extensions; setting the log level to debug shows each condition being
tried. On an outbound leg: the far end answered 485 or 604.

**`UNALLOCATED_NUMBER` (1).** The far end answered 404: the number does not
exist there. Check the number format the carrier expects (prefix, `+`,
country code).

**`INVALID_NUMBER_FORMAT` (28).** The far end answered 484: the number is
incomplete or malformed for it.

**`USER_NOT_REGISTERED` (806).** The dial string asked for a registered
user (`user/1001` or `sofia/internal/1001%domain`) and there is no
registration. `sofia status profile internal reg` lists who is registered.
A phone that registers and drops again usually has a NAT or expiry problem.

**`SUBSCRIBER_ABSENT` (20).** The user exists but cannot be reached now.

**`INVALID_GATEWAY` (808).** The dial string names a gateway that is not
loaded. `sofia status gateway` lists them; after adding one, run
`sofia profile external rescan`.

**`GATEWAY_DOWN` (809).** The gateway is marked down because its OPTIONS
pings fail, so the call was not even tried. `sofia status gateway <name>`
shows the ping state.

**`EXCHANGE_ROUTING_ERROR` (25).** The far end answered 482 or 483: a loop,
or too many hops. Look for a route that sends the call back where it came
from.

### The far end or the network failed

**`NORMAL_TEMPORARY_FAILURE` (41).** The far end answered 400, 481, 500 or
503. A carrier out of capacity, an overloaded proxy, or a request it could
not parse. If only one gateway does it, it is theirs.

**`RECOVERY_ON_TIMER_EXPIRE` (102).** A SIP timer ran out: 408 or 504, or
no reply to the INVITE at all. The packets are not arriving or the answers
are not coming back. Check the address and port, the firewall, and whether
the far end is up.

**`NETWORK_OUT_OF_ORDER` (38).** The far end answered 502: a gateway behind
it failed.

**`DESTINATION_OUT_OF_ORDER` (27).** FreeSWITCH could not set the leg up
at all: a profile in the dial string that does not exist or is not running,
media that could not be negotiated, or a transport that failed. The caller
gets a 502.

**`NORMAL_CIRCUIT_CONGESTION` (34), `SWITCH_CONGESTION` (42).** No capacity.
The caller gets a 503. If it is this switch that refuses calls, `status`
shows its session limit and its sessions-per-second limit next to the
current values.

### Audio

**`INCOMPATIBLE_DESTINATION` (88).** The two sides have no codec in common
(488 or 606). Compare `inbound-codec-prefs` / `outbound-codec-prefs` of the
profile with what the far end offers in its SDP.

**`MEDIA_TIMEOUT` (804).** No RTP arrived for `rtp-timeout-sec` (or the
`media_timeout` variable), so FreeSWITCH ended the call. Typical for NAT
problems, a phone that lost its network, or a far end that stopped sending
without signalling. One-way audio before the drop points at NAT.

### The switch ended it

**`MANAGER_REQUEST` (703).** Someone ran `hupall` without naming a cause.
(`uuid_kill` without a cause ends the channel with `NORMAL_CLEARING`.)

**`SYSTEM_SHUTDOWN` (701).** FreeSWITCH was shutting down.

**`NORMAL_UNSPECIFIED` (31).** No specific reason: an application or script
hung up without naming a cause, or a SIP status arrived that has no mapping.

## Commands that help

```
sofia status profile internal reg     who is registered
sofia status gateway <name>           gateway state and ping
sofia profile <name> siptrace on      print every SIP message of a profile
uuid_dump <uuid>                      all variables of a live channel
fsctl loglevel debug                  see dialplan conditions being tried
status                                session and rate limits
```

Turn the SIP trace and the debug level off again when you are done: both
produce a lot of output on a busy switch.

## Seeing all of this at once

[FreeSWITCH Console](https://github.com/juki/freeswitch-console) keeps the
last hang-ups of a switch with the cause explained, the SIP status, who
hung up, the reason of a failed bridge and the call's own log lines. There
is a [demo](https://fsconsole.vi.uy) with simulated switches.

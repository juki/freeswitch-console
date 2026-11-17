# FreeSWITCH 挂断原因对照：各自意味着什么，先查哪里

[English](hangup-causes.md) | 简体中文

FreeSWITCH 里每个通道结束时都带一个挂断原因，比如 `NORMAL_CLEARING`、`NO_ROUTE_DESTINATION`。名字本身信息量很大，但有三点容易被忽略：

1. **原因是按“腿”算的。** 一通桥接的电话有两个通道，各有各的挂断原因。要看出问题的那条腿。
2. **“收到”和“发出”用的是两张表。** FreeSWITCH *收到*某个 SIP 错误后报出的原因，不一定等于让它*发出*同一个状态码的那个原因。对端回 404 时报的是 `UNALLOCATED_NUMBER`；而本机拨号计划路由不了的号码是 `NO_ROUTE_DESTINATION`，主叫收到的也是 404。
3. **`NORMAL_CLEARING` 可能掩盖了失败。** 如果拨号计划在桥接失败后继续往下走（原版配置会转语音信箱），这条腿最后是“正常挂断”。桥接为什么失败，写在变量里，不在挂断原因里。

下面的对照表取自 FreeSWITCH 源码（`sofia_glue.c`、`mod_sofia.c`、`switch_types.h`），并在 FreeSWITCH 1.11.3 上核对过。

## 去哪里找挂断原因

- **日志。** 挂断那一行会写出状态和原因：
  `Hangup sofia/internal/1001@pbx.example [CS_EXECUTE] [NORMAL_CLEARING]`。
- **通道变量**，在话单、`CHANNEL_HANGUP_COMPLETE` 事件或 `uuid_dump` 里都能看到：

  | 变量 | 含义 |
  |---|---|
  | `hangup_cause` | 这条腿的挂断原因 |
  | `hangup_cause_q850` | 对应的编号 |
  | `sip_term_status` | 结束这条腿的 SIP 状态码 |
  | `sip_invite_failure_status` | 让外呼 INVITE 失败的 SIP 状态码 |
  | `proto_specific_hangup_cause` | 同上，带协议前缀，例如 `sip:486` |
  | `last_bridge_hangup_cause` | 上一次桥接里另一条腿为什么结束 |
  | `originate_failed_cause` | 上一次桥接或外呼尝试为什么失败 |
  | `sip_hangup_disposition` | 是谁结束了这条腿，见下文 |

## 是谁挂的

挂断原因回答不了的问题，`sip_hangup_disposition` 能回答：

| 取值 | 含义 |
|---|---|
| `recv_bye` | 这条腿的对端发了 BYE，也就是对方挂的 |
| `send_bye` | FreeSWITCH 发了 BYE，通常是因为另一条腿结束了 |
| `recv_cancel` | 主叫在接通前取消了呼叫 |
| `send_cancel` | FreeSWITCH 取消了自己发出的 INVITE（超时，或者别的腿先接了） |
| `recv_refuse` | 对端用错误码回应了 INVITE |
| `send_refuse` | FreeSWITCH 用错误码回应了呼入的 INVITE |

## 收到某个 SIP 状态码，会报什么原因

| 收到的 SIP 状态码 | 挂断原因 |
|---|---|
| 200 | `NORMAL_CLEARING` |
| 401、402、403、407、603、608 | `CALL_REJECTED` |
| 404 | `UNALLOCATED_NUMBER` |
| 485、604 | `NO_ROUTE_DESTINATION` |
| 408、504 | `RECOVERY_ON_TIMER_EXPIRE` |
| 410 | `NUMBER_CHANGED` |
| 480 | `NO_USER_RESPONSE` |
| 400、481、500、503 | `NORMAL_TEMPORARY_FAILURE` |
| 486、600 | `USER_BUSY` |
| 484 | `INVALID_NUMBER_FORMAT` |
| 488、606 | `INCOMPATIBLE_DESTINATION` |
| 502 | `NETWORK_OUT_OF_ORDER` |
| 482、483 | `EXCHANGE_ROUTING_ERROR` |
| 487 | `ORIGINATOR_CANCEL` |
| 405 | `SERVICE_UNAVAILABLE` |
| 406、415、501 | `SERVICE_NOT_IMPLEMENTED` |
| 413、414、416、420、421、423、505、513 | `INTERWORKING` |
| 其他 | `NORMAL_UNSPECIFIED` |

## 一条腿以某个原因结束，主叫会收到什么状态码

| 挂断原因 | 发出的 SIP 状态码 |
|---|---|
| `NO_ROUTE_DESTINATION`、`UNALLOCATED_NUMBER` | 404 |
| `USER_BUSY` | 486 |
| `NO_USER_RESPONSE` | 408 |
| `NO_ANSWER`、`SUBSCRIBER_ABSENT`、`NORMAL_UNSPECIFIED` | 480 |
| `CALL_REJECTED` | 603 |
| `DESTINATION_OUT_OF_ORDER`、`INVALID_PROFILE` | 502 |
| `INVALID_NUMBER_FORMAT`、`INVALID_GATEWAY` | 484 |
| `NORMAL_TEMPORARY_FAILURE`、`NORMAL_CIRCUIT_CONGESTION`、`SWITCH_CONGESTION`、`GATEWAY_DOWN`、`NETWORK_OUT_OF_ORDER` | 503 |
| `INCOMPATIBLE_DESTINATION` | 488 |
| `RECOVERY_ON_TIMER_EXPIRE` | 504 |
| `ORIGINATOR_CANCEL` | 487 |
| `EXCHANGE_ROUTING_ERROR` | 483 |
| 没有对应关系的原因 | 480 |

## 逐个说明

括号里的数字是 `hangup_cause_q850`。

### 正常结束

**`NORMAL_CLEARING`（16）。** 其中一方挂断，`sip_hangup_disposition` 会说明是哪一方。`uuid_kill` 不带原因时给出的也是它。如果主叫仍然投诉，去看 `originate_failed_cause` 和 `last_bridge_hangup_cause`：可能是桥接先失败了，这条腿随后转到了语音信箱或提示音。

**`ATTENDED_TRANSFER`（801）、`BLIND_TRANSFER`（800）。** 通话被转接，这条腿随之结束，不需要处理。

**`LOSE_RACE`（702）。** 这条腿是同时振铃的多路之一，别的那路先接了。振铃组里每次都会出现，属于正常。

**`PICKED_OFF`（805）。** 通话被别的分机代接。

### 没人接

**`ORIGINATOR_CANCEL`（487）。** 主叫在振铃时挂断。如果很多呼叫都是刚拨出就取消，多半是接续时延太长：看对端回应 INVITE 用了多久。

**`NO_ANSWER`（19）。** 超时前没人接：桥接上的 `call_timeout` 或 `leg_timeout`，外呼上的 `originate_timeout`。超时后 FreeSWITCH 会取消 INVITE（`send_cancel`）。如果对端根本没振铃，那是对端不可达，而不是没人接。

**`NO_USER_RESPONSE`（18）。** 对端回了 480（暂时不可用）：它后面的设备离线、没有在那边注册，或者开了免打扰。

**`USER_BUSY`（17）。** 对端回了 486 或 600。

**`CALL_REJECTED`（21）。** 对端或它前面的某个系统用 401、403、407 或 603 拒绝了呼叫。常见原因：ACL、网关鉴权失败、主叫号码被拦截，或者用户按了拒接。SIP 状态码能缩小范围，开 SIP 跟踪能看出是谁发的。

**`ALLOTTED_TIMEOUT`（802）。** 被时限结束：`sched_hangup`，或桥接上设置的时长限制。

**`PROGRESS_TIMEOUT`（807）。** 在 `progress_timeout` 时间内既没有振铃也没有早期媒体。

### 号码路由不出去

**`NO_ROUTE_DESTINATION`（3）。** 在呼入的那条腿上：呼叫进入的 context 里没有任何拨号计划条目匹配所拨号码。检查 context（日志里的 `Caller-Context`）以及其中各条目的正则；把日志级别调到 debug 可以看到每个条件的匹配过程。在呼出的那条腿上：对端回了 485 或 604。

**`UNALLOCATED_NUMBER`（1）。** 对端回了 404：那边不存在这个号码。检查运营商要求的号码格式（前缀、`+`、国家码）。

**`INVALID_NUMBER_FORMAT`（28）。** 对端回了 484：号码不完整，或格式不被对端接受。

**`USER_NOT_REGISTERED`（806）。** 拨号串要找的是已注册用户（`user/1001` 或 `sofia/internal/1001%domain`），但没有注册记录。`sofia status profile internal reg` 可以列出当前注册的用户。话机注册后又掉线，通常是 NAT 或注册有效期的问题。

**`SUBSCRIBER_ABSENT`（20）。** 用户存在，但此刻无法到达。

**`INVALID_GATEWAY`（808）。** 拨号串里的网关没有加载。`sofia status gateway` 列出已有网关；新增网关后执行 `sofia profile external rescan`。

**`GATEWAY_DOWN`（809）。** 网关因为 OPTIONS 探测失败被标记为不可用，所以这通呼叫根本没有发出去。`sofia status gateway <名称>` 能看到探测状态。

**`EXCHANGE_ROUTING_ERROR`（25）。** 对端回了 482 或 483：出现环路，或跳数过多。检查是否有路由把呼叫又送回了来的地方。

### 对端或网络出了问题

**`NORMAL_TEMPORARY_FAILURE`（41）。** 对端回了 400、481、500 或 503。运营商容量不足、代理过载，或者对端无法解析这个请求。如果只有某一个网关这样，问题在对方。

**`RECOVERY_ON_TIMER_EXPIRE`（102）。** SIP 定时器超时：408 或 504，或者 INVITE 完全没有回应。包没有到对端，或者回应回不来。检查地址和端口、防火墙，以及对端是否在线。

**`NETWORK_OUT_OF_ORDER`（38）。** 对端回了 502：它后面的网关出了故障。

**`DESTINATION_OUT_OF_ORDER`（27）。** FreeSWITCH 根本没能把这条腿建立起来：拨号串里的 Profile 不存在或没在运行、媒体协商不成，或者传输层失败。主叫收到 502。

**`NORMAL_CIRCUIT_CONGESTION`（34）、`SWITCH_CONGESTION`（42）。** 容量不足，主叫收到 503。如果是本机在拒绝呼叫，`status` 会显示会话数上限、每秒会话数上限以及当前值。

### 媒体

**`INCOMPATIBLE_DESTINATION`（88）。** 双方没有共同支持的编解码（488 或 606）。对比 Profile 的 `inbound-codec-prefs` / `outbound-codec-prefs` 和对端 SDP 里提供的编解码。

**`MEDIA_TIMEOUT`（804）。** 在 `rtp-timeout-sec`（或变量 `media_timeout`）设定的时间内没有收到 RTP，FreeSWITCH 结束了通话。常见于 NAT 问题、话机断网，或者对端不发媒体也不发信令。如果断之前就是单通，多半是 NAT。

### 交换机自己结束的

**`MANAGER_REQUEST`（703）。** 有人执行了不带原因的 `hupall`。（`uuid_kill` 不带原因时，通道以 `NORMAL_CLEARING` 结束。）

**`SYSTEM_SHUTDOWN`（701）。** FreeSWITCH 正在关闭。

**`NORMAL_UNSPECIFIED`（31）。** 没有具体原因：某个应用或脚本挂断时没有指定原因，或者收到了一个没有对应关系的 SIP 状态码。

## 用得上的命令

```
sofia status profile internal reg     谁注册着
sofia status gateway <名称>           网关状态和探测结果
sofia profile <名称> siptrace on      打印某个 Profile 的全部 SIP 报文
uuid_dump <uuid>                      一个在线通道的全部变量
fsctl loglevel debug                  查看拨号计划条件的匹配过程
status                                会话数和速率上限
```

排查完记得把 SIP 跟踪和 debug 日志级别关掉：在繁忙的交换机上这两样的输出量都很大。

## 想一眼看全这些信息

[FreeSWITCH Console](https://github.com/juki/freeswitch-console) 会保留一台交换机最近的挂断记录，附带原因解释、SIP 状态码、是谁挂的、桥接失败的原因，以及这通电话自己的日志行。这里有一个用模拟交换机的[在线演示](https://fsconsole.vi.uy)。

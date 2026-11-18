package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/juki/freeswitch-console/internal/store"
)

// Webhook notifications. Every enabled target has a worker goroutine and a
// queue of its own, so a slow or dead endpoint holds up nothing but its own
// messages: the evaluator only puts them in the queues, dropping the oldest
// of a full one. The evaluator starts, updates and stops the workers at each
// tick from the saved settings (syncTargets). A target that is removed or
// switched off has its worker stopped: the request in flight is cancelled
// and the queued messages are not sent. A target that is changed keeps its
// worker and queue; the request in flight finishes with what it started
// with, and every later attempt uses the new settings. A webhook URL is a
// secret: only its host appears in an answer, a log line or the audit log.

const (
	targetQueue   = 100
	hookTimeout   = 5 * time.Second
	hookAnswerMax = 4 << 10 // bytes of an answer read
	hookDetailMax = 300     // characters of an answer or error shown
	alertTextMax  = 200     // characters of a name or reason in a message
)

// hookRetry are the waits before the second and third attempt.
var hookRetry = []time.Duration{2 * time.Second, 10 * time.Second}

// notice is one message for the targets.
type notice struct {
	entry *alertEntry // where the outcomes are recorded; nil for a test
	data  alertEntry  // what the message says
}

// target is the worker of one enabled target.
type target struct {
	queue  chan notice
	ctx    context.Context // cancelled when the target is removed or switched off, or the server closes
	cancel context.CancelFunc

	mu  sync.Mutex
	cfg store.AlertTarget
}

func (t *target) config() store.AlertTarget {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.cfg
}

// syncTargets runs a worker for every enabled target of list and stops the
// others. Demo mode sends nothing.
func (s *Server) syncTargets(list []store.AlertTarget) {
	s.notifyMu.Lock()
	defer s.notifyMu.Unlock()
	if s.notifyClosed {
		return
	}
	keep := map[string]bool{}
	for _, cfg := range list {
		if !cfg.Enabled || s.cfg.Demo {
			continue
		}
		keep[cfg.ID] = true
		if t := s.targets[cfg.ID]; t != nil {
			t.mu.Lock()
			t.cfg = cfg
			t.mu.Unlock()
			continue
		}
		ctx, cancel := context.WithCancel(context.Background())
		t := &target{queue: make(chan notice, s.queueSize), ctx: ctx, cancel: cancel, cfg: cfg}
		s.targets[cfg.ID] = t
		s.workers.Add(1)
		go s.runTarget(t)
	}
	for id, t := range s.targets {
		if !keep[id] {
			t.cancel()
			delete(s.targets, id)
		}
	}
}

// stopTargets stops every worker and waits for them.
func (s *Server) stopTargets() {
	s.notifyMu.Lock()
	s.notifyClosed = true
	for id, t := range s.targets {
		t.cancel()
		delete(s.targets, id)
	}
	s.notifyMu.Unlock()
	s.workers.Wait()
}

// enqueue puts every notice in the queue of every worker. A full queue loses
// its oldest message, which is recorded and logged.
func (s *Server) enqueue(list []notice) {
	if len(list) == 0 {
		return
	}
	type drop struct {
		n   notice
		cfg store.AlertTarget
	}
	var dropped []drop
	s.notifyMu.Lock()
	for _, t := range s.targets {
		for _, n := range list {
			// Only the evaluator puts messages in, so this ends.
			for queued := false; !queued; {
				select {
				case t.queue <- n:
					queued = true
				default:
					select {
					case old := <-t.queue:
						dropped = append(dropped, drop{old, t.config()})
					default:
					}
				}
			}
		}
	}
	s.notifyMu.Unlock()
	for _, d := range dropped {
		log.Printf("alerts: the queue of target %q (%s) is full; dropped its oldest message", d.cfg.Name, store.URLHost(d.cfg.URL))
		s.recordDelivery(d.n.entry, alertDelivery{Target: d.cfg.Name, Error: "not sent: the queue of the target was full"})
	}
}

// runTarget sends the messages of t one by one until it is stopped; what is
// still queued then is recorded as not sent.
func (s *Server) runTarget(t *target) {
	defer s.workers.Done()
	for {
		select {
		case <-t.ctx.Done():
			for {
				select {
				case n := <-t.queue:
					s.recordDelivery(n.entry, alertDelivery{Target: t.config().Name, Error: errTargetStopped})
				default:
					return
				}
			}
		case n := <-t.queue:
			s.deliver(t, n)
		}
	}
}

const errTargetStopped = "not sent: the target was removed or switched off"

// deliver sends n to t: up to 1+len(hookRetry) attempts for an error that
// may pass (no answer, 5xx, 429).
func (s *Server) deliver(t *target, n notice) {
	var res hookResult
	for attempt := 0; ; attempt++ {
		res = s.post(t.ctx, t.config(), n.data)
		if res.ok || !res.retry || attempt == len(s.hookRetry) {
			break
		}
		wait := time.NewTimer(s.hookRetry[attempt])
		select {
		case <-t.ctx.Done():
			wait.Stop()
			res.why = errTargetStopped
		case <-wait.C:
			continue
		}
		break
	}
	cfg := t.config()
	if !res.ok {
		log.Printf("alerts: sending to target %q (%s) failed: %s", cfg.Name, store.URLHost(cfg.URL), res.why)
	}
	s.recordDelivery(n.entry, alertDelivery{Target: cfg.Name, OK: res.ok, Status: res.status, Error: res.why})
}

// hookResult is the outcome of one attempt.
type hookResult struct {
	ok     bool
	status int    // 0 without an answer
	detail string // the answer, or the error
	why    string // why it failed; "" when ok
	retry  bool   // the error may pass
}

// post sends e to t once, within s.hookTimeout. Redirects are not followed.
// Success is a 2xx answer that is not a JSON object with a non-zero
// errcode, code or StatusCode (DingTalk, WeCom and Feishu answer 200 with an
// error code).
func (s *Server) post(ctx context.Context, t store.AlertTarget, e alertEntry) hookResult {
	ctx, cancel := context.WithTimeout(ctx, s.hookTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.URL, bytes.NewReader(hookBody(t.Format, t.Language, s.cfg.Version, e)))
	if err != nil {
		// The error would quote the URL.
		return hookResult{detail: "invalid url", why: "invalid url"}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "fsconsole/"+s.cfg.Version)
	resp, err := s.hookClient.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // the URL is in the wrapper only
		}
		why := err.Error()
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			why = fmt.Sprintf("no answer within %v", s.hookTimeout)
		case errors.Is(err, context.Canceled):
			why = errTargetStopped
		}
		why = cutText(redact(why, t.URL), hookDetailMax)
		return hookResult{detail: why, why: why, retry: !errors.Is(err, context.Canceled)}
	}
	defer resp.Body.Close()
	answer, _ := io.ReadAll(io.LimitReader(resp.Body, hookAnswerMax))
	res := hookResult{status: resp.StatusCode, detail: cutText(redact(strings.TrimSpace(string(answer)), t.URL), hookDetailMax)}
	why := ""
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		why = "HTTP " + strconv.Itoa(resp.StatusCode)
		res.retry = resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests
	} else if key, code := answerCode(answer); code != 0 {
		why = fmt.Sprintf("%s %v", key, code)
	}
	if why == "" {
		res.ok = true
		return res
	}
	if res.detail != "" {
		why += ": " + res.detail
	}
	res.why = cutText(why, hookDetailMax)
	return res
}

// answerCode returns the first non-zero numeric errcode, code or StatusCode
// of an answer that is a JSON object.
func answerCode(answer []byte) (string, float64) {
	var obj map[string]any
	if json.Unmarshal(answer, &obj) != nil {
		return "", 0
	}
	for _, k := range []string{"errcode", "code", "StatusCode"} {
		if n, ok := obj[k].(float64); ok && n != 0 {
			return k, n
		}
	}
	return "", 0
}

// redact hides in text the path segments and query values of raw that may
// be a token, in case an endpoint quotes its URL.
func redact(text, raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return text
	}
	var secrets []string
	for _, p := range strings.Split(u.Path, "/") {
		secrets = append(secrets, p)
	}
	for _, vs := range u.Query() {
		secrets = append(secrets, vs...)
	}
	secrets = append(secrets, u.EscapedPath(), u.RawQuery)
	for _, v := range secrets {
		if len(v) >= 8 {
			text = strings.ReplaceAll(text, v, "…")
		}
	}
	return text
}

// cutText returns at most the first n characters of v.
func cutText(v string, n int) string {
	for i := range v {
		if n == 0 {
			return v[:i]
		}
		n--
	}
	return v
}

var slackEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

// hookBody is the request body of e in the given format and language.
func hookBody(format, language, version string, e alertEntry) []byte {
	text := alertSummary(language, e)
	var v any
	switch format {
	case "slack":
		v = map[string]string{"text": slackEscaper.Replace(text)}
	case "dingtalk", "wecom":
		v = struct {
			MsgType string            `json:"msgtype"`
			Text    map[string]string `json:"text"`
		}{"text", map[string]string{"content": text}}
	case "feishu":
		v = struct {
			MsgType string            `json:"msg_type"`
			Content map[string]string `json:"content"`
		}{"text", map[string]string{"text": text}}
	default:
		type ref struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		v = struct {
			Source          string  `json:"source"`
			Version         string  `json:"version"`
			Event           string  `json:"event"`
			Rule            string  `json:"rule"`
			Instance        ref     `json:"instance"`
			Subject         string  `json:"subject"`
			Summary         string  `json:"summary"`
			State           string  `json:"state"`
			Ping            string  `json:"ping"`
			Reason          string  `json:"reason"`
			Value           float64 `json:"value"`
			Threshold       int     `json:"threshold"`
			Sessions        int     `json:"sessions"`
			Limit           int     `json:"limit"`
			Since           string  `json:"since"`
			At              string  `json:"at"`
			DurationSeconds int64   `json:"durationSeconds"`
		}{"fsconsole", version, e.Event, e.Rule, ref{e.InstanceID, e.Instance}, e.Subject, text, e.State, e.Ping, e.Reason,
			e.Value, e.Threshold, e.Sessions, e.Limit,
			time.UnixMilli(e.Since).Format(time.RFC3339), time.UnixMilli(e.T).Format(time.RFC3339), e.DurationMs / 1000}
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
	return bytes.TrimSpace(b.Bytes())
}

// alertSummary is the text of e in language (en or zh-CN). It starts with
// "[FreeSWITCH Console] " so a keyword rule of a chat robot matches.
func alertSummary(language string, e alertEntry) string {
	zh := language == "zh-CN"
	pick := func(en, cn string) string {
		if zh {
			return cn
		}
		return en
	}
	inst, gw, reason := cutText(e.Instance, alertTextMax), cutText(e.Subject, alertTextMax), cutText(e.Reason, alertTextMax)
	since := time.UnixMilli(e.Since).Format("2006-01-02 15:04:05 MST")
	dur := formatDuration(time.Duration(e.DurationMs)*time.Millisecond, zh)
	value := strconv.FormatFloat(e.Value, 'f', -1, 64)
	firing := e.Event == "firing"
	var text string
	switch {
	case e.Event == "test":
		text = pick("Test notification. If you can read this, the webhook works.", "测试通知。能看到这条消息，说明 Webhook 配置正确。")
	case e.Rule == ruleInstanceDown && firing:
		en, cn := "", ""
		if reason != "" {
			en, cn = ": "+reason, "（"+reason+"）"
		}
		text = pick(fmt.Sprintf("DOWN: %s cannot be reached%s · since %s", inst, en, since),
			fmt.Sprintf("故障：无法连接 %s%s · 开始于 %s", inst, cn, since))
	case e.Rule == ruleInstanceDown:
		text = pick(fmt.Sprintf("OK: %s is connected again · was down for %s", inst, dur),
			fmt.Sprintf("恢复：%s 已重新连接 · 持续 %s", inst, dur))
	case e.Rule == ruleGatewayDown && firing:
		en, cn := "", ""
		if e.Ping == "DOWN" {
			en, cn = ", ping DOWN", "，探测失败"
		}
		text = pick(fmt.Sprintf("DOWN: gateway %s on %s is %s%s · since %s", gw, inst, e.State, en, since),
			fmt.Sprintf("故障：%s 的网关 %s 状态为 %s%s · 开始于 %s", inst, gw, e.State, cn, since))
	case e.Rule == ruleGatewayDown:
		text = pick(fmt.Sprintf("OK: gateway %s on %s is %s again · was down for %s", gw, inst, e.State, dur),
			fmt.Sprintf("恢复：%s 的网关 %s 已恢复（%s） · 持续 %s", inst, gw, e.State, dur))
	case e.Rule == ruleIdleCPULow && firing:
		text = pick(fmt.Sprintf("WARNING: %s idle CPU is %s%% (threshold %d%%) · since %s", inst, value, e.Threshold, since),
			fmt.Sprintf("警告：%s 空闲 CPU 为 %s%%（阈值 %d%%） · 开始于 %s", inst, value, e.Threshold, since))
	case e.Rule == ruleIdleCPULow:
		text = pick(fmt.Sprintf("OK: %s idle CPU is back to %s%% · lasted %s", inst, value, dur),
			fmt.Sprintf("恢复：%s 空闲 CPU 回到 %s%% · 持续 %s", inst, value, dur))
	case e.Rule == ruleSessionsHigh && firing:
		text = pick(fmt.Sprintf("WARNING: %s has %d of %d sessions (%s%%, threshold %d%%) · since %s", inst, e.Sessions, e.Limit, value, e.Threshold, since),
			fmt.Sprintf("警告：%s 会话数 %d / %d（%s%%，阈值 %d%%） · 开始于 %s", inst, e.Sessions, e.Limit, value, e.Threshold, since))
	case e.Rule == ruleSessionsHigh:
		text = pick(fmt.Sprintf("OK: %s is back to %d of %d sessions (%s%%) · lasted %s", inst, e.Sessions, e.Limit, value, dur),
			fmt.Sprintf("恢复：%s 会话数回到 %d / %d（%s%%） · 持续 %s", inst, e.Sessions, e.Limit, value, dur))
	}
	return "[FreeSWITCH Console] " + text
}

// formatDuration is like "1h 5m", "5m 12s" or "40s" ("1小时5分", "5分12秒",
// "40秒").
func formatDuration(d time.Duration, zh bool) string {
	d = d.Round(time.Second)
	h, m, sec := int(d/time.Hour), int(d/time.Minute)%60, int(d/time.Second)%60
	switch {
	case h > 0 && zh:
		return fmt.Sprintf("%d小时%d分", h, m)
	case h > 0:
		return fmt.Sprintf("%dh %dm", h, m)
	case m > 0 && zh:
		return fmt.Sprintf("%d分%d秒", m, sec)
	case m > 0:
		return fmt.Sprintf("%dm %ds", m, sec)
	case zh:
		return fmt.Sprintf("%d秒", sec)
	}
	return fmt.Sprintf("%ds", sec)
}

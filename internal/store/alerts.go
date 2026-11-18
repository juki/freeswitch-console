package store

import (
	"errors"
	"fmt"
	"net/url"
	"slices"
	"unicode/utf8"
)

// AlertRule is the settings of one alert rule: it is evaluated while
// Enabled, and its alerts change state once the condition has held for
// ForSeconds.
type AlertRule struct {
	Enabled    bool `json:"enabled"`
	ForSeconds int  `json:"forSeconds"`
}

// BelowRule fires while a value is below Below.
type BelowRule struct {
	AlertRule
	Below int `json:"below"`
}

// AboveRule fires while a value is at or above Above.
type AboveRule struct {
	AlertRule
	Above int `json:"above"`
}

// AlertRules are the four fixed rules; they apply to every instance.
type AlertRules struct {
	InstanceDown AlertRule `json:"instanceDown"`
	GatewayDown  AlertRule `json:"gatewayDown"`
	IdleCPULow   BelowRule `json:"idleCpuLow"`
	SessionsHigh AboveRule `json:"sessionsHigh"`
}

// AlertTarget is a chat webhook alerts are sent to.
type AlertTarget struct {
	ID       string `json:"id"` // 16 hex chars, generated
	Name     string `json:"name"`
	Format   string `json:"format"`   // one of AlertFormats
	Language string `json:"language"` // one of AlertLanguages
	Enabled  bool   `json:"enabled"`
	URL      string `json:"url"` // a secret; encrypted in the file when a secret key is set
}

// AlertSettings are the rules and the webhook targets.
type AlertSettings struct {
	Rules   AlertRules    `json:"rules"`
	Targets []AlertTarget `json:"targets"`
}

func (a AlertSettings) clone() AlertSettings {
	a.Targets = append([]AlertTarget{}, a.Targets...)
	return a
}

// Bounds of the alert settings.
const (
	MaxAlertTargets  = 5
	maxAlertFor      = 86400
	maxTargetName    = 40
	maxAlertURLBytes = 2048
)

var (
	AlertFormats   = []string{"json", "slack", "dingtalk", "wecom", "feishu"}
	AlertLanguages = []string{"en", "zh-CN"}

	ErrInvalidAlerts = errors.New("invalid alert settings")
)

// CheckAlerts explains why a cannot be saved, or returns nil. The error
// names a target by its name, never by its URL.
func CheckAlerts(a AlertSettings) error {
	r := a.Rules
	for _, rule := range []struct {
		key string
		AlertRule
	}{{"instanceDown", r.InstanceDown}, {"gatewayDown", r.GatewayDown}, {"idleCpuLow", r.IdleCPULow.AlertRule}, {"sessionsHigh", r.SessionsHigh.AlertRule}} {
		if rule.ForSeconds < 0 || rule.ForSeconds > maxAlertFor {
			return fmt.Errorf("%w: forSeconds of %s must be 0 to 86400", ErrInvalidAlerts, rule.key)
		}
	}
	if r.IdleCPULow.Below < 1 || r.IdleCPULow.Below > 99 {
		return fmt.Errorf("%w: below of idleCpuLow must be 1 to 99", ErrInvalidAlerts)
	}
	if r.SessionsHigh.Above < 1 || r.SessionsHigh.Above > 100 {
		return fmt.Errorf("%w: above of sessionsHigh must be 1 to 100", ErrInvalidAlerts)
	}
	if len(a.Targets) > MaxAlertTargets {
		return fmt.Errorf("%w: at most %d targets", ErrInvalidAlerts, MaxAlertTargets)
	}
	for i, t := range a.Targets {
		if n := utf8.RuneCountInString(t.Name); n < 1 || n > maxTargetName {
			return fmt.Errorf("%w: the name of target %d must be 1 to %d characters", ErrInvalidAlerts, i+1, maxTargetName)
		}
		for _, o := range a.Targets[:i] {
			if o.Name == t.Name {
				return fmt.Errorf("%w: two targets are named %q", ErrInvalidAlerts, t.Name)
			}
		}
		if !slices.Contains(AlertFormats, t.Format) {
			return fmt.Errorf("%w: the format of target %q must be json, slack, dingtalk, wecom or feishu", ErrInvalidAlerts, t.Name)
		}
		if !slices.Contains(AlertLanguages, t.Language) {
			return fmt.Errorf("%w: the language of target %q must be en or zh-CN", ErrInvalidAlerts, t.Name)
		}
		if URLHost(t.URL) == "" {
			return fmt.Errorf("%w: the url of target %q must be an absolute http or https URL with a host, at most %d characters", ErrInvalidAlerts, t.Name, maxAlertURLBytes)
		}
	}
	return nil
}

// URLHost returns the host, with its port, of a valid webhook URL, and ""
// for anything else.
func URLHost(raw string) string {
	if len(raw) > maxAlertURLBytes {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return ""
	}
	return u.Host
}

// Alerts returns a copy of the alert settings, and false when none were
// saved.
func (s *Store) Alerts() (AlertSettings, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.alerts == nil {
		return AlertSettings{}, false
	}
	return s.alerts.clone(), true
}

// SetAlerts checks a, gives every target without an ID a new one, saves it
// and returns what was saved.
func (s *Store) SetAlerts(a AlertSettings) (AlertSettings, error) {
	a = a.clone()
	if err := CheckAlerts(a); err != nil {
		return AlertSettings{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range a.Targets {
		for a.Targets[i].ID == "" {
			id, err := newID()
			if err != nil {
				return AlertSettings{}, err
			}
			if !slices.ContainsFunc(a.Targets, func(t AlertTarget) bool { return t.ID == id }) {
				a.Targets[i].ID = id
			}
		}
	}
	if err := s.save(s.instances, s.users, &a); err != nil {
		return AlertSettings{}, err
	}
	s.alerts = &a
	return a.clone(), nil
}

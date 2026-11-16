package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/juki/freeswitch-console/internal/audit"
	"github.com/juki/freeswitch-console/internal/esl"
	"github.com/juki/freeswitch-console/internal/fsapi"
)

// actionRequest is the body of POST .../actions; each action reads the
// fields it needs.
type actionRequest struct {
	Action  string    `json:"action"`
	Op      string    `json:"op"`
	Profile string    `json:"profile"`
	Gateway string    `json:"gateway"`
	Module  string    `json:"module"`
	Name    string    `json:"name"`
	Value   textValue `json:"value"`
	On      *bool     `json:"on"`
}

// textValue takes a JSON string or number, so {"value":7} and {"value":"7"}
// both work.
type textValue string

func (v *textValue) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*v = textValue(s)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return errors.New("value must be a string or a number")
	}
	*v = textValue(n)
	return nil
}

// actionCaps says who may run each operation: action, then op ("" for the
// actions without one), then the capability it needs.
var actionCaps = map[string]map[string]capability{
	"reloadxml":      {"": capOperate},
	"reloadacl":      {"": capOperate},
	"sofia_rescan":   {"": capOperate},
	"sofia_profile":  {"rescan": capOperate, "start": capControl, "stop": capControl, "restart": capControl, "flush_inbound_reg": capControl},
	"sofia_gateway":  {"register": capOperate, "unregister": capOperate, "kill": capControl},
	"sofia_siptrace": {"": capControl},
	"module":         {"load": capControl, "unload": capControl, "reload": capControl},
	"fsctl":          {"loglevel": capControl, "max_sessions": capControl, "sps": capControl, "pause": capControl, "resume": capControl},
	"setvar":         {"": capControl},
}

var (
	nameRE   = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
	moduleRE = regexp.MustCompile(`^mod_[a-z0-9_]+$`)
)

// fsctlRanges are the allowed values of the fsctl operations that take one.
var fsctlRanges = map[string][2]int{"loglevel": {0, 7}, "max_sessions": {1, 100000}, "sps": {1, 10000}}

// actionCommand validates every argument, since they are interpolated into
// the ESL command, and returns the command and its audit detail.
func actionCommand(req actionRequest) (command, detail string, err error) {
	name := func(field, v string) error {
		if !nameRE.MatchString(v) {
			return fmt.Errorf("invalid %s name", field)
		}
		return nil
	}
	switch req.Action {
	case "reloadxml", "reloadacl":
		return req.Action, "", nil
	case "sofia_rescan":
		if err := name("profile", req.Profile); err != nil {
			return "", "", err
		}
		return "sofia profile " + req.Profile + " rescan", "profile=" + req.Profile, nil
	case "sofia_profile":
		if err := name("profile", req.Profile); err != nil {
			return "", "", err
		}
		return "sofia profile " + req.Profile + " " + req.Op, "op=" + req.Op + " profile=" + req.Profile, nil
	case "sofia_siptrace":
		if err := name("profile", req.Profile); err != nil {
			return "", "", err
		}
		if req.On == nil {
			return "", "", errors.New("on must be true or false")
		}
		state := "off"
		if *req.On {
			state = "on"
		}
		return "sofia profile " + req.Profile + " siptrace " + state, "profile=" + req.Profile + " siptrace=" + state, nil
	case "sofia_gateway":
		if err := name("profile", req.Profile); err != nil {
			return "", "", err
		}
		if err := name("gateway", req.Gateway); err != nil {
			return "", "", err
		}
		verb := req.Op
		if verb == "kill" {
			verb = "killgw"
		}
		return "sofia profile " + req.Profile + " " + verb + " " + req.Gateway, "op=" + req.Op + " profile=" + req.Profile + " gateway=" + req.Gateway, nil
	case "module":
		if !moduleRE.MatchString(req.Module) {
			return "", "", errors.New("module must look like mod_name: lowercase letters, digits and '_'")
		}
		return req.Op + " " + req.Module, "op=" + req.Op + " module=" + req.Module, nil
	case "fsctl":
		v := string(req.Value)
		rng, takes := fsctlRanges[req.Op]
		if !takes {
			if v != "" {
				return "", "", fmt.Errorf("fsctl %s takes no value", req.Op)
			}
			return "fsctl " + req.Op, "op=" + req.Op, nil
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < rng[0] || n > rng[1] {
			return "", "", fmt.Errorf("fsctl %s needs a value from %d to %d", req.Op, rng[0], rng[1])
		}
		return "fsctl " + req.Op + " " + strconv.Itoa(n), "op=" + req.Op + " value=" + strconv.Itoa(n), nil
	case "setvar":
		v := string(req.Value)
		if err := name("variable", req.Name); err != nil {
			return "", "", err
		}
		if strings.ContainsAny(v, "\r\n") || utf8.RuneCountInString(v) > 1024 {
			return "", "", errors.New("value must be one line of at most 1024 characters")
		}
		// The audit log masks values of variables that look like secrets.
		shown := fsapi.MaskVars([]fsapi.Var{{Name: req.Name, Value: v}})[0].Value
		return "global_setvar " + req.Name + "=" + v, "name=" + req.Name + " value=" + shown, nil
	}
	return "", "", errors.New("unknown action")
}

// handleAction runs a runtime operation. The route needs capOperate; each
// operation then needs the capability actionCaps gives it.
func (s *Server) handleAction(w http.ResponseWriter, r *http.Request, u user) {
	inst := s.lookup(w, r)
	if inst == nil {
		return
	}
	var req actionRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	ops, known := actionCaps[req.Action]
	if !known {
		writeError(w, http.StatusBadRequest, "unknown action")
		return
	}
	need, known := ops[req.Op]
	if !known {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown op %q for %s", truncate(req.Op, 32), req.Action))
		return
	}
	entry := audit.Entry{User: u.Username, Action: "action." + req.Action, Target: s.instanceName(r.PathValue("id"))}
	if req.Op != "" {
		entry.Detail = "op=" + req.Op
	}
	if !allowed(u.Role, need) {
		entry.Result, entry.Error = audit.Denied, "permission denied"
		s.audit(r, entry)
		writeError(w, http.StatusForbidden, "permission denied")
		return
	}
	command, detail, err := actionCommand(req)
	if err != nil {
		s.record(r, entry, err)
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	entry.Detail = detail
	if !ready(w, inst) {
		s.record(r, entry, esl.ErrNotConnected)
		return
	}
	ctx, cancel := eslContext(r, eslTimeout)
	defer cancel()
	out, err := fsapi.Run(ctx, inBackground{inst.sw, eslTimeout}, command)
	s.record(r, entry, err)
	writeOutput(w)(out, err)
}

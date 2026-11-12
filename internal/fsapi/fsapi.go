// Package fsapi runs typed queries against FreeSWITCH over an ESL API
// connection and parses their output.
package fsapi

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
)

// Commander runs "api <command>" and returns the reply body verbatim.
type Commander interface {
	API(ctx context.Context, command string) (string, error)
}

var (
	// ErrInvalidArgument is returned before anything is sent when an argument
	// would not be safe to interpolate into an ESL command.
	ErrInvalidArgument = errors.New("invalid argument")
	// ErrNotFound is returned when xml_locate finds nothing.
	ErrNotFound = errors.New("not found")
	// ErrBadReply is returned when a reply cannot be parsed.
	ErrBadReply = errors.New("unexpected reply from FreeSWITCH")
)

// CommandError is a reply body starting with "-ERR".
type CommandError struct {
	Command string
	Reply   string
}

func (e *CommandError) Error() string { return e.Reply }

// Var is a global variable.
type Var struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// ConfigInfo describes one <configuration> of the configuration section.
type ConfigInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

var (
	nameRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
	uuidRe = regexp.MustCompile(`^[A-Za-z0-9-]+$`)
)

var rowKinds = map[string]bool{"registrations": true, "channels": true, "calls": true, "modules": true}

const notFoundReply = "can't find anything"

func run(ctx context.Context, c Commander, cmd string) (string, error) {
	body, err := c.API(ctx, cmd)
	if err != nil {
		return "", err
	}
	if t := strings.TrimSpace(body); strings.HasPrefix(t, "-ERR") {
		return "", &CommandError{Command: cmd, Reply: t}
	}
	return body, nil
}

func output(ctx context.Context, c Commander, cmd string) (string, error) {
	body, err := run(ctx, c, cmd)
	return strings.TrimSpace(body), err
}

func badReply(cmd string, err error) error {
	return fmt.Errorf("%w: %s: %v", ErrBadReply, cmd, err)
}

// Status returns the "response" object of the JSON status command.
func Status(ctx context.Context, c Commander) (json.RawMessage, error) {
	const cmd = `json {"command":"status","data":""}`
	body, err := run(ctx, c, cmd)
	if err != nil {
		return nil, err
	}
	var reply struct {
		Response json.RawMessage `json:"response"`
	}
	if err := json.Unmarshal([]byte(body), &reply); err != nil {
		return nil, badReply(cmd, err)
	}
	if !bytes.HasPrefix(bytes.TrimSpace(reply.Response), []byte("{")) {
		return nil, badReply(cmd, errors.New("no response object"))
	}
	return reply.Response, nil
}

// Hostname returns the switch host name.
func Hostname(ctx context.Context, c Commander) (string, error) {
	return output(ctx, c, "hostname")
}

// Rows returns the rows of "show <kind> as json"; never nil.
func Rows(ctx context.Context, c Commander, kind string) ([]map[string]any, error) {
	if !rowKinds[kind] {
		return nil, fmt.Errorf("%w: unknown kind %q", ErrInvalidArgument, kind)
	}
	cmd := "show " + kind + " as json"
	body, err := run(ctx, c, cmd)
	if err != nil {
		return nil, err
	}
	var reply struct {
		Rows []map[string]any `json:"rows"`
	}
	if err := json.Unmarshal([]byte(body), &reply); err != nil {
		return nil, badReply(cmd, err)
	}
	if reply.Rows == nil {
		reply.Rows = []map[string]any{}
	}
	return reply.Rows, nil
}

// SofiaStatus returns one row per profile, gateway and alias.
func SofiaStatus(ctx context.Context, c Commander) ([]map[string]string, error) {
	const cmd = "sofia xmlstatus"
	root, err := runXML(ctx, c, cmd, "profiles")
	if err != nil {
		return nil, err
	}
	rows := []map[string]string{}
	for _, item := range root.Children {
		row := item.fields()
		if _, ok := row["type"]; !ok {
			row["type"] = item.XMLName.Local
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// SofiaGateways returns one row per gateway with every child element.
func SofiaGateways(ctx context.Context, c Commander) ([]map[string]string, error) {
	const cmd = "sofia xmlstatus gateway"
	root, err := runXML(ctx, c, cmd, "gateways")
	if err != nil {
		return nil, err
	}
	rows := []map[string]string{}
	for _, item := range root.Children {
		if item.XMLName.Local == "gateway" {
			rows = append(rows, item.fields())
		}
	}
	return rows, nil
}

// GlobalVars returns the global variables sorted by name, masked.
func GlobalVars(ctx context.Context, c Commander) ([]Var, error) {
	body, err := run(ctx, c, "global_getvar")
	if err != nil {
		return nil, err
	}
	vars := []Var{}
	for _, line := range strings.Split(body, "\n") {
		name, value, ok := strings.Cut(strings.TrimRight(line, "\r"), "=")
		if ok && name != "" {
			vars = append(vars, Var{Name: name, Value: value})
		}
	}
	sort.SliceStable(vars, func(i, j int) bool { return vars[i].Name < vars[j].Name })
	return MaskVars(vars), nil
}

// ConfigList returns the <configuration> entries of the configuration
// section, sorted by name.
func ConfigList(ctx context.Context, c Commander) ([]ConfigInfo, error) {
	const cmd = "xml_locate configuration"
	body, err := run(ctx, c, cmd)
	if err != nil {
		return nil, err
	}
	list := []ConfigInfo{}
	if strings.TrimSpace(body) == notFoundReply {
		return list, nil
	}
	// Stream the tokens: the section holds every configuration file, and only
	// the attributes of its direct children are needed.
	d := newDecoder(body)
	depth := 0
	for {
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, badReply(cmd, err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			if depth == 2 && t.Name.Local == "configuration" {
				list = append(list, ConfigInfo{Name: attr(t, "name"), Description: attr(t, "description")})
			}
		case xml.EndElement:
			depth--
		}
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	return list, nil
}

// Config returns one configuration as masked, indented XML.
func Config(ctx context.Context, c Commander, name string) (string, error) {
	if !nameRe.MatchString(name) {
		return "", fmt.Errorf("%w: invalid configuration name", ErrInvalidArgument)
	}
	cmd := "xml_locate configuration configuration name " + name
	body, err := run(ctx, c, cmd)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(body) == notFoundReply {
		return "", fmt.Errorf("%w: configuration %s", ErrNotFound, name)
	}
	out, err := MaskXML(body)
	if err != nil {
		return "", badReply(cmd, err)
	}
	return out, nil
}

// SectionItem is one named entry of the dialplan or directory section.
type SectionItem struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// DialplanContexts returns one item per dialplan <context>, counting its
// <extension> children, sorted by name.
func DialplanContexts(ctx context.Context, c Commander) ([]SectionItem, error) {
	return sectionItems(ctx, c, "dialplan", "context", "extension", false)
}

// DialplanContext returns one dialplan context as masked, indented XML.
func DialplanContext(ctx context.Context, c Commander, name string) (string, error) {
	return sectionEntry(ctx, c, "dialplan", "context", name)
}

// DirectoryDomains returns one item per directory <domain>, counting every
// <user> element below it (pointer entries included), sorted by name.
func DirectoryDomains(ctx context.Context, c Commander) ([]SectionItem, error) {
	return sectionItems(ctx, c, "directory", "domain", "user", true)
}

// DirectoryDomain returns one directory domain as masked, indented XML.
func DirectoryDomain(ctx context.Context, c Commander, name string) (string, error) {
	return sectionEntry(ctx, c, "directory", "domain", name)
}

// sectionItems lists the <item> children of an xml_locate section and counts
// the <child> elements below each: direct children only, or at any depth.
func sectionItems(ctx context.Context, c Commander, section, item, child string, anyDepth bool) ([]SectionItem, error) {
	cmd := "xml_locate " + section
	body, err := run(ctx, c, cmd)
	if err != nil {
		return nil, err
	}
	list := []SectionItem{}
	if strings.TrimSpace(body) == notFoundReply {
		return list, nil
	}
	d := newDecoder(body)
	depth, cur := 0, -1 // cur indexes the item being read
	for {
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, badReply(cmd, err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			switch {
			case depth == 2 && t.Name.Local == item:
				list = append(list, SectionItem{Name: attr(t, "name")})
				cur = len(list) - 1
			case depth == 2:
				cur = -1
			case cur >= 0 && t.Name.Local == child && (anyDepth || depth == 3):
				list[cur].Count++
			}
		case xml.EndElement:
			depth--
		}
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	return list, nil
}

// sectionEntry returns "xml_locate <section> <tag> name <name>" as masked,
// indented XML.
func sectionEntry(ctx context.Context, c Commander, section, tag, name string) (string, error) {
	if !nameRe.MatchString(name) {
		return "", fmt.Errorf("%w: invalid %s name", ErrInvalidArgument, tag)
	}
	cmd := "xml_locate " + section + " " + tag + " name " + name
	body, err := run(ctx, c, cmd)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(body) == notFoundReply {
		return "", fmt.Errorf("%w: %s %s", ErrNotFound, tag, name)
	}
	out, err := MaskXML(body)
	if err != nil {
		return "", badReply(cmd, err)
	}
	return out, nil
}

// HangupChannel kills the channel with the given uuid.
func HangupChannel(ctx context.Context, c Commander, uuid string) (string, error) {
	if !uuidRe.MatchString(uuid) {
		return "", fmt.Errorf("%w: invalid channel uuid", ErrInvalidArgument)
	}
	return output(ctx, c, "uuid_kill "+uuid)
}

// ReloadXML runs reloadxml.
func ReloadXML(ctx context.Context, c Commander) (string, error) {
	return output(ctx, c, "reloadxml")
}

// ReloadACL runs reloadacl.
func ReloadACL(ctx context.Context, c Commander) (string, error) {
	return output(ctx, c, "reloadacl")
}

// SofiaRescan rescans a sofia profile for new gateways.
func SofiaRescan(ctx context.Context, c Commander, profile string) (string, error) {
	if !nameRe.MatchString(profile) {
		return "", fmt.Errorf("%w: invalid profile name", ErrInvalidArgument)
	}
	return output(ctx, c, "sofia profile "+profile+" rescan")
}

type xmlNode struct {
	XMLName  xml.Name
	Children []xmlNode `xml:",any"`
	Text     string    `xml:",chardata"`
}

func (n xmlNode) fields() map[string]string {
	row := make(map[string]string, len(n.Children))
	for _, f := range n.Children {
		row[f.XMLName.Local] = strings.TrimSpace(f.Text)
	}
	return row
}

func runXML(ctx context.Context, c Commander, cmd, rootName string) (xmlNode, error) {
	body, err := run(ctx, c, cmd)
	if err != nil {
		return xmlNode{}, err
	}
	var root xmlNode
	if err := newDecoder(body).Decode(&root); err != nil {
		return xmlNode{}, badReply(cmd, err)
	}
	if root.XMLName.Local != rootName {
		return xmlNode{}, badReply(cmd, fmt.Errorf("root element <%s>, want <%s>", root.XMLName.Local, rootName))
	}
	return root, nil
}

// newDecoder tolerates what FreeSWITCH emits: an ISO-8859-1 declaration on
// UTF-8/ASCII text, or none, and unescaped '&' in values.
func newDecoder(s string) *xml.Decoder {
	d := xml.NewDecoder(strings.NewReader(s))
	d.Strict = false
	d.CharsetReader = func(_ string, r io.Reader) (io.Reader, error) { return r, nil }
	return d
}

func attr(t xml.StartElement, name string) string {
	for _, a := range t.Attr {
		if a.Name.Space == "" && a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}

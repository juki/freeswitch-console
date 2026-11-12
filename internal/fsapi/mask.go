package fsapi

import (
	"encoding/xml"
	"fmt"
	"io"
	"regexp"
	"strings"
)

// Mask replaces secret values.
const Mask = "********"

var (
	// A bare "pass" would also hit inbound-bypass-media and pass-rfc2833.
	sensitiveRe = regexp.MustCompile(`(?i)pass(w|phrase)|(^|[-_.])pass$|secret|a1-hash|token|dsn|api[-_]?key|auth[-_]?key|credential`)
	// The password may itself contain '@'; the greedy match stops at the last
	// '@' before the path.
	urlPasswordRe = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://[^/?#@\s:]*):[^/?#\s]*@`)
)

func maskURL(s string) string {
	return urlPasswordRe.ReplaceAllString(s, "${1}:"+Mask+"@")
}

// MaskVars masks the value of every variable whose name looks secret and the
// password part of URLs in the other values.
func MaskVars(vars []Var) []Var {
	out := make([]Var, len(vars))
	for i, v := range vars {
		if sensitiveRe.MatchString(v.Name) {
			v.Value = Mask
		} else {
			v.Value = maskURL(v.Value)
		}
		out[i] = v
	}
	return out
}

// maskNode is an element, or a text node when name is empty.
type maskNode struct {
	name     string
	attrs    []xml.Attr
	text     string
	children []*maskNode
}

// MaskXML parses an XML document and re-emits it indented with secrets
// masked: the value and data attributes of an element whose name attribute
// looks secret, every attribute whose own name looks secret, and URL
// passwords elsewhere. Comments, processing instructions and directives are
// dropped (comments often hold commented-out credentials).
func MaskXML(doc string) (string, error) {
	d := newDecoder(doc)
	root := &maskNode{}
	stack := []*maskNode{root}
	for {
		tok, err := d.RawToken()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		top := stack[len(stack)-1]
		switch t := tok.(type) {
		case xml.StartElement:
			st := maskStart(t)
			n := &maskNode{name: st.Name.Local, attrs: st.Attr}
			top.children = append(top.children, n)
			stack = append(stack, n)
		case xml.EndElement:
			if name := flatName(t.Name).Local; len(stack) == 1 || name != top.name {
				return "", fmt.Errorf("unexpected </%s>", name)
			}
			stack = stack[:len(stack)-1]
		case xml.CharData:
			if s := strings.TrimSpace(string(t)); s != "" {
				top.children = append(top.children, &maskNode{text: maskURL(s)})
			}
		}
	}
	if len(stack) != 1 {
		return "", fmt.Errorf("unclosed <%s>", stack[len(stack)-1].name)
	}
	var b strings.Builder
	for _, n := range root.children {
		writeNode(&b, n, 0)
	}
	return b.String(), nil
}

// writeNode prints one element per line; an element without children is
// self-closing and one that holds only text stays on a single line.
func writeNode(b *strings.Builder, n *maskNode, depth int) {
	indent := strings.Repeat("  ", depth)
	b.WriteString(indent)
	if n.name == "" {
		xml.EscapeText(b, []byte(n.text))
		b.WriteByte('\n')
		return
	}
	b.WriteByte('<')
	b.WriteString(n.name)
	for _, a := range n.attrs {
		b.WriteByte(' ')
		b.WriteString(a.Name.Local)
		b.WriteString(`="`)
		xml.EscapeText(b, []byte(a.Value))
		b.WriteByte('"')
	}
	switch {
	case len(n.children) == 0:
		b.WriteString("/>\n")
	case len(n.children) == 1 && n.children[0].name == "":
		b.WriteByte('>')
		xml.EscapeText(b, []byte(n.children[0].text))
		b.WriteString("</" + n.name + ">\n")
	default:
		b.WriteString(">\n")
		for _, c := range n.children {
			writeNode(b, c, depth+1)
		}
		b.WriteString(indent + "</" + n.name + ">\n")
	}
}

func maskStart(t xml.StartElement) xml.StartElement {
	secretElem := false
	for _, a := range t.Attr {
		if a.Name.Space == "" && a.Name.Local == "name" && sensitiveRe.MatchString(a.Value) {
			secretElem = true
		}
	}
	out := xml.StartElement{Name: flatName(t.Name), Attr: make([]xml.Attr, len(t.Attr))}
	for i, a := range t.Attr {
		a.Name = flatName(a.Name)
		switch {
		case sensitiveRe.MatchString(a.Name.Local):
			a.Value = Mask
		case secretElem && (a.Name.Local == "value" || a.Name.Local == "data"):
			a.Value = Mask
		default:
			a.Value = maskURL(a.Value)
		}
		out.Attr[i] = a
	}
	return out
}

// flatName keeps a prefixed name verbatim; RawToken does not resolve
// namespaces.
func flatName(n xml.Name) xml.Name {
	if n.Space != "" {
		return xml.Name{Local: n.Space + ":" + n.Local}
	}
	return n
}

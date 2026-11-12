package fsapi

import (
	"encoding/xml"
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

// MaskXML parses an XML document and re-emits it indented with secrets
// masked: the value and data attributes of an element whose name attribute
// looks secret, every attribute whose own name looks secret, and URL
// passwords elsewhere. Comments, processing instructions and directives are
// dropped (comments often hold commented-out credentials).
func MaskXML(doc string) (string, error) {
	d := newDecoder(doc)
	var b strings.Builder
	e := xml.NewEncoder(&b)
	e.Indent("", "  ")
	for {
		tok, err := d.RawToken()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			err = e.EncodeToken(maskStart(t))
		case xml.EndElement:
			err = e.EncodeToken(xml.EndElement{Name: flatName(t.Name)})
		case xml.CharData:
			if strings.TrimSpace(string(t)) != "" {
				err = e.EncodeToken(xml.CharData(maskURL(string(t))))
			}
		}
		if err != nil {
			return "", err
		}
	}
	// Close reports unclosed elements, so the output is always well-formed.
	if err := e.Close(); err != nil {
		return "", err
	}
	return b.String() + "\n", nil
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
// namespaces and the encoder would otherwise invent xmlns attributes.
func flatName(n xml.Name) xml.Name {
	if n.Space != "" {
		return xml.Name{Local: n.Space + ":" + n.Local}
	}
	return n
}

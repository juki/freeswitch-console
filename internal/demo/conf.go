package demo

import (
	"embed"
	"encoding/xml"
	"fmt"
	"io"
	"io/fs"
	"path"
	"regexp"
	"strings"
)

// confFS is the sample configuration, written for this project.
//
//go:embed conf
var confFS embed.FS

// confRoot is confFS with paths relative to the configuration directory.
var confRoot = func() fs.FS {
	sub, err := fs.Sub(confFS, "conf")
	if err != nil {
		panic(err)
	}
	return sub
}()

// configuration is the sample configuration as one switch loads it.
type configuration struct {
	doc  string      // freeswitch.xml with every X-PRE-PROCESS carried out
	vars [][2]string // the global variables in the order they were set
}

var (
	preRe = regexp.MustCompile(`<X-PRE-PROCESS\s+cmd="([^"]*)"\s+data="([^"]*)"\s*/>`)
	varRe = regexp.MustCompile(`\$\$\{([^}]*)\}`)
)

// loadConf reads freeswitch.xml as FreeSWITCH does: builtin are the
// variables the switch sets before, X-PRE-PROCESS set defines a variable,
// include inlines the files a glob relative to the including file matches
// (an <include> root is dropped), and $${name} is replaced on every line.
func loadConf(builtin [][2]string) (*configuration, error) {
	p := &preprocessor{vars: map[string]string{}}
	for _, kv := range builtin {
		p.set(kv[0], kv[1])
	}
	if err := p.file("freeswitch.xml", 0); err != nil {
		return nil, err
	}
	c := &configuration{doc: p.out.String()}
	for _, name := range p.order {
		c.vars = append(c.vars, [2]string{name, p.vars[name]})
	}
	if err := wellFormed(c.doc); err != nil {
		return nil, fmt.Errorf("freeswitch.xml after preprocessing: %w", err)
	}
	return c, nil
}

type preprocessor struct {
	vars  map[string]string
	order []string
	out   strings.Builder
}

func (p *preprocessor) set(name, value string) {
	if _, ok := p.vars[name]; !ok {
		p.order = append(p.order, name)
	}
	p.vars[name] = value
}

func (p *preprocessor) expand(s string) string {
	return varRe.ReplaceAllStringFunc(s, func(m string) string { return p.vars[m[3:len(m)-1]] })
}

func (p *preprocessor) file(name string, depth int) error {
	if depth > 8 {
		return fmt.Errorf("%s: includes nested too deep", name)
	}
	data, err := fs.ReadFile(confRoot, name)
	if err != nil {
		return err
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if depth > 0 {
		lines = unwrapInclude(lines)
	}
	for _, line := range lines {
		m := preRe.FindStringSubmatch(line)
		if m == nil {
			p.out.WriteString(p.expand(line))
			p.out.WriteByte('\n')
			continue
		}
		switch arg := p.expand(m[2]); m[1] {
		case "set":
			k, v, _ := strings.Cut(arg, "=")
			p.set(k, v)
		case "include":
			files, err := fs.Glob(confRoot, path.Join(path.Dir(name), arg))
			if err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			for _, f := range files {
				if err := p.file(f, depth+1); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// unwrapInclude drops an XML declaration and an <include> root.
func unwrapInclude(lines []string) []string {
	if len(lines) > 0 && strings.HasPrefix(strings.TrimSpace(lines[0]), "<?xml") {
		lines = lines[1:]
	}
	if len(lines) >= 2 && strings.TrimSpace(lines[0]) == "<include>" && strings.TrimSpace(lines[len(lines)-1]) == "</include>" {
		lines = lines[1 : len(lines)-1]
	}
	return lines
}

func wellFormed(doc string) error {
	d := xml.NewDecoder(strings.NewReader(doc))
	for {
		if _, err := d.Token(); err == io.EOF {
			return nil
		} else if err != nil {
			return err
		}
	}
}

// locate returns the text of what xml_locate finds: the whole document for
// no section, a section, or the first element of the section with the tag
// whose attribute attr is value.
func (c *configuration) locate(section, tag, attr, value string) (string, bool) {
	if section == "" {
		return c.doc, true
	}
	d := xml.NewDecoder(strings.NewReader(c.doc))
	depth, start, inSection := 0, -1, false
	for {
		off := int(d.InputOffset())
		tok, err := d.Token()
		if err != nil {
			return "", false
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			switch {
			case start >= 0:
			case depth == 2 && t.Name.Local == "section" && attrOf(t, "name") == section:
				inSection = true
				if tag == "" {
					start = off
				}
			case inSection && depth == 3 && t.Name.Local == tag && attrOf(t, attr) == value:
				start = off
			}
		case xml.EndElement:
			if start >= 0 && (depth == 2 && tag == "" || depth == 3 && tag != "") {
				return c.doc[start:d.InputOffset()], true
			}
			if depth == 2 {
				inSection = false
			}
			depth--
		}
	}
}

func attrOf(t xml.StartElement, name string) string {
	for _, a := range t.Attr {
		if a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}

package fsapi

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// MaxCompleteLine is the longest line Complete accepts, in bytes.
const MaxCompleteLine = 1024

// maxMatches bounds the matches Complete returns.
const maxMatches = 200

// Completion is FreeSWITCH's answer to a Tab: the candidates, and the text
// that replaces the last Replace bytes before the cursor.
type Completion struct {
	Matches []string `json:"matches"`
	Replace int      `json:"replace"`
	Text    string   `json:"text"`
}

// Complete asks FreeSWITCH to complete line, the console input up to the
// cursor. FreeSWITCH strips trailing spaces from a command, so a sentinel
// character follows the cursor: "show " completes the next word instead of
// "show" itself. An empty line goes without one, since cursor 0 counts as no
// cursor and the sentinel would become the word.
func Complete(ctx context.Context, c Commander, line string) (Completion, error) {
	if err := CheckCompleteLine(line); err != nil {
		return Completion{}, err
	}
	cmd := "console_complete_xml c=" + strconv.Itoa(len(line)) + ";" + line
	if line != "" {
		cmd += "x"
	}
	body, err := run(ctx, c, cmd)
	if err != nil {
		return Completion{}, err
	}
	var doc struct {
		XMLName xml.Name `xml:"complete"`
		Matches []string `xml:"match"`
		Write   *struct {
			Length string `xml:"length,attr"`
			Text   string `xml:",chardata"`
		} `xml:"write"`
	}
	if err := xml.Unmarshal([]byte(strings.TrimSpace(body)), &doc); err != nil {
		return Completion{}, badReply("console_complete_xml", err)
	}
	out := Completion{Matches: []string{}}
	seen := map[string]bool{}
	for _, m := range doc.Matches {
		if m = strings.TrimSpace(m); m != "" && !seen[m] && len(out.Matches) < maxMatches {
			seen[m] = true
			out.Matches = append(out.Matches, m)
		}
	}
	if doc.Write != nil {
		n, err := strconv.Atoi(doc.Write.Length)
		if err != nil || n < 0 || n > len(line) {
			return Completion{}, badReply("console_complete_xml", fmt.Errorf("write length %q", doc.Write.Length))
		}
		out.Replace, out.Text = n, doc.Write.Text
	}
	// Without a match there is nothing to write, whatever length came back.
	if len(out.Matches) == 0 {
		out.Replace, out.Text = 0, ""
	}
	return out, nil
}

// CheckCompleteLine rejects a line that is too long or holds a line break:
// it is interpolated into an ESL command.
func CheckCompleteLine(line string) error {
	if len(line) > MaxCompleteLine {
		return fmt.Errorf("%w: line is longer than %d bytes", ErrInvalidArgument, MaxCompleteLine)
	}
	if strings.ContainsAny(line, "\r\n") {
		return fmt.Errorf("%w: line must not contain line breaks", ErrInvalidArgument)
	}
	return nil
}

// APICommand is one API command FreeSWITCH offers.
type APICommand struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Syntax      string `json:"syntax"`
}

// APICommands lists the API commands from "show api as json", sorted by
// name, each name once.
func APICommands(ctx context.Context, c Commander) ([]APICommand, error) {
	const cmd = "show api as json"
	body, err := run(ctx, c, cmd)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Rows []APICommand `json:"rows"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(body)), &doc); err != nil {
		return nil, badReply(cmd, err)
	}
	out := []APICommand{}
	seen := map[string]bool{}
	for _, r := range doc.Rows {
		if r.Name != "" && !seen[r.Name] {
			seen[r.Name] = true
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

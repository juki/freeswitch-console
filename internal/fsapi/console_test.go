package fsapi

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// Answers as the real FreeSWITCH 1.11.3 gave them.
const (
	completeProfile = `<?xml version="1.0"?>
<complete>
  <match>172.17.0.2</match>
  <match>external</match>
  <match>external-ipv6</match>
  <match>internal</match>
  <match>internal-ipv6</match>
  <match>restart</match>
  <write length="0"></write>
</complete>

`
	completeShowCh = `<?xml version="1.0"?>
<complete>
  <match>channels</match>
  <match>chat</match>
  <write length="2">cha</write>
</complete>
`
	completeNone = `<?xml version="1.0"?>
<complete>
  <write length="3"></write>
</complete>
`
)

func TestComplete(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		line, cmd, reply string
		want             Completion
	}{
		{"sofia profile ", "console_complete_xml c=14;sofia profile x", completeProfile,
			Completion{Matches: []string{"172.17.0.2", "external", "external-ipv6", "internal", "internal-ipv6", "restart"}}},
		{"show ch", "console_complete_xml c=7;show chx", completeShowCh,
			Completion{Matches: []string{"channels", "chat"}, Replace: 2, Text: "cha"}},
		{"zzz", "console_complete_xml c=3;zzzx", completeNone, Completion{Matches: []string{}}},
		// Empty line: no sentinel, it would become the word to complete.
		{"", "console_complete_xml c=0;", "<complete><match>acl</match><match>alias</match><write length=\"0\"></write></complete>",
			Completion{Matches: []string{"acl", "alias"}}},
		// Byte length, not characters.
		{"echo é", "console_complete_xml c=7;echo éx", "<complete><write length=\"2\"></write></complete>", Completion{Matches: []string{}}},
		// Duplicates go, order stays; unknown elements are ignored; no write.
		{"sofia profile internal ", "console_complete_xml c=23;sofia profile internal x",
			"<?xml version=\"1.0\"?><complete><match>start</match><match>stop</match><match>start</match><extra/><match> </match></complete>",
			Completion{Matches: []string{"start", "stop"}}},
	} {
		c := reply(tc.cmd, tc.reply)
		got, err := Complete(ctx, c, tc.line)
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%q: %+v %v (sent %q)", tc.line, got, err, c.sent)
		}
	}

	// At most 200 matches.
	many := "<complete>"
	for i := 0; i < 250; i++ {
		many += "<match>m" + strings.Repeat("x", i) + "</match>"
	}
	many += `<write length="1">m</write></complete>`
	if got, err := Complete(ctx, reply("console_complete_xml c=1;mx", many), "m"); err != nil || len(got.Matches) != 200 {
		t.Fatalf("%d matches, %v", len(got.Matches), err)
	}

	for _, line := range []string{"a\nb", "a\rapi shutdown", strings.Repeat("a", MaxCompleteLine+1)} {
		c := &fakeCommander{}
		if _, err := Complete(ctx, c, line); !errors.Is(err, ErrInvalidArgument) || len(c.sent) != 0 {
			t.Errorf("%q: %v, sent %q", line[:min(len(line), 20)], err, c.sent)
		}
	}
	if _, err := Complete(ctx, reply("console_complete_xml c=1024;"+strings.Repeat("a", 1024)+"x", completeNone), strings.Repeat("a", 1024)); err != nil {
		t.Fatalf("1024 bytes: %v", err)
	}

	for name, body := range map[string]string{
		"not xml":     "+OK",
		"other root":  "<completion><match>a</match></completion>",
		"unclosed":    "<complete><match>a</match>",
		"bad length":  `<complete><match>a</match><write length="x">a</write></complete>`,
		"negative":    `<complete><match>a</match><write length="-1">a</write></complete>`,
		"beyond line": `<complete><match>a</match><write length="9">a</write></complete>`,
		"unescaped":   `<complete><match>a&b</match></complete>`,
	} {
		if _, err := Complete(ctx, reply("console_complete_xml c=1;ax", body), "a"); !errors.Is(err, ErrBadReply) {
			t.Errorf("%s: %v", name, err)
		}
	}
	var ce *CommandError
	if _, err := Complete(ctx, reply("console_complete_xml c=1;ax", "-ERR console_complete_xml Command not found!\n"), "a"); !errors.As(err, &ce) {
		t.Fatalf("-ERR: %v", err)
	}
}

func TestAPICommands(t *testing.T) {
	ctx := context.Background()
	body := `{"row_count":4,"rows":[{"name":"status","description":"Show current status","syntax":"","ikey":"mod_commands"},{"name":"acl","description":"Compare an ip to an acl list","syntax":"<ip> <list_name>","ikey":"mod_commands"},{"name":"acl","description":"dup","syntax":"","ikey":"x"},{"name":"","description":"nameless"}]}` + "\n"
	got, err := APICommands(ctx, reply("show api as json", body))
	want := []APICommand{{"acl", "Compare an ip to an acl list", "<ip> <list_name>"}, {"status", "Show current status", ""}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("%+v %v", got, err)
	}
	if got, err := APICommands(ctx, reply("show api as json", `{"row_count":0}`)); err != nil || got == nil || len(got) != 0 {
		t.Fatalf("empty: %v %v", got, err)
	}
	if _, err := APICommands(ctx, reply("show api as json", "nope")); !errors.Is(err, ErrBadReply) {
		t.Fatalf("bad: %v", err)
	}
}

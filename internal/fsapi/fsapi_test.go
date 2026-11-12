package fsapi

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"reflect"
	"strings"
	"testing"
)

type fakeCommander struct {
	replies map[string]string
	err     error
	sent    []string
}

func (f *fakeCommander) API(_ context.Context, cmd string) (string, error) {
	f.sent = append(f.sent, cmd)
	if f.err != nil {
		return "", f.err
	}
	return f.replies[cmd], nil
}

func reply(cmd, body string) *fakeCommander {
	return &fakeCommander{replies: map[string]string{cmd: body}}
}

const statusReply = `{"command":"status","data":"","status":"success","response":{"systemStatus":"ready","uptime":{"years":0,"days":1,"hours":2,"minutes":3,"seconds":4,"milliseconds":5,"microseconds":6},"version":"1.10.12-release 64bit","sessions":{"count":{"total":12,"active":1,"peak":3,"peak5Min":1,"limit":1000},"rate":{"current":0,"max":30,"peak":2,"peak5Min":0}},"idleCPU":{"used":0,"allowed":98.5},"stackSizeKB":{"current":240,"max":8192}}}
`

func TestStatus(t *testing.T) {
	c := reply(`json {"command":"status","data":""}`, statusReply)
	got, err := Status(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		SystemStatus string `json:"systemStatus"`
		Sessions     struct {
			Count struct{ Limit int } `json:"count"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(got, &v); err != nil {
		t.Fatal(err)
	}
	if v.SystemStatus != "ready" || v.Sessions.Count.Limit != 1000 {
		t.Fatalf("got %s", got)
	}

	for _, bad := range []string{"not json", `{"status":"error"}`, `{"response":"text"}`} {
		c := reply(`json {"command":"status","data":""}`, bad)
		if _, err := Status(context.Background(), c); !errors.Is(err, ErrBadReply) {
			t.Errorf("%q: err = %v, want ErrBadReply", bad, err)
		}
	}
}

func TestHostnameAndOutput(t *testing.T) {
	c := &fakeCommander{replies: map[string]string{
		"hostname":  "fs01\n",
		"reloadxml": "+OK [Success]\n\n",
		"reloadacl": "+OK acl reloaded\n",
	}}
	ctx := context.Background()
	if h, _ := Hostname(ctx, c); h != "fs01" {
		t.Errorf("hostname %q", h)
	}
	if out, _ := ReloadXML(ctx, c); out != "+OK [Success]" {
		t.Errorf("reloadxml %q", out)
	}
	if out, _ := ReloadACL(ctx, c); out != "+OK acl reloaded" {
		t.Errorf("reloadacl %q", out)
	}
}

func TestCommandError(t *testing.T) {
	c := reply("sofia xmlstatus", "-ERR sofia Command not found!\n")
	_, err := SofiaStatus(context.Background(), c)
	var ce *CommandError
	if !errors.As(err, &ce) || ce.Reply != "-ERR sofia Command not found!" || ce.Command != "sofia xmlstatus" {
		t.Fatalf("err = %#v", err)
	}
	if err.Error() != "-ERR sofia Command not found!" {
		t.Errorf("Error() = %q", err.Error())
	}
}

func TestTransportErrorPassesThrough(t *testing.T) {
	want := errors.New("esl: not connected")
	c := &fakeCommander{err: want}
	if _, err := Hostname(context.Background(), c); err != want {
		t.Fatalf("err = %v", err)
	}
}

func TestRows(t *testing.T) {
	ctx := context.Background()
	c := reply("show channels as json", `{"row_count":1,"rows":[{"uuid":"a1b2","direction":"inbound","cid_num":"1000"}]}`+"\n")
	rows, err := Rows(ctx, c, "channels")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0]["uuid"] != "a1b2" || rows[0]["cid_num"] != "1000" {
		t.Fatalf("rows %v", rows)
	}

	c = reply("show registrations as json", "{\"row_count\": 0}\n")
	rows, err = Rows(ctx, c, "registrations")
	if err != nil || rows == nil || len(rows) != 0 {
		t.Fatalf("empty rows = %#v, %v", rows, err)
	}
	b, _ := json.Marshal(rows)
	if string(b) != "[]" {
		t.Errorf("marshal = %s", b)
	}

	c = &fakeCommander{}
	if _, err := Rows(ctx, c, "channels; reloadxml"); !errors.Is(err, ErrInvalidArgument) || len(c.sent) != 0 {
		t.Errorf("bad kind: err %v, sent %v", err, c.sent)
	}
	for _, kind := range []string{"registrations", "channels", "calls", "modules"} {
		c := reply("show "+kind+" as json", `{"row_count":0}`)
		if _, err := Rows(ctx, c, kind); err != nil {
			t.Errorf("%s: %v", kind, err)
		}
	}
}

const sofiaStatusReply = `<?xml version="1.0" encoding="ISO-8859-1"?>
<profiles>
<profile>
<name>external</name>
<type>profile</type>
<data>sip:mod_sofia@203.0.113.5:5080</data>
<state>RUNNING (0)</state>
</profile>
<gateway>
<name>carrier</name>
<type>gateway</type>
<data>sip:sip.example.com&amp;x=1</data>
<state>FAIL_WAIT</state>
</gateway>
 (retry: 25s)
<alias>
<name>203.0.113.5</name>
<type>alias</type>
<data>internal</data>
<state>ALIASED</state>
</alias>
<custom>
<name>odd</name>
</custom>
</profiles>
`

func TestSofiaStatus(t *testing.T) {
	rows, err := SofiaStatus(context.Background(), reply("sofia xmlstatus", sofiaStatusReply))
	if err != nil {
		t.Fatal(err)
	}
	want := []map[string]string{
		{"name": "external", "type": "profile", "data": "sip:mod_sofia@203.0.113.5:5080", "state": "RUNNING (0)"},
		{"name": "carrier", "type": "gateway", "data": "sip:sip.example.com&x=1", "state": "FAIL_WAIT"},
		{"name": "203.0.113.5", "type": "alias", "data": "internal", "state": "ALIASED"},
		{"name": "odd", "type": "custom"},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("rows\n got %v\nwant %v", rows, want)
	}

	// No declaration, a bare '&' and no children.
	rows, err = SofiaStatus(context.Background(), reply("sofia xmlstatus", "<profiles>\n</profiles>\n"))
	if err != nil || rows == nil || len(rows) != 0 {
		t.Fatalf("empty = %#v, %v", rows, err)
	}
	rows, err = SofiaStatus(context.Background(), reply("sofia xmlstatus", "<profiles><gateway><name>a&b</name></gateway></profiles>"))
	if err != nil || rows[0]["name"] != "a&b" {
		t.Fatalf("bare amp = %v, %v", rows, err)
	}
	if _, err := SofiaStatus(context.Background(), reply("sofia xmlstatus", "Invalid Syntax!\n")); !errors.Is(err, ErrBadReply) {
		t.Errorf("garbage: %v", err)
	}
}

const gatewaysReply = `<?xml version="1.0" encoding="ISO-8859-1"?>
<gateways>
  <gateway>
    <name>carrier</name>
    <profile>external</profile>
    <scheme>Digest</scheme>
    <realm>sip.example.com</realm>
    <username>acct1</username>
    <password>yes</password>
    <from>&lt;sip:acct1@sip.example.com&gt;</from>
    <contact>&lt;sip:gw+carrier@203.0.113.5:5080;transport=udp;gw=carrier&gt;</contact>
    <exten>acct1</exten>
    <to>sip:acct1@sip.example.com</to>
    <proxy>sip:sip.example.com</proxy>
    <context>public</context>
    <expires>3600</expires>
    <freq>3600</freq>
    <ping>0</ping>
    <pingfreq>0</pingfreq>
    <pingmin>1</pingmin>
    <pingcount>0</pingcount>
    <pingmax>1</pingmax>
    <pingtime>0.00</pingtime>
    <pinging>0</pinging>
    <state>REGED</state>
    <status>UP</status>
    <uptime-usec>123456</uptime-usec>
    <calls-in>1</calls-in>
    <calls-out>2</calls-out>
    <failed-calls-in>0</failed-calls-in>
    <failed-calls-out>0</failed-calls-out>
  </gateway>
  <gateway>
    <name>second</name>
    <state>FAIL_WAIT</state>
    <retry>12s</retry>
  </gateway>
</gateways>
`

func TestSofiaGateways(t *testing.T) {
	rows, err := SofiaGateways(context.Background(), reply("sofia xmlstatus gateway", gatewaysReply))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows %v", rows)
	}
	g := rows[0]
	if g["name"] != "carrier" || g["from"] != "<sip:acct1@sip.example.com>" || g["uptime-usec"] != "123456" || g["status"] != "UP" || len(g) != 28 {
		t.Errorf("gateway %v (%d keys)", g, len(g))
	}
	if rows[1]["retry"] != "12s" {
		t.Errorf("second %v", rows[1])
	}
	rows, err = SofiaGateways(context.Background(), reply("sofia xmlstatus gateway", "<?xml version=\"1.0\" encoding=\"ISO-8859-1\"?>\n<gateways>\n</gateways>\n"))
	if err != nil || rows == nil || len(rows) != 0 {
		t.Errorf("empty = %#v, %v", rows, err)
	}
}

func TestGlobalVars(t *testing.T) {
	body := "local_ip_v4=10.0.0.5\nhostname=fs01\ndefault_password=1234\nxml_rpc_password=works\nbase_dir=/usr\n" +
		"hold_music=local_stream://moh\njson_db_handle=pgsql://hostaddr=127.0.0.1 password=x\n" +
		"remote=http://admin:s3cr3t@example.com/x\nempty=\nno separator line\n=novalue\nsome=a=b\n"
	vars, err := GlobalVars(context.Background(), reply("global_getvar", body))
	if err != nil {
		t.Fatal(err)
	}
	want := []Var{
		{"base_dir", "/usr"},
		{"default_password", Mask},
		{"empty", ""},
		{"hold_music", "local_stream://moh"},
		{"hostname", "fs01"},
		{"json_db_handle", "pgsql://hostaddr=127.0.0.1 password=x"},
		{"local_ip_v4", "10.0.0.5"},
		{"remote", "http://admin:" + Mask + "@example.com/x"},
		{"some", "a=b"},
		{"xml_rpc_password", Mask},
	}
	if !reflect.DeepEqual(vars, want) {
		t.Fatalf("vars\n got %v\nwant %v", vars, want)
	}
}

func TestMaskVars(t *testing.T) {
	tests := []struct{ name, value, want string }{
		{"default_password", "1234", Mask},
		{"PASSWORD", "x", Mask},
		{"zrtp_secret", "x", Mask},
		{"a1-hash", "x", Mask},
		{"api_token", "x", Mask},
		{"core-db-dsn", "x", Mask},
		{"odbc-dsn", "x", Mask},
		{"apikey", "x", Mask},
		{"api-key", "x", Mask},
		{"auth_key", "x", Mask},
		{"aws_credentials", "x", Mask},
		{"domain", "example.com", "example.com"},
		{"url", "mysql://root:pw@db:3306/fs", "mysql://root:" + Mask + "@db:3306/fs"},
		{"url2", "odbc://user:p@ss@host/db", "odbc://user:" + Mask + "@host/db"},
		{"url3", "sip:1000@example.com", "sip:1000@example.com"},
		{"url4", "http://user@example.com/", "http://user@example.com/"},
		{"two", "a://u:p@h b://v:q@i", "a://u:" + Mask + "@h b://v:" + Mask + "@i"},
	}
	in := make([]Var, len(tests))
	for i, tt := range tests {
		in[i] = Var{tt.name, tt.value}
	}
	out := MaskVars(in)
	for i, tt := range tests {
		if out[i].Value != tt.want || out[i].Name != tt.name {
			t.Errorf("%s=%q: got %q, want %q", tt.name, tt.value, out[i].Value, tt.want)
		}
		if in[i].Value != tt.value {
			t.Errorf("%s: input modified", tt.name)
		}
	}
}

func TestMaskXML(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		contains []string
		absent   []string
	}{
		{
			name:     "param password",
			in:       `<params><param name="password" value="x1"/><param name="dial-string" value="{a=b}"/></params>`,
			contains: []string{`<param name="password" value="********"/>`, `<param name="dial-string" value="{a=b}"/>`},
			absent:   []string{"x1"},
		},
		{
			name:     "variable default_password",
			in:       `<variables><variable name="default_password" value="1234"/><variable name="user_context" value="default"/></variables>`,
			contains: []string{`name="default_password" value="********"`, `value="default"`},
			absent:   []string{"1234"},
		},
		{
			name:     "data attribute of sensitive element",
			in:       `<x name="Secret-Thing" data="d1" other="o1"/>`,
			contains: []string{`data="********"`, `other="o1"`},
			absent:   []string{"d1"},
		},
		{
			name:     "attribute named password",
			in:       `<user id="1000" password="pw1" vm-password="pw2" a1-hash="h1"/>`,
			contains: []string{`id="1000"`, `password="********"`, `vm-password="********"`, `a1-hash="********"`},
			absent:   []string{"pw1", "pw2", "h1"},
		},
		{
			name:     "dsn params",
			in:       `<settings><param name="odbc-dsn" value="fs:root:pw3"/><param name="core-db-dsn" value="pgsql://hostaddr=1.2.3.4 password=pw4"/></settings>`,
			contains: []string{`name="odbc-dsn" value="********"`, `name="core-db-dsn" value="********"`},
			absent:   []string{"pw3", "pw4"},
		},
		{
			name:     "url in non-sensitive value",
			in:       `<param name="url" value="https://bob:pw5@api.example.com/x?y=1"/>`,
			contains: []string{`value="https://bob:********@api.example.com/x?y=1"`},
			absent:   []string{"pw5"},
		},
		{
			name:     "url in text",
			in:       `<a>mysql://u:pw6@db/fs</a>`,
			contains: []string{`<a>mysql://u:********@db/fs</a>`},
			absent:   []string{"pw6"},
		},
		{
			name:     "comments dropped",
			in:       `<a><!-- <param name="password" value="pw7"/> --><b/></a>`,
			absent:   []string{"pw7", "<!--"},
			contains: []string{"<b/>"},
		},
		{
			name:     "declaration and entities",
			in:       "<?xml version=\"1.0\" encoding=\"ISO-8859-1\"?>\n<c name=\"x\" v=\"a&amp;b &quot;q&quot; &lt;t&gt;\" w=\"bare & amp\"/>",
			contains: []string{`v="a&amp;b &#34;q&#34; &lt;t&gt;"`, `w="bare &amp; amp"`},
			absent:   []string{"<?xml"},
		},
		{
			name:     "case insensitive",
			in:       `<p name="SIP-PASSWORD" value="pw8"/>`,
			absent:   []string{"pw8"},
			contains: []string{`value="********"`},
		},
		{
			name:     "pass inside a longer word is not a secret",
			in:       `<s><param name="inbound-bypass-media" value="true"/><param name="pass-rfc2833" value="yes"/><param name="auth-pass" value="pw9"/><param name="passwd" value="pw10"/></s>`,
			contains: []string{`name="inbound-bypass-media" value="true"`, `name="pass-rfc2833" value="yes"`, `name="auth-pass" value="********"`, `name="passwd" value="********"`},
			absent:   []string{"pw9", "pw10"},
		},
		{
			name:     "data= inside value is not handled",
			in:       `<x cmd="set" data="default_password=1234"/>`,
			contains: []string{`data="default_password=1234"`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := MaskXML(tt.in)
			if err != nil {
				t.Fatal(err)
			}
			for _, s := range tt.contains {
				if !strings.Contains(out, s) {
					t.Errorf("missing %q in\n%s", s, out)
				}
			}
			for _, s := range tt.absent {
				if strings.Contains(out, s) {
					t.Errorf("leaked %q in\n%s", s, out)
				}
			}
			assertWellFormed(t, out)
		})
	}

	for _, bad := range []string{"<a><b></a>", "<a>", "<a></b>"} {
		if _, err := MaskXML(bad); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
}

func assertWellFormed(t *testing.T, s string) {
	t.Helper()
	d := xml.NewDecoder(strings.NewReader(s))
	for {
		_, err := d.Token()
		if err != nil {
			if err.Error() != "EOF" {
				t.Fatalf("output not well-formed: %v\n%s", err, s)
			}
			return
		}
	}
}

const configListReply = `<section name="configuration" description="Various Configuration">
  <configuration name="switch.conf" description="Core Configuration">
    <settings>
      <param name="core-db-dsn" value="secret-dsn"/>
      <configuration name="nested" description="not a child"/>
    </settings>
  </configuration>
  <configuration name="acl.conf" description="Network Lists">
    <network-lists/>
  </configuration>
  <configuration name="sofia.conf" description="sofia Endpoint"/>
</section>
`

func TestConfigList(t *testing.T) {
	list, err := ConfigList(context.Background(), reply("xml_locate configuration", configListReply))
	if err != nil {
		t.Fatal(err)
	}
	want := []ConfigInfo{
		{"acl.conf", "Network Lists"},
		{"sofia.conf", "sofia Endpoint"},
		{"switch.conf", "Core Configuration"},
	}
	if !reflect.DeepEqual(list, want) {
		t.Fatalf("got %v", list)
	}
	list, err = ConfigList(context.Background(), reply("xml_locate configuration", "can't find anything\n"))
	if err != nil || list == nil || len(list) != 0 {
		t.Fatalf("not found = %#v, %v", list, err)
	}
}

const sofiaConfReply = `<configuration name="sofia.conf" description="sofia Endpoint">
  <global_settings>
    <param name="log-level" value="0"/>
  </global_settings>
  <profiles>
    <profile name="external">
      <gateways>
        <gateway name="carrier">
          <param name="username" value="acct1"/>
          <param name="password" value="gwpass"/>
        </gateway>
      </gateways>
      <settings>
        <param name="odbc-dsn" value="fsdb:fs:dbpass"/>
        <param name="ext-rtp-ip" value="auto-nat"/>
      </settings>
    </profile>
  </profiles>
</configuration>`

func TestConfig(t *testing.T) {
	const cmd = "xml_locate configuration configuration name sofia.conf"
	out, err := Config(context.Background(), reply(cmd, sofiaConfReply), "sofia.conf")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"gwpass", "dbpass"} {
		if strings.Contains(out, s) {
			t.Errorf("leaked %s", s)
		}
	}
	if !strings.Contains(out, "\n        <gateway name=\"carrier\">\n") || !strings.Contains(out, `value="acct1"`) {
		t.Errorf("unexpected output:\n%s", out)
	}
	assertWellFormed(t, out)

	_, err = Config(context.Background(), reply("xml_locate configuration configuration name nope.conf", "can't find anything\n"), "nope.conf")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("not found: %v", err)
	}
}

func TestArgumentValidation(t *testing.T) {
	ctx := context.Background()
	bad := []string{"", "a b", "a;b", "a\nb", "a\rb", "x/y", "sofia.conf name y", "a\x00", "ä"}
	for _, arg := range bad {
		c := &fakeCommander{}
		if _, err := Config(ctx, c, arg); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("Config(%q): %v", arg, err)
		}
		if _, err := SofiaRescan(ctx, c, arg); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("SofiaRescan(%q): %v", arg, err)
		}
		if _, err := HangupChannel(ctx, c, arg); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("HangupChannel(%q): %v", arg, err)
		}
		if len(c.sent) != 0 {
			t.Errorf("%q: sent %v", arg, c.sent)
		}
	}
	if _, err := HangupChannel(ctx, &fakeCommander{}, "a.b"); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("uuid with dot: %v", err)
	}

	c := &fakeCommander{replies: map[string]string{
		"sofia profile internal-v6.x_1 rescan":                 "+OK scan complete\n",
		"uuid_kill 8a4c2e1f-0000-4b2a-9c3d-1234567890ab":       "+OK\n",
		"xml_locate configuration configuration name acl.conf": `<configuration name="acl.conf"/>`,
	}}
	if out, err := SofiaRescan(ctx, c, "internal-v6.x_1"); err != nil || out != "+OK scan complete" {
		t.Errorf("rescan %q %v", out, err)
	}
	if out, err := HangupChannel(ctx, c, "8a4c2e1f-0000-4b2a-9c3d-1234567890ab"); err != nil || out != "+OK" {
		t.Errorf("kill %q %v", out, err)
	}
	if _, err := Config(ctx, c, "acl.conf"); err != nil {
		t.Errorf("config %v", err)
	}
}

// Shaped like real xml_locate output: blank lines, explicit end tags.
const dialplanReply = `<section name="dialplan" description="Regex/XML Dialplan">

    <context name="public">
        <extension name="unloop">
            <condition field="${unroll_loops}" expression="^true$"></condition>
        </extension>
        <extension name="public_extensions">
            <condition field="destination_number" expression="^(10[01][0-9])$">
	        <action application="transfer" data="$1 XML default"></action>
      </condition>
    </extension>
    </context>

    <context name="default">
        <extension name="local">
            <condition field="destination_number" expression="^1000$">
	        <action application="bridge" data="user/1000"></action>
      </condition>
    </extension>
        <extension name="a"><condition><extension name="not a child"/></condition></extension>
        <extension name="b"/>
    </context>

    <context name="empty"></context>
    <other><extension name="outside"/></other>
</section>
`

func TestDialplanContexts(t *testing.T) {
	c := reply("xml_locate dialplan", dialplanReply)
	list, err := DialplanContexts(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	want := []SectionItem{{"default", 3}, {"empty", 0}, {"public", 2}}
	if !reflect.DeepEqual(list, want) {
		t.Fatalf("got %v", list)
	}
	if b, _ := json.Marshal(list[0]); string(b) != `{"name":"default","count":3}` {
		t.Errorf("json %s", b)
	}
	if !reflect.DeepEqual(c.sent, []string{"xml_locate dialplan"}) {
		t.Errorf("sent %q", c.sent)
	}
	list, err = DialplanContexts(context.Background(), reply("xml_locate dialplan", "can't find anything\n"))
	if err != nil || list == nil || len(list) != 0 {
		t.Fatalf("not found = %#v, %v", list, err)
	}
	if _, err := DialplanContexts(context.Background(), reply("xml_locate dialplan", "<section><context")); !errors.Is(err, ErrBadReply) {
		t.Errorf("truncated: %v", err)
	}
	var ce *CommandError
	if _, err := DialplanContexts(context.Background(), reply("xml_locate dialplan", "-ERR no\n")); !errors.As(err, &ce) {
		t.Errorf("-ERR: %v", err)
	}
}

// A vanilla 1000.xml / brian.xml user after preprocessing, with the
// commented-out a1-hash enabled, plus pointer entries in a second group.
const vanillaUser = `<user id="1000">
                <params>
                    <param name="password" value="JUkuSM3IzWjCLPqwLrn0"></param>
                    <param name="vm-password" value="9999"></param>
                    <param name="a1-hash" value="c6440e5de50b403206989679159de89a"></param>
    </params>
                <variables>
                    <variable name="toll_allow" value="domestic,international,local"></variable>
                    <variable name="accountcode" value="1000"></variable>
                    <variable name="user_context" value="default"></variable>
                    <variable name="effective_caller_id_name" value="Extension 1000"></variable>
    </variables>
  </user>`

const directoryReply = `<section name="directory" description="User Directory">
    <domain name="10.0.0.1">
        <params>
            <param name="dial-string" value="{^^:sip_invite_domain=${dialed_domain}:presence_id=${dialed_user}@${dialed_domain}}${sofia_contact(*/${dialed_user}@${dialed_domain})}"></param>
    </params>
        <variables>
            <variable name="record_stereo" value="true"></variable>
    </variables>
        <groups>
            <group name="default">
	        <users>
            ` + vanillaUser + `
            <user id="brian" cidr="192.0.2.0/24">
                <gateways></gateways>
                <params><param name="password" value="1234"></param></params>
            </user>
    </users>
  </group>
            <group name="sales">
	        <users>
            <user id="1000" type="pointer"></user>
            <user id="brian" type="pointer"></user>
    </users>
  </group>
    </groups>
  </domain>
    <domain name="alpha.example.com"></domain>
</section>
`

func TestDirectoryDomains(t *testing.T) {
	list, err := DirectoryDomains(context.Background(), reply("xml_locate directory", directoryReply))
	if err != nil {
		t.Fatal(err)
	}
	// Pointer entries are counted: four <user> elements, two distinct ids.
	want := []SectionItem{{"10.0.0.1", 4}, {"alpha.example.com", 0}}
	if !reflect.DeepEqual(list, want) {
		t.Fatalf("got %v", list)
	}
	list, err = DirectoryDomains(context.Background(), reply("xml_locate directory", "can't find anything\n"))
	if err != nil || list == nil || len(list) != 0 {
		t.Fatalf("not found = %#v, %v", list, err)
	}
}

func TestSectionEntry(t *testing.T) {
	ctx := context.Background()
	const dpCmd = "xml_locate dialplan context name default"
	c := reply(dpCmd, `<context name="default"><extension name="x"><condition field="destination_number" expression="^1000$"><action application="bridge" data="user/1000"/></condition></extension></context>`)
	out, err := DialplanContext(ctx, c, "default")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "\n  <extension name=\"x\">\n") || !strings.Contains(out, `<action application="bridge" data="user/1000"/>`) {
		t.Errorf("unexpected output:\n%s", out)
	}
	assertWellFormed(t, out)

	const dirCmd = "xml_locate directory domain name 10.0.0.1"
	out, err = DirectoryDomain(ctx, reply(dirCmd, "<domain name=\"10.0.0.1\"><groups><group name=\"default\"><users>"+vanillaUser+"</users></group></groups></domain>\n"), "10.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"JUkuSM3IzWjCLPqwLrn0", "9999", "c6440e5de50b403206989679159de89a"} {
		if strings.Contains(out, s) {
			t.Errorf("leaked %s in\n%s", s, out)
		}
	}
	for _, s := range []string{`<param name="password" value="********"/>`, `<param name="vm-password" value="********"/>`, `<param name="a1-hash" value="********"/>`, `<variable name="accountcode" value="1000"/>`} {
		if !strings.Contains(out, s) {
			t.Errorf("missing %s in\n%s", s, out)
		}
	}
	assertWellFormed(t, out)

	if _, err := DialplanContext(ctx, reply("xml_locate dialplan context name nope", "can't find anything\n"), "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("context not found: %v", err)
	}
	if _, err := DirectoryDomain(ctx, reply("xml_locate directory domain name nope", "can't find anything\n"), "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("domain not found: %v", err)
	}
	var ce *CommandError
	if _, err := DirectoryDomain(ctx, reply("xml_locate directory domain name x", "-ERR no\n"), "x"); !errors.As(err, &ce) {
		t.Errorf("-ERR: %v", err)
	}
	if _, err := DialplanContext(ctx, reply("xml_locate dialplan context name x", "<context><x></context>"), "x"); !errors.Is(err, ErrBadReply) {
		t.Errorf("bad xml: %v", err)
	}
	for _, arg := range []string{"", "a b", "default name x", "a;b", "a\nb", "x/y", "ä"} {
		c := &fakeCommander{}
		if _, err := DialplanContext(ctx, c, arg); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("DialplanContext(%q): %v", arg, err)
		}
		if _, err := DirectoryDomain(ctx, c, arg); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("DirectoryDomain(%q): %v", arg, err)
		}
		if len(c.sent) != 0 {
			t.Errorf("%q: sent %v", arg, c.sent)
		}
	}
}

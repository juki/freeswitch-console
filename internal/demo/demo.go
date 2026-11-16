// Package demo simulates FreeSWITCH switches for the demo mode of the
// console: a Switch answers the commands the console sends from simulated
// state, in the formats FreeSWITCH uses, and delivers heartbeats, hang-up
// events and log lines as the ESL client does. Files serves the embedded
// sample configuration the simulation is built from.
//
// A switch from Dial follows the wall clock; one from New stands still until
// Advance moves it, and with the same seed produces the same calls, so tests
// are deterministic. Every change a visitor makes is undone after two
// minutes, and at most 60 channels exist per switch, so a public demo heals
// itself and stays bounded however long it runs.
package demo

import (
	"net"
	"strconv"
	"time"

	"github.com/juki/freeswitch-console/internal/store"
)

const (
	// eslPort is the Event Socket port of every simulated switch.
	eslPort = 8021
	// maxChannels bounds the channels of one switch, whatever the visitors
	// originate.
	maxChannels = 60
	// healAfter is how long a change made by a visitor lasts.
	healAfter = 2 * time.Minute
	// heartbeatEvery is FreeSWITCH's default heartbeat interval.
	heartbeatEvery = 20 * time.Second
	// tickEvery is how often a live switch catches up with the wall clock.
	tickEvery = 250 * time.Millisecond
)

// spec describes one simulated switch.
type spec struct {
	name     string // instance name and host name
	ip       string // ESL and SIP address
	version  string
	down     string        // the connect error of a switch that never connects; "" for one that does
	uptime   time.Duration // at the start of the simulation
	since    int           // sessions since startup at the start
	sessions [2]int        // the range the number of channels is kept in
	gap      time.Duration // mean time between two new calls
	busy     int           // channels in progress at the start
	gateways []string      // names of gatewaySpecs on the external profile
	regs     []regSpec
	lan      string // prefix of the office phone addresses
}

// regSpec is one registered device.
type regSpec struct {
	user    string
	ip      string
	port    int
	proto   string // udp, tcp or wss
	agent   string
	expires int // seconds
}

// gatewaySpec is one carrier.
type gatewaySpec struct {
	name     string
	proxy    string
	ip       string
	username string
	register bool
	up       bool
	agent    string
}

var gatewaySpecs = []gatewaySpec{
	{name: "carrier-a", proxy: "sip.carrier-a.example", ip: "198.51.100.40", username: "pbx-demo-01", register: true, up: true, agent: "SBC-Edge/4.2"},
	{name: "carrier-b", proxy: "sip.carrier-b.example", ip: "198.51.100.50", username: "pbx-demo-02", register: false, up: true, agent: "CoreSwitch 11.0"},
	{name: "carrier-backup", proxy: "sip.carrier-backup.example", ip: "203.0.113.60", username: "pbx-demo-03", register: true, up: false},
}

// users are the directory users of the sample configuration, with their
// effective_caller_id_name; 1009 and 1010 have no device.
var users = []struct{ id, name string }{
	{"1001", "Reception"}, {"1002", "Front Office"}, {"1003", "Sales"}, {"1004", "Support 1"}, {"1005", "Support 2"},
	{"1006", "Support 3"}, {"1007", "Accounts"}, {"1008", "Warehouse"}, {"1009", "Meeting Room"}, {"1010", "Lobby"},
}

const (
	deskPhone = "DeskPhone DP-410 2.4.1"
	webPhone  = "WebPhone 1.8.0 (WebRTC)"
	softDial  = "SoftDial 5.2.3 (Windows)"
)

var specs = []*spec{
	{
		name: "pbx-east-1", ip: "192.0.2.11", version: "1.10.12",
		uptime: 12*24*time.Hour + 3*time.Hour + 17*time.Minute + 42*time.Second, since: 96418,
		sessions: [2]int{8, 40}, gap: 8 * time.Second, busy: 20,
		gateways: []string{"carrier-a", "carrier-b", "carrier-backup"},
		regs: []regSpec{
			{"1001", "10.20.1.21", 5060, "udp", deskPhone, 3600},
			{"1001", "198.51.100.81", 51734, "wss", webPhone, 600},
			{"1002", "10.20.1.22", 5060, "udp", deskPhone, 3600},
			{"1003", "10.20.1.23", 5060, "udp", "DeskPhone DP-610 2.4.1", 3600},
			{"1003", "203.0.113.83", 49152, "tcp", "MobileDial 2.9.4 (Android)", 1800},
			{"1004", "10.20.1.24", 5060, "udp", "DeskPhone DP-410 2.3.9", 3600},
			{"1004", "198.51.100.84", 50211, "wss", webPhone, 600},
			{"1005", "10.20.1.25", 5060, "udp", deskPhone, 3600},
			{"1005", "10.20.2.15", 5062, "tcp", softDial, 1800},
			{"1006", "10.20.1.26", 5060, "udp", deskPhone, 3600},
			{"1006", "203.0.113.86", 60412, "wss", "WebPhone 1.7.2 (WebRTC)", 600},
			{"1007", "10.20.2.17", 5062, "tcp", "SoftDial 5.2.1 (macOS)", 1800},
			{"1008", "10.20.1.28", 5060, "udp", "DeskPhone DP-210 1.9.0", 3600},
			{"1008", "203.0.113.88", 5060, "udp", "MobileDial 2.9.4 (iOS)", 1800},
		},
	},
	{
		name: "pbx-west-2", ip: "198.51.100.21", version: "1.11.3",
		uptime: 3*24*time.Hour + 6*time.Hour + 4*time.Minute + 9*time.Second, since: 2307,
		sessions: [2]int{0, 6}, gap: 70 * time.Second, busy: 2,
		gateways: []string{"carrier-a"},
		regs: []regSpec{
			{"1001", "10.20.5.21", 5060, "udp", deskPhone, 3600},
			{"1002", "10.20.5.22", 5060, "udp", deskPhone, 3600},
			{"1003", "198.51.100.93", 50877, "wss", webPhone, 600},
			{"1004", "10.20.5.24", 5060, "udp", deskPhone, 3600},
			{"1005", "10.20.6.15", 5062, "tcp", softDial, 1800},
			{"1006", "10.20.5.26", 5060, "udp", "DeskPhone DP-210 1.9.0", 3600},
		},
	},
	{name: "pbx-lab", ip: "203.0.113.30", down: "connection refused"},
}

// specAt returns the switch at the ESL address addr, or nil.
func specAt(addr string) *spec {
	for _, sp := range specs {
		if net.JoinHostPort(sp.ip, strconv.Itoa(eslPort)) == addr {
			return sp
		}
	}
	return nil
}

// Seed adds the simulated switches to st, each with the sample
// configuration as its config files.
func Seed(st *store.Store) error {
	for _, sp := range specs {
		if _, err := st.Add(store.Instance{Name: sp.name, Host: sp.ip, Port: eslPort, Password: "demo", ConfigMode: store.ConfigLocal, ConfigPath: "/etc/freeswitch"}); err != nil {
			return err
		}
	}
	return nil
}

// Dial returns the simulated switch at addr, live: it follows the wall
// clock until Close. An address that is not a simulated switch, like
// pbx-lab, refuses the connection. The password is not checked.
func Dial(addr, _ string) *Switch {
	now := time.Now()
	s := newSwitch(addr, now.UnixNano(), now)
	if s.state.Connected {
		s.clock = time.Now
		s.stop, s.done = make(chan struct{}), make(chan struct{})
		go s.run()
	}
	return s
}

// New returns the simulated switch at addr with its clock at start, standing
// still until Advance moves it. Two switches with the same address, seed and
// start behave alike.
func New(addr string, seed int64, start time.Time) *Switch {
	return newSwitch(addr, seed, start)
}

package server

import "fsdeck/internal/store"

// capability is what a route requires of the signed-in account.
type capability int

const (
	capPublic    capability = iota // no session needed
	capRead                        // any signed-in account: status, SIP, calls, config, history
	capLogs                        // log lines on the event stream
	capOperate                     // hang up, reloadxml, reloadacl, profile rescan, gateway register
	capControl                     // profile start/stop, gateway kill, modules, fsctl, global variables
	capConsole                     // any API command
	capInstances                   // instance create, update, delete
	capUsers                       // accounts and the audit log
	capFiles                       // config files: raw, with secrets in clear
)

// roleCaps holds the roles of the core; the operator role comes with the
// paid user management (ee_register.go).
var roleCaps = map[string]map[capability]bool{
	store.RoleViewer: {capRead: true},
	store.RoleAdmin:  {capRead: true, capLogs: true, capOperate: true, capControl: true, capConsole: true, capInstances: true, capUsers: true, capFiles: true},
}

// allowed reports whether role has c; an unknown role has nothing.
func allowed(role string, c capability) bool {
	return roleCaps[role][c]
}

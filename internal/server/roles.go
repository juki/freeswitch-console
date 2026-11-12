package server

import "fsdeck/internal/store"

// capability is what a route requires of the signed-in account.
type capability int

const (
	capPublic    capability = iota // no session needed
	capRead                        // any signed-in account: status, SIP, calls, config, history
	capLogs                        // log lines on the event stream
	capOperate                     // hang up, reloadxml, reloadacl, sofia rescan
	capConsole                     // any API command
	capInstances                   // instance create, update, delete
	capUsers                       // accounts and the audit log
)

var roleCaps = map[string]map[capability]bool{
	store.RoleViewer:   {capRead: true},
	store.RoleOperator: {capRead: true, capLogs: true, capOperate: true},
	store.RoleAdmin:    {capRead: true, capLogs: true, capOperate: true, capConsole: true, capInstances: true, capUsers: true},
}

// allowed reports whether role has c; an unknown role has nothing.
func allowed(role string, c capability) bool {
	return roleCaps[role][c]
}

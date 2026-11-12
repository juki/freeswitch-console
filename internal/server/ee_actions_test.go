//go:build !community

// Licensed under the fsdeck paid-feature terms, see ee/LICENSE.

package server

import (
	"testing"

	"fsdeck/internal/store"
)

// TestOperatorActions runs every operation as an operator: the table decides.
func TestOperatorActions(t *testing.T) {
	h := newHarness(t, licensed(t), noEnvAccounts)
	h.addUser("op", store.RoleOperator)
	tok := h.login("op", userPW)
	p := "/api/instances/" + h.id + "/actions"
	f := h.fake(0)
	for body, code := range map[string]int{
		`{"action":"reloadxml"}`:                                                            200,
		`{"action":"reloadacl"}`:                                                            200,
		`{"action":"sofia_rescan","profile":"internal"}`:                                    200,
		`{"action":"sofia_profile","profile":"internal","op":"rescan"}`:                     200,
		`{"action":"sofia_gateway","profile":"external","gateway":"gw1","op":"register"}`:   200,
		`{"action":"sofia_gateway","profile":"external","gateway":"gw1","op":"unregister"}`: 200,
		`{"action":"sofia_profile","profile":"internal","op":"restart"}`:                    403,
		`{"action":"sofia_profile","profile":"internal","op":"stop"}`:                       403,
		`{"action":"sofia_profile","profile":"internal","op":"start"}`:                      403,
		`{"action":"sofia_profile","profile":"internal","op":"flush_inbound_reg"}`:          403,
		`{"action":"sofia_gateway","profile":"external","gateway":"gw1","op":"kill"}`:       403,
		`{"action":"module","module":"mod_sofia","op":"reload"}`:                            403,
		`{"action":"fsctl","op":"pause"}`:                                                   403,
		`{"action":"setvar","name":"a","value":"b"}`:                                        403,
		`{"action":"sofia_gateway","profile":"bad name","gateway":"gw1","op":"kill"}`:       403, // refused before validation
	} {
		f.set(func(f *fakeSwitch) { f.sent = nil })
		r := h.do("POST", p, tok, body)
		expect(t, r, code, "")
		sent, _, _, _ := f.snapshot()
		if (len(sent) == 1) != (code == 200) {
			t.Errorf("%s sent %q", body, sent)
		}
	}
}

package provider

import "testing"

// TestAccountThisDeviceKept: `magpie provider this-device <id> yes` (and the
// GUI's toggle) must store thisDevice for a signed-in account too, not only
// for a plain provider (#7229): the flag is what keeps the provider on this
// computer out of sync, so a subscription signed in on one computer among
// several that sync silently lost it and the next sync switched it off.
func TestAccountThisDeviceKept(t *testing.T) {
	signIn(t)
	for _, id := range []string{"codex", "copilot"} {
		p, err := Find(id)
		if err != nil {
			t.Fatal(err)
		}
		if p.Account == nil {
			t.Fatalf("%s: not the signed-in account: %+v", id, *p)
		}
		if err := SetThisDevice(id, true); err != nil {
			t.Fatal(err)
		}
		stored, err := Stored()
		if err != nil {
			t.Fatal(err)
		}
		var kept bool
		for _, s := range stored {
			if s.ID == id && s.ThisDevice {
				kept = true
			}
		}
		if !kept {
			t.Errorf("%s: this-device not stored for a signed-in account", id)
		}
		// and back off again, so nothing sticks behind the test's back
		if err := SetThisDevice(id, false); err != nil {
			t.Fatal(err)
		}
		stored, err = Stored()
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range stored {
			if s.ID == id && s.ThisDevice {
				t.Errorf("%s: this-device left on after clearing", id)
			}
		}
	}
}

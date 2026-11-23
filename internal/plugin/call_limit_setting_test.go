package plugin

import (
	"errors"
	"testing"

	"github.com/yetone/magpie/internal/settings"
)

// #6976: the plugin host's call limit is settable (settings.json's
// pluginCallLimit): a host under a raised limit admits past the calls the
// stream budget's default allows, and one with no setting keeps the
// default. The chans are built at host start, so a lowered default proves
// the setting is read when they are.
func TestCallLimitFromSettings(t *testing.T) {
	oldWindow, oldBudget := streamWindow, streamBudget
	oldReserve := rpcReserve
	streamWindow, streamBudget, rpcReserve = 1<<20, 2<<20, 0 // default: two streaming calls
	t.Cleanup(func() { streamWindow, streamBudget, rpcReserve = oldWindow, oldBudget, oldReserve })

	t.Run("default", func(t *testing.T) {
		f := newFakeHost(t)
		for i := range 2 {
			if _, _, err := f.host.begin(true); err != nil {
				t.Fatalf("call %d: %v", i, err)
			}
		}
		if _, _, err := f.host.begin(true); !errors.Is(err, errTooManyCalls) {
			t.Fatalf("the call past the default = %v, want errTooManyCalls", err)
		}
	})

	t.Run("setting raises it", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("XDG_CONFIG_HOME", home)
		t.Setenv("XDG_CACHE_HOME", home)
		t.Setenv("HOME", home)
		t.Setenv("USERPROFILE", home)
		if err := settings.Save(settings.Settings{PluginCallLimit: 4}); err != nil {
			t.Fatal(err)
		}
		f := newFakeHost(t)
		for i := range 4 {
			if _, _, err := f.host.begin(true); err != nil {
				t.Fatalf("call %d refused: %v", i, err)
			}
		}
		if _, _, err := f.host.begin(true); !errors.Is(err, errTooManyCalls) {
			t.Fatalf("the call past the setting = %v, want errTooManyCalls", err)
		}
	})
}

// #6976: a raised call limit keeps the RPC reserve — a full stream set
// leaves the control path its slots, so a quota read (an RPC) is admitted
// where a stream would be refused.
func TestCallLimitKeepsTheRPCReserve(t *testing.T) {
	oldWindow, oldBudget := streamWindow, streamBudget
	oldReserve := rpcReserve
	streamWindow, streamBudget, rpcReserve = 1<<20, 2<<20, 1 // default: two streams, one RPC on top
	t.Cleanup(func() { streamWindow, streamBudget, rpcReserve = oldWindow, oldBudget, oldReserve })

	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("XDG_CACHE_HOME", home)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if err := settings.Save(settings.Settings{PluginCallLimit: 6}); err != nil {
		t.Fatal(err)
	}
	f := newFakeHost(t)
	// the raised limit: streams = 6-1 = 5, calls = 6
	for i := range 5 {
		if _, _, err := f.host.begin(true); err != nil {
			t.Fatalf("stream %d refused: %v", i, err)
		}
	}
	if _, _, err := f.host.begin(true); !errors.Is(err, errTooManyCalls) {
		t.Fatalf("the stream past 5 = %v, want errTooManyCalls", err)
	}
	// yet an RPC is admitted: the reserve is untouched
	if id, _, err := f.host.begin(false); err != nil {
		t.Fatalf("an RPC over a full stream set refused: %v", err)
	} else {
		f.host.calls[id].release()
	}
}

// #6976: a raised limit keeps the RPC reserve — with every stream slot
// held, an RPC (a quota read) is still admitted where another stream is
// refused.
func TestCallLimitRaisedKeepsRPCsOverFullStreams(t *testing.T) {
	oldWindow, oldBudget := streamWindow, streamBudget
	oldReserve := rpcReserve
	streamWindow, streamBudget, rpcReserve = 1<<20, 2<<20, 1
	t.Cleanup(func() { streamWindow, streamBudget, rpcReserve = oldWindow, oldBudget, oldReserve })

	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("XDG_CACHE_HOME", home)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if err := settings.Save(settings.Settings{PluginCallLimit: 128}); err != nil {
		t.Fatal(err)
	}
	f := newFakeHost(t)
	// streams = 128-1 = 127; saturate the stream set
	for i := range 127 {
		if _, _, err := f.host.begin(true); err != nil {
			t.Fatalf("stream %d refused: %v", i, err)
		}
	}
	if _, _, err := f.host.begin(true); !errors.Is(err, errTooManyCalls) {
		t.Fatalf("the stream past the raised limit = %v, want errTooManyCalls", err)
	}
	// yet an RPC is admitted: the reserve stayed its own
	if id, _, err := f.host.begin(false); err != nil {
		t.Fatalf("an RPC over a full stream set refused: %v", err)
	} else {
		f.host.calls[id].release()
	}
}

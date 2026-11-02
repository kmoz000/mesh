//go:build linux

package main

import "testing"

// TestParseTxPowerArgs exercises only the pure arg-parsing/validation helper.
// It must never call radio.GuardedTxPower, which touches real hardware.
func TestParseTxPowerArgs(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		got, err := parseTxPowerArgs([]string{"-iface", "wlan0", "-dbm", "20"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.iface != "wlan0" || got.dbm != 20 || got.revert {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("revert flag", func(t *testing.T) {
		got, err := parseTxPowerArgs([]string{"-iface", "wlan0", "-dbm", "0", "-revert"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !got.revert || got.dbm != 0 {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("missing iface", func(t *testing.T) {
		if _, err := parseTxPowerArgs([]string{"-dbm", "20"}); err == nil {
			t.Fatal("expected error for missing -iface")
		}
	})

	t.Run("missing dbm", func(t *testing.T) {
		if _, err := parseTxPowerArgs([]string{"-iface", "wlan0"}); err == nil {
			t.Fatal("expected error for missing -dbm")
		}
	})

	t.Run("dbm too high", func(t *testing.T) {
		if _, err := parseTxPowerArgs([]string{"-iface", "wlan0", "-dbm", "41"}); err == nil {
			t.Fatal("expected error for -dbm above range")
		}
	})

	t.Run("dbm negative", func(t *testing.T) {
		if _, err := parseTxPowerArgs([]string{"-iface", "wlan0", "-dbm", "-1"}); err == nil {
			t.Fatal("expected error for negative -dbm")
		}
	})

	t.Run("dbm boundaries", func(t *testing.T) {
		for _, v := range []string{"0", "40"} {
			if _, err := parseTxPowerArgs([]string{"-iface", "wlan0", "-dbm", v}); err != nil {
				t.Fatalf("boundary %s rejected: %v", v, err)
			}
		}
	})

	t.Run("unexpected positional", func(t *testing.T) {
		if _, err := parseTxPowerArgs([]string{"-iface", "wlan0", "-dbm", "20", "extra"}); err == nil {
			t.Fatal("expected error for positional argument")
		}
	})

	t.Run("bad flag", func(t *testing.T) {
		if _, err := parseTxPowerArgs([]string{"-iface", "wlan0", "-dbm", "notint"}); err == nil {
			t.Fatal("expected error for non-integer -dbm")
		}
	})
}

// TestParseChannelArgs exercises only the pure arg-parsing/validation helper.
// It must never call radio.GuardedChannelSwitch, which touches real hardware.
func TestParseChannelArgs(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		got, err := parseChannelArgs([]string{"-iface", "wlan0", "-channel", "36"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.iface != "wlan0" || got.channel != 36 || got.revert {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("revert flag", func(t *testing.T) {
		got, err := parseChannelArgs([]string{"-iface", "wlan0", "-channel", "6", "-revert"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !got.revert || got.channel != 6 {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("missing iface", func(t *testing.T) {
		if _, err := parseChannelArgs([]string{"-channel", "36"}); err == nil {
			t.Fatal("expected error for missing -iface")
		}
	})

	t.Run("missing channel", func(t *testing.T) {
		if _, err := parseChannelArgs([]string{"-iface", "wlan0"}); err == nil {
			t.Fatal("expected error for missing -channel")
		}
	})

	t.Run("channel too high", func(t *testing.T) {
		if _, err := parseChannelArgs([]string{"-iface", "wlan0", "-channel", "197"}); err == nil {
			t.Fatal("expected error for -channel above range")
		}
	})

	t.Run("channel too low", func(t *testing.T) {
		if _, err := parseChannelArgs([]string{"-iface", "wlan0", "-channel", "0"}); err == nil {
			t.Fatal("expected error for -channel below range")
		}
	})

	t.Run("channel negative", func(t *testing.T) {
		if _, err := parseChannelArgs([]string{"-iface", "wlan0", "-channel", "-1"}); err == nil {
			t.Fatal("expected error for negative -channel")
		}
	})

	t.Run("channel boundaries", func(t *testing.T) {
		for _, v := range []string{"1", "196"} {
			if _, err := parseChannelArgs([]string{"-iface", "wlan0", "-channel", v}); err != nil {
				t.Fatalf("boundary %s rejected: %v", v, err)
			}
		}
	})

	t.Run("unexpected positional", func(t *testing.T) {
		if _, err := parseChannelArgs([]string{"-iface", "wlan0", "-channel", "36", "extra"}); err == nil {
			t.Fatal("expected error for positional argument")
		}
	})

	t.Run("bad flag", func(t *testing.T) {
		if _, err := parseChannelArgs([]string{"-iface", "wlan0", "-channel", "notint"}); err == nil {
			t.Fatal("expected error for non-integer -channel")
		}
	})
}

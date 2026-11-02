//go:build linux

package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"

	"wantasticmesh/internal/radio"
)

// runRadio implements `wmeshd radio <show|txpower>`: a read-only radio
// enumerator and a guarded tx-power change. It is the CLI face of
// internal/radio; all the nl80211/sysfs work lives in that package.
func runRadio(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: wmeshd radio <show|txpower|channel>")
	}
	switch args[0] {
	case "show":
		return radioShow(args[1:])
	case "txpower":
		return radioTxPower(args[1:])
	case "channel":
		return radioChannel(args[1:])
	default:
		return fmt.Errorf("unknown radio subcommand %q (want show|txpower|channel)", args[0])
	}
}

// radioShow enumerates the node's radios and prints one line each. It is
// read-only. Telemetry is best-effort: if the netlink/sysfs probe cannot open
// at all it returns an error (non-zero exit); if a probe returns partial data
// with an error it prints what it has and still reports the error.
func radioShow(args []string) error {
	fs := flag.NewFlagSet("radio show", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: wmeshd radio show")
	}

	prober, err := radio.NewLinuxProber("/")
	if err != nil {
		return fmt.Errorf("radio telemetry unavailable (try as root): %w", err)
	}
	defer prober.Close()

	caps, probeErr := prober.Probe()
	if len(caps.Radios) == 0 {
		if probeErr != nil {
			return fmt.Errorf("probe radios: %w", probeErr)
		}
		return errors.New("no radios found")
	}

	for _, r := range caps.Radios {
		fmt.Println(formatRadio(r))
	}
	if probeErr != nil {
		// Some fields may be missing; surface the error but keep what we printed.
		return fmt.Errorf("probe incomplete: %w", probeErr)
	}
	return nil
}

// formatRadio renders one radio as a single stable line, honoring the Known
// bits: a field whose bit is unset prints "?" rather than a misleading zero.
func formatRadio(r radio.Radio) string {
	channel := "?"
	if r.Known&radio.KnownChannel != 0 {
		channel = strconv.FormatUint(uint64(r.CurChannel), 10)
	}
	width := "?"
	if r.Known&radio.KnownWidth != 0 {
		width = strconv.FormatUint(uint64(r.CurWidth), 10) + "MHz"
	}
	txpower := "?"
	if r.Known&radio.KnownTxPower != 0 {
		txpower = strconv.Itoa(int(r.CurTxPower)) + "dBm"
	}
	mld := "none"
	if r.MLDLink != radio.MLDLinkNone {
		mld = strconv.FormatUint(uint64(r.MLDLink), 10)
	}
	return fmt.Sprintf("radio index=%d phy=%s netdev=%s band=%s channel=%s width=%s txpower=%s mld-link=%s flags=%s",
		r.Index, orNone(r.Phy), orNone(r.Netdev), bandLabel(r.Band), channel, width, txpower, mld, radioFlags(r))
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// bandLabel maps the wmproto band code (0 unknown, 1=2.4, 2=5, 3=6) to a label.
func bandLabel(b radio.Band) string {
	switch b {
	case radio.Band24:
		return "2.4"
	case radio.Band5:
		return "5"
	case radio.Band6:
		return "6"
	default:
		return "?"
	}
}

// radioFlags renders the boolean role flags as a compact comma list.
func radioFlags(r radio.Radio) string {
	var flags []string
	if r.Backhaul {
		flags = append(flags, "backhaul")
	}
	if r.MLO {
		flags = append(flags, "mlo")
	}
	if len(flags) == 0 {
		return "-"
	}
	out := flags[0]
	for _, f := range flags[1:] {
		out += "," + f
	}
	return out
}

// txPowerArgs is the parsed, validated form of `radio txpower` flags.
type txPowerArgs struct {
	iface  string
	dbm    int
	revert bool
}

// parseTxPowerArgs parses and validates the `radio txpower` flags. It is a pure
// helper (no device access) so it can be unit-tested. It requires -iface and
// -dbm and bounds -dbm to a sane 0..40 dBm window.
func parseTxPowerArgs(args []string) (txPowerArgs, error) {
	fs := flag.NewFlagSet("radio txpower", flag.ContinueOnError)
	iface := fs.String("iface", "", "wireless interface to adjust (e.g. wlan0)")
	dbm := fs.Int("dbm", 0, "target tx power in whole dBm (0..40)")
	revert := fs.Bool("revert", false, "apply then restore the original power (a safe probe)")
	if err := fs.Parse(args); err != nil {
		return txPowerArgs{}, err
	}
	if fs.NArg() != 0 {
		return txPowerArgs{}, errors.New("unexpected positional arguments")
	}

	// flag.Int cannot tell "unset" from "set to the default", so track -dbm.
	dbmSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "dbm" {
			dbmSet = true
		}
	})
	if *iface == "" {
		return txPowerArgs{}, errors.New("-iface is required")
	}
	if !dbmSet {
		return txPowerArgs{}, errors.New("-dbm is required")
	}
	if *dbm < 0 || *dbm > 40 {
		return txPowerArgs{}, fmt.Errorf("-dbm %d out of range (want 0..40)", *dbm)
	}
	return txPowerArgs{iface: *iface, dbm: *dbm, revert: *revert}, nil
}

// radioTxPower performs a guarded tx-power change. It requires root because it
// opens netlink and changes radio state. With -revert it leaves the power
// unchanged (apply then restore); without it the new value stays.
func radioTxPower(args []string) error {
	parsed, err := parseTxPowerArgs(args)
	if err != nil {
		return err
	}
	if os.Geteuid() != 0 {
		return errors.New("radio txpower needs root (opens netlink and changes radio state)")
	}
	before, after, err := radio.GuardedTxPower(parsed.iface, parsed.dbm, parsed.revert)
	if err != nil {
		return fmt.Errorf("set tx power on %s: %w", parsed.iface, err)
	}
	fmt.Printf("before=%ddBm after=%ddBm\n", before, after)
	return nil
}

// channelArgs is the parsed, validated form of `radio channel` flags.
type channelArgs struct {
	iface   string
	channel int
	revert  bool
}

// parseChannelArgs parses and validates the `radio channel` flags. It is a pure
// helper (no device access) so it can be unit-tested. It requires -iface and
// -channel and bounds -channel to a sane 1..196 window.
func parseChannelArgs(args []string) (channelArgs, error) {
	fs := flag.NewFlagSet("radio channel", flag.ContinueOnError)
	iface := fs.String("iface", "", "wireless interface to switch (e.g. wlan0)")
	channel := fs.Int("channel", 0, "target channel number (1..196)")
	revert := fs.Bool("revert", false, "switch then restore the original channel (a safe probe)")
	if err := fs.Parse(args); err != nil {
		return channelArgs{}, err
	}
	if fs.NArg() != 0 {
		return channelArgs{}, errors.New("unexpected positional arguments")
	}

	// flag.Int cannot tell "unset" from "set to the default", so track -channel.
	channelSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "channel" {
			channelSet = true
		}
	})
	if *iface == "" {
		return channelArgs{}, errors.New("-iface is required")
	}
	if !channelSet {
		return channelArgs{}, errors.New("-channel is required")
	}
	if *channel < 1 || *channel > 196 {
		return channelArgs{}, fmt.Errorf("-channel %d out of range (want 1..196)", *channel)
	}
	return channelArgs{iface: *iface, channel: *channel, revert: *revert}, nil
}

// radioChannel performs a guarded channel switch. It requires root because it
// opens netlink and changes radio state. With -revert it leaves the channel
// unchanged (switch then restore); without it the new channel stays.
func radioChannel(args []string) error {
	parsed, err := parseChannelArgs(args)
	if err != nil {
		return err
	}
	if os.Geteuid() != 0 {
		return errors.New("radio channel needs root (opens netlink and changes radio state)")
	}
	before, after, err := radio.GuardedChannelSwitch(parsed.iface, parsed.channel, parsed.revert)
	if err != nil {
		return fmt.Errorf("switch channel on %s: %w", parsed.iface, err)
	}
	fmt.Printf("before=ch%d after=ch%d\n", before, after)
	return nil
}

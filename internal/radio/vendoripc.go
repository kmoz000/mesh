package radio

import (
	"errors"
	"strings"
)

// Vendor IPC trigger over the ezmesh control FIFOs. This is a thin one-way
// surface: open a named pipe for write and echo a text verb; the vendor stack
// does the radio-safe work (channel-preference computation, backhaul scan,
// topology optimisation). It is NOT a reconfiguration this layer owns, so a
// trigger is never a Change/Mechanism and has nothing to verify or roll back.
//
// Confirmed from inventory/artifacts/t2-b/ezmesh-cmd (a shell script, not an
// ELF): `echo $* > <pipe>` on one of two pipes, with the verb set below. The
// full verb set and any readback are PENDING device confirmation; only these
// four are proven from the script.

// Pipe selects which ezmesh FIFO a verb is written to.
type Pipe uint8

const (
	PipeCtrl  Pipe = iota // /var/run/ezmesh-ctrl-cmd.fifo  (controller side)
	PipeAgent             // /var/run/ezmesh-agent-cmd.fifo  (agent side)
)

func (p Pipe) String() string {
	if p == PipeAgent {
		return "agent"
	}
	return "ctrl"
}

// Path returns the FIFO path for the pipe. Kept here (not in the linux file) so
// the mapping is visible to tests.
func (p Pipe) Path() string {
	if p == PipeAgent {
		return "/var/run/ezmesh-agent-cmd.fifo"
	}
	return "/var/run/ezmesh-ctrl-cmd.fifo"
}

// Verb is one ezmesh command: its name, the pipe it belongs on, and whether it
// carries arguments.
type Verb struct {
	Name     string
	Pipe     Pipe
	WithArgs bool
}

// The four confirmed verbs. chanprefquery / topopt / peer_bss_info go to the
// controller pipe; bkscan to the agent pipe.
var (
	VerbChanPrefQuery = Verb{Name: "chanprefquery", Pipe: PipeCtrl}
	VerbTopOpt        = Verb{Name: "topopt", Pipe: PipeCtrl}
	VerbPeerBSSInfo   = Verb{Name: "peer_bss_info", Pipe: PipeCtrl, WithArgs: true}
	VerbBkScan        = Verb{Name: "bkscan", Pipe: PipeAgent}
)

// PipeWriter writes a verb line to a FIFO. The real implementation
// (vendoripc_linux.go) opens the pipe O_WRONLY|O_NONBLOCK and writes; tests
// supply a mock that records lines and opens no pipe.
type PipeWriter interface {
	// WriteVerb writes line (which the implementation terminates with a newline,
	// matching `echo`) to pipe p. It returns ErrVendorUnavailable when the pipe
	// is absent or has no reader.
	WriteVerb(p Pipe, line string) error
}

// Vendor IPC errors.
var (
	// ErrVendorBusy means a reconfiguration transaction is open; a trigger that
	// could make the vendor stack retune is refused so the two never run
	// concurrently.
	ErrVendorBusy = errors.New("radio: vendor trigger refused while a transaction is open")
	// ErrVendorUnavailable means the FIFO is not present or has no reader
	// (ezmesh not running). Non-blocking open surfaces this immediately instead
	// of hanging.
	ErrVendorUnavailable = errors.New("radio: ezmesh FIFO unavailable (not present or no reader)")
	// ErrVendorArgs means a verb was given args it does not accept, or omitted
	// required args.
	ErrVendorArgs = errors.New("radio: vendor verb arguments invalid")
)

// buildVerbLine assembles the text line for a verb and its args, matching the
// ezmesh-cmd script's `echo $*`. It is pure and tested.
func buildVerbLine(v Verb, args ...string) (string, error) {
	if len(args) > 0 && !v.WithArgs {
		return "", ErrVendorArgs
	}
	if len(args) == 0 {
		return v.Name, nil
	}
	// Reject arg tokens with whitespace/newline that would split the line or
	// forge a second verb.
	for _, a := range args {
		if a == "" || strings.ContainsAny(a, " \t\r\n") {
			return "", ErrVendorArgs
		}
	}
	return v.Name + " " + strings.Join(args, " "), nil
}

// VendorTrigger fires ezmesh verbs through a PipeWriter, gated by an optional
// Busy predicate that reports whether a reconfiguration transaction is open.
type VendorTrigger struct {
	W PipeWriter
	// Busy, if set, is consulted before every trigger; a true result refuses the
	// trigger with ErrVendorBusy. The daemon wires this to its transaction state.
	Busy func() bool
}

// Trigger writes a verb (with optional args) to its pipe. It refuses while a
// transaction is open.
func (t *VendorTrigger) Trigger(v Verb, args ...string) error {
	if t.Busy != nil && t.Busy() {
		return ErrVendorBusy
	}
	line, err := buildVerbLine(v, args...)
	if err != nil {
		return err
	}
	return t.W.WriteVerb(v.Pipe, line)
}

// ChanPrefQuery triggers a vendor channel-preference computation. BackhaulScan
// triggers a backhaul scan. TopologyOptimize triggers a topology pass. These
// are the intended entry points for the daemon.
func (t *VendorTrigger) ChanPrefQuery() error    { return t.Trigger(VerbChanPrefQuery) }
func (t *VendorTrigger) BackhaulScan() error     { return t.Trigger(VerbBkScan) }
func (t *VendorTrigger) TopologyOptimize() error { return t.Trigger(VerbTopOpt) }

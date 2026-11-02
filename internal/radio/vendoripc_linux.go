//go:build linux

package radio

import (
	"os"
	"syscall"
)

// FIFOWriter is the production PipeWriter: it opens an ezmesh control FIFO for
// write in non-blocking mode and writes a newline-terminated verb line, exactly
// as the ezmesh-cmd shell script does (`echo $* > <pipe>`).
//
// The open MUST be non-blocking: a blocking write-open on a FIFO with no reader
// blocks forever, so if ezmesh is not running the daemon would hang. O_NONBLOCK
// makes the open fail fast with ENXIO ("no such device or address") when there
// is no reader, which is mapped to ErrVendorUnavailable. DEVICE EXECUTION IS
// PENDING: this has not been run against a live ezmesh.
type FIFOWriter struct {
	// CtrlPath / AgentPath override the pipe paths (empty uses the defaults from
	// Pipe.Path). Overridable for a device whose paths differ.
	CtrlPath  string
	AgentPath string
}

func (f *FIFOWriter) path(p Pipe) string {
	if p == PipeAgent {
		if f.AgentPath != "" {
			return f.AgentPath
		}
		return PipeAgent.Path()
	}
	if f.CtrlPath != "" {
		return f.CtrlPath
	}
	return PipeCtrl.Path()
}

// WriteVerb opens the pipe non-blocking and writes "line\n". A missing pipe or a
// pipe with no reader both surface as ErrVendorUnavailable rather than a hang.
func (f *FIFOWriter) WriteVerb(p Pipe, line string) error {
	path := f.path(p)
	fd, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		// ENXIO: FIFO exists but no reader. ENOENT: not present. Either way the
		// vendor stack is not listening.
		if os.IsNotExist(err) {
			return ErrVendorUnavailable
		}
		if pe, ok := err.(*os.PathError); ok && pe.Err == syscall.ENXIO {
			return ErrVendorUnavailable
		}
		return err
	}
	defer fd.Close()
	if _, err := fd.WriteString(line + "\n"); err != nil {
		return err
	}
	return nil
}

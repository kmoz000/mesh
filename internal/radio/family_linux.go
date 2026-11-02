//go:build linux

package radio

// LinuxFamilyResolver resolves a generic-netlink family id by name over a real
// socket with CTRL_CMD_GETFAMILY. It is the production FamilyResolver; wrap it
// in a CachedResolver so each family is resolved once per device. No id is ever
// hardcoded. DEVICE EXECUTION IS PENDING.
type LinuxFamilyResolver struct {
	conn *genlConn
}

// NewLinuxFamilyResolver opens a socket for resolution. Close releases it.
func NewLinuxFamilyResolver() (*LinuxFamilyResolver, error) {
	c, err := openGenl()
	if err != nil {
		return nil, err
	}
	return &LinuxFamilyResolver{conn: c}, nil
}

// Close releases the socket.
func (r *LinuxFamilyResolver) Close() error {
	if r == nil || r.conn == nil {
		return nil
	}
	return r.conn.Close()
}

// Resolve issues CTRL_CMD_GETFAMILY for name and parses the reply. A family that
// is not present (no reply / error) surfaces as ErrFamilyNotFound.
func (r *LinuxFamilyResolver) Resolve(name string) (FamilyInfo, error) {
	var fi FamilyInfo
	var got bool
	scratch := make([]byte, 1024)
	err := r.conn.execute(false,
		func(seq uint32) ([]byte, error) {
			return buildGetFamily(scratch, name, seq, r.conn.pid)
		},
		func(_ uint8, attrs []byte) error {
			info, perr := parseGetFamilyReply(attrs)
			if perr != nil {
				return perr
			}
			fi = info
			got = true
			return nil
		},
	)
	if err != nil {
		return FamilyInfo{}, ErrFamilyNotFound
	}
	if !got || fi.ID == 0 {
		return FamilyInfo{}, ErrFamilyNotFound
	}
	return fi, nil
}

package ieee1905

import "errors"

var (
	ErrShortFrame       = errors.New("short frame")
	ErrWrongEtherType   = errors.New("not an IEEE 1905 frame")
	ErrMissingEndTLV    = errors.New("missing end-of-message TLV")
	ErrTLVLength        = errors.New("TLV length exceeds frame")
	ErrEndTLVLength     = errors.New("invalid end-of-message TLV length")
	ErrTooManyTLVs      = errors.New("too many TLVs")
	ErrUnexpectedEndTLV = errors.New("end-of-message TLV in non-final fragment")
)

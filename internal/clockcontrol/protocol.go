package clockcontrol

import (
	"encoding/binary"
	"errors"
	"math"
	"time"
)

const (
	SourceNRF             = "nrf"
	SourceGPS             = "gps"
	SourceUser            = "user"
	SourceNetworkHoldover = "network-holdover"
)

// Request uses CLOCK_BOOTTIME nanoseconds for receipt and age fields.
type Request struct {
	Op             string `json:"op"`
	BootID         string `json:"boot_id,omitempty"`
	Generation     uint64 `json:"generation,omitempty"`
	Source         string `json:"source,omitempty"`
	UTCUnixNS      int64  `json:"utc_unix_ns,omitempty"`
	ReceiptBootNS  int64  `json:"receipt_boot_ns,omitempty"`
	DeadlineBootNS int64  `json:"deadline_boot_ns,omitempty"`
	SampleAgeNS    int64  `json:"sample_age_ns,omitempty"`
	UncertaintyNS  int64  `json:"uncertainty_ns,omitempty"`
}

type Quality struct {
	BootID             string  `json:"boot_id"`
	Generation         uint64  `json:"generation"`
	Episode            string  `json:"episode"`
	Outcome            string  `json:"outcome"`
	Preparing          bool    `json:"preparing"`
	NRFEligible        bool    `json:"nrf_eligible"`
	NRFDeadlineBootNS  int64   `json:"nrf_deadline_boot_ns"`
	Source             string  `json:"source"`
	External           bool    `json:"external"`
	AgeNS              int64   `json:"age_ns"`
	ResidualSeconds    float64 `json:"residual_seconds"`
	UncertaintySeconds float64 `json:"uncertainty_seconds"`
}
type Response struct {
	OK      bool    `json:"ok"`
	Error   string  `json:"error,omitempty"`
	Quality Quality `json:"quality"`
}

// EncodeSOCK encodes a chrony SOCK sample with a 32- or 64-bit timeval.
func EncodeSOCK(t time.Time, offset float64, wordBytes int, order binary.ByteOrder) ([]byte, error) {
	if math.IsNaN(offset) || math.IsInf(offset, 0) {
		return nil, errors.New("invalid offset")
	}
	sec, usec := t.Unix(), int64(t.Nanosecond()/1000)
	var b []byte
	var off int
	switch wordBytes {
	case 4:
		if sec < math.MinInt32 || sec > math.MaxInt32 {
			return nil, errors.New("timeval overflow")
		}
		b = make([]byte, 32)
		order.PutUint32(b[0:4], uint32(int32(sec)))
		order.PutUint32(b[4:8], uint32(usec))
		off = 8
	case 8:
		b = make([]byte, 40)
		order.PutUint64(b[0:8], uint64(sec))
		order.PutUint64(b[8:16], uint64(usec))
		off = 16
	default:
		return nil, errors.New("unsupported timeval width")
	}
	order.PutUint64(b[off:off+8], math.Float64bits(offset))
	order.PutUint32(b[off+20:off+24], 0x534f434b)
	return b, nil
}

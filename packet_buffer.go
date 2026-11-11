package astits

import (
	"bufio"
	"errors"
	"fmt"
	"io"

	"github.com/asticode/go-astikit"
)

// ErrCannotResync is returned when no trustworthy sync position can be found
// within the configured scan and buffer limits
var ErrCannotResync = errors.New("astits: cannot resync")

// packetBuffer represents a packet buffer
type packetBuffer struct {
	leftover             []byte // Bytes read ahead while resyncing, waiting to be consumed
	packetSize           int
	resynced             bool // Whether a resync happened since this flag was last cleared
	resyncMaxBufferBytes int
	resyncMaxScanBytes   int
	s                    PacketSkipper
	r                    io.Reader
	packetReadBuffer     []byte
}

// newPacketBuffer creates a new packet buffer
func newPacketBuffer(r io.Reader, packetSize int, s PacketSkipper, resyncMaxScanBytes, resyncMaxBufferBytes int) (pb *packetBuffer, err error) {
	// Init
	pb = &packetBuffer{
		packetSize:           packetSize,
		resyncMaxBufferBytes: resyncMaxBufferBytes,
		resyncMaxScanBytes:   resyncMaxScanBytes,
		s:                    s,
		r:                    r,
	}

	// Packet size is not set
	if pb.packetSize == 0 {
		// Auto detect packet size
		if pb.packetSize, err = autoDetectPacketSize(r); err != nil {
			err = fmt.Errorf("astits: auto detecting packet size failed: %w", err)
			return
		}
	}
	return
}

// autoDetectPacketSize updates the packet size based on the first bytes
// Minimum packet size is 188 and is bounded by 2 sync bytes
// Assumption is made that the first byte of the reader is a sync byte
func autoDetectPacketSize(r io.Reader) (packetSize int, err error) {
	// Read first bytes
	const l = 193
	var b = make([]byte, l)
	shouldRewind, rerr := peek(r, b)
	if rerr != nil {
		err = fmt.Errorf("astits: reading first %d bytes failed: %w", l, rerr)
		return
	}

	// Packet must start with a sync byte
	if b[0] != syncByte {
		err = ErrPacketMustStartWithASyncByte
		return
	}

	// Look for sync bytes
	for idx, b := range b {
		if b == syncByte && idx >= MpegTsPacketSize {
			// Update packet size
			packetSize = idx

			if !shouldRewind {
				return
			}

			// Rewind or sync reader
			var n int64
			if n, err = rewind(r); err != nil {
				err = fmt.Errorf("astits: rewinding failed: %w", err)
				return
			} else if n == -1 {
				var ls = packetSize - (l - packetSize)
				if _, err = r.Read(make([]byte, ls)); err != nil {
					err = fmt.Errorf("astits: reading %d bytes to sync reader failed: %w", ls, err)
					return
				}
			}
			return
		}
	}
	err = fmt.Errorf("astits: only one sync byte detected in first %d bytes", l)
	return
}

// bufio.Reader can't be rewinded, which leads to packet loss on packet size autodetection
// but it has handy Peek() method
// so what we do here is peeking bytes for bufio.Reader and falling back to rewinding/syncing for all other readers
func peek(r io.Reader, b []byte) (shouldRewind bool, err error) {
	if br, ok := r.(*bufio.Reader); ok {
		var bs []byte
		bs, err = br.Peek(len(b))
		if err != nil {
			return
		}
		copy(b, bs)
		return false, nil
	}

	_, err = r.Read(b)
	shouldRewind = true
	return
}

// rewind rewinds the reader if possible, otherwise n = -1
func rewind(r io.Reader) (n int64, err error) {
	if s, ok := r.(io.Seeker); ok {
		if n, err = s.Seek(0, 0); err != nil {
			err = fmt.Errorf("astits: seeking to 0 failed: %w", err)
			return
		}
		return
	}
	n = -1
	return
}

// next fetches the next packet from the buffer
func (pb *packetBuffer) next() (p *Packet, err error) {
	// Read
	if pb.packetReadBuffer == nil || len(pb.packetReadBuffer) != pb.packetSize {
		pb.packetReadBuffer = make([]byte, pb.packetSize)
	}

	// Loop to make sure we return a packet even if first packets are skipped
	for p == nil {
		if err = pb.fill(); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				err = ErrNoMorePackets
			} else {
				err = fmt.Errorf("astits: reading %d bytes failed: %w", pb.packetSize, err)
			}
			return
		}

		// The packet boundary is lost
		if pb.canResync() && pb.packetReadBuffer[0] != syncByte {
			if err = pb.resync(); err != nil {
				return
			}
		}

		// Parse packet
		if p, err = parsePacket(astikit.NewBytesIterator(pb.packetReadBuffer), pb.s); err != nil {
			if !errors.Is(err, errSkippedPacket) {
				err = fmt.Errorf("astits: building packet failed: %w", err)
				return
			}
		}
	}

	return
}

// fill fills the packet read buffer, consuming leftover bytes first
func (pb *packetBuffer) fill() (err error) {
	b := pb.packetReadBuffer
	for len(b) > 0 && len(pb.leftover) > 0 {
		n := copy(b, pb.leftover)
		pb.leftover = pb.leftover[n:]
		b = b[n:]
	}
	if len(pb.leftover) == 0 {
		pb.leftover = nil
	}
	if len(b) > 0 {
		_, err = io.ReadFull(pb.r, b)
	}
	return
}

// canResync checks whether resyncing is enabled
func (pb *packetBuffer) canResync() bool {
	return pb.resyncMaxScanBytes > 0 && pb.resyncMaxBufferBytes > 0
}

// resync looks for a trustworthy sync position within the configured scan and buffer
// limits and realigns the read buffer on it. A sync position is considered trustworthy
// when a sync byte is followed by another sync byte exactly one packet away, so that
// random sync bytes inside payloads are not mistaken for packet headers.
func (pb *packetBuffer) resync() (err error) {
	// The read buffer holds misaligned bytes
	buf := append([]byte(nil), pb.packetReadBuffer...)

	// Loop until a trustworthy sync position is found or a limit is reached
	scanned := 0
	for {
		// Look for a trustworthy sync position
		for scanned+pb.packetSize < len(buf) {
			if buf[scanned] == syncByte && buf[scanned+pb.packetSize] == syncByte {
				// Realign the read buffer and keep the remaining bytes for next reads
				copy(pb.packetReadBuffer, buf[scanned:scanned+pb.packetSize])
				pb.leftover = append([]byte(nil), buf[scanned+pb.packetSize:]...)
				pb.resynced = true
				return
			}
			scanned++
			if scanned >= pb.resyncMaxScanBytes {
				err = fmt.Errorf("astits: scanned %d bytes without finding a trustworthy sync position: %w", scanned, ErrCannotResync)
				return
			}
		}

		// Check buffer limit
		if len(buf) >= pb.resyncMaxBufferBytes {
			err = fmt.Errorf("astits: buffered %d bytes without finding a trustworthy sync position: %w", len(buf), ErrCannotResync)
			return
		}

		// Read more bytes
		l := pb.packetSize
		if max := pb.resyncMaxBufferBytes - len(buf); max < l {
			l = max
		}
		b := make([]byte, l)
		var n int
		if n, err = pb.r.Read(b); n > 0 {
			buf = append(buf, b[:n]...)
		}
		if err != nil {
			if err == io.EOF {
				err = ErrNoMorePackets
			} else {
				err = fmt.Errorf("astits: reading bytes while resyncing failed: %w", err)
			}
			return
		}
		if n == 0 {
			err = fmt.Errorf("astits: reading bytes while resyncing failed: %w", io.ErrNoProgress)
			return
		}
	}
}

package astits

import (
	"bytes"
	"errors"
	"testing"

	"github.com/asticode/go-astikit"
	"github.com/stretchr/testify/assert"
)

// resyncPacket builds a 188 bytes packet with the given payload, padded with zeros
func resyncPacket(h PacketHeader, payload []byte) (b []byte, p *Packet) {
	// packetHeaderBytes hardcodes those values
	h.TransportPriority = true
	h.TransportScramblingControl = ScramblingControlScrambledWithEvenKey
	b, _ = packetShort(h, nil)
	copy(b[4:], payload)
	padded := make([]byte, MpegTsPacketSize-4)
	copy(padded, payload)
	p = &Packet{Header: h, Payload: padded}
	return
}

func resyncTestStream() (b1, b2, b3 []byte, p1, p2, p3 *Packet) {
	b1, p1 = resyncPacket(PacketHeader{ContinuityCounter: 0, HasPayload: true, PID: 256}, []byte("payload1"))
	b2, p2 = resyncPacket(PacketHeader{ContinuityCounter: 1, HasPayload: true, PID: 256}, []byte("payload2"))
	b3, p3 = resyncPacket(PacketHeader{ContinuityCounter: 2, HasPayload: true, PID: 256}, []byte("payload3"))
	return
}

func TestPacketBufferResync(t *testing.T) {
	b1, b2, b3, p1, p2, p3 := resyncTestStream()

	// 37 garbage bytes are inserted between the first and second packets. One of
	// them is a sync byte to make sure random sync bytes are not mistaken for
	// packet headers
	garbage := make([]byte, 37)
	garbage[1] = syncByte
	var stream []byte
	stream = append(stream, b1...)
	stream = append(stream, garbage...)
	stream = append(stream, b2...)
	stream = append(stream, b3...)

	// Resync disabled
	pb, err := newPacketBuffer(bytes.NewReader(stream), MpegTsPacketSize, nil, 0, 0)
	assert.NoError(t, err)
	p, err := pb.next()
	assert.NoError(t, err)
	assert.Equal(t, p1, p)
	_, err = pb.next()
	assert.True(t, errors.Is(err, ErrPacketMustStartWithASyncByte))

	// Resync enabled
	pb, err = newPacketBuffer(bytes.NewReader(stream), MpegTsPacketSize, nil, 1024, 2048)
	assert.NoError(t, err)
	p, err = pb.next()
	assert.NoError(t, err)
	assert.Equal(t, p1, p)
	assert.False(t, pb.resynced)
	p, err = pb.next()
	assert.NoError(t, err)
	assert.Equal(t, p2, p)
	assert.True(t, pb.resynced)
	p, err = pb.next()
	assert.NoError(t, err)
	assert.Equal(t, p3, p)
	_, err = pb.next()
	assert.EqualError(t, err, ErrNoMorePackets.Error())
}

func TestPacketBufferResyncLimits(t *testing.T) {
	b1, b2, b3, _, _, _ := resyncTestStream()

	// Scan limit
	garbage := make([]byte, 100)
	var stream []byte
	stream = append(stream, b1...)
	stream = append(stream, garbage...)
	stream = append(stream, b2...)
	stream = append(stream, b3...)
	pb, err := newPacketBuffer(bytes.NewReader(stream), MpegTsPacketSize, nil, 50, 2048)
	assert.NoError(t, err)
	_, err = pb.next()
	assert.NoError(t, err)
	_, err = pb.next()
	assert.True(t, errors.Is(err, ErrCannotResync))

	// Buffer limit
	garbage = make([]byte, 37)
	stream = append([]byte{}, b1...)
	stream = append(stream, garbage...)
	stream = append(stream, b2...)
	stream = append(stream, b3...)
	pb, err = newPacketBuffer(bytes.NewReader(stream), MpegTsPacketSize, nil, 1024, 200)
	assert.NoError(t, err)
	_, err = pb.next()
	assert.NoError(t, err)
	_, err = pb.next()
	assert.True(t, errors.Is(err, ErrCannotResync))

	// EOF while resyncing
	garbage = make([]byte, 200)
	stream = append([]byte{}, b1...)
	stream = append(stream, garbage...)
	pb, err = newPacketBuffer(bytes.NewReader(stream), MpegTsPacketSize, nil, 1024, 2048)
	assert.NoError(t, err)
	_, err = pb.next()
	assert.NoError(t, err)
	_, err = pb.next()
	assert.EqualError(t, err, ErrNoMorePackets.Error())
}

func TestPacketBufferResyncPacketSizes(t *testing.T) {
	for _, packetSize := range []int{192, 204} {
		// Build packets made of a sync byte, extra bytes and a 187 bytes tail
		mk := func(cc uint8, payload string) (b []byte, p *Packet) {
			var b188 []byte
			b188, p = resyncPacket(PacketHeader{ContinuityCounter: cc, HasPayload: true, PID: 256}, []byte(payload))
			b = append([]byte{syncByte}, make([]byte, packetSize-MpegTsPacketSize)...)
			b = append(b, b188[1:]...)
			return
		}
		b1, p1 := mk(0, "payload1")
		b2, p2 := mk(1, "payload2")
		b3, p3 := mk(2, "payload3")

		// 41 garbage bytes are inserted between the first and second packets
		garbage := make([]byte, 41)
		garbage[5] = syncByte
		var stream []byte
		stream = append(stream, b1...)
		stream = append(stream, garbage...)
		stream = append(stream, b2...)
		stream = append(stream, b3...)

		pb, err := newPacketBuffer(bytes.NewReader(stream), packetSize, nil, 1024, 2048)
		assert.NoError(t, err)
		p, err := pb.next()
		assert.NoError(t, err)
		assert.Equal(t, p1, p)
		p, err = pb.next()
		assert.NoError(t, err)
		assert.Equal(t, p2, p)
		assert.True(t, pb.resynced)
		p, err = pb.next()
		assert.NoError(t, err)
		assert.Equal(t, p3, p)
		_, err = pb.next()
		assert.EqualError(t, err, ErrNoMorePackets.Error())
	}
}

func TestAutoDetectPacketSize(t *testing.T) {
	// Packet should start with a sync byte
	buf := &bytes.Buffer{}
	w := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: buf})
	w.Write(uint8(2))
	w.Write(byte(syncByte))
	_, err := autoDetectPacketSize(bytes.NewReader(buf.Bytes()))
	assert.EqualError(t, err, ErrPacketMustStartWithASyncByte.Error())

	// Valid packet size
	buf.Reset()
	w.Write(byte(syncByte))
	w.Write(make([]byte, 20))
	w.Write(byte(syncByte))
	w.Write(make([]byte, 166))
	w.Write(byte(syncByte))
	w.Write(make([]byte, 187))
	w.Write([]byte("test"))
	r := bytes.NewReader(buf.Bytes())
	p, err := autoDetectPacketSize(r)
	assert.NoError(t, err)
	assert.Equal(t, MpegTsPacketSize, p)
	assert.Equal(t, 380, r.Len())
}

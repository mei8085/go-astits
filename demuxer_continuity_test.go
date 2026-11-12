package astits

import (
	"bytes"
	"context"
	"testing"

	"github.com/asticode/go-astikit"
	"github.com/stretchr/testify/assert"
)

// buildPESPayloadForContinuityTest builds a bounded PES payload: start code,
// stream id, packet length, minimal optional header, then data.
func buildPESPayloadForContinuityTest(streamID uint8, data []byte) (payload []byte) {
	l := 3 + len(data)
	payload = []byte{0x00, 0x00, 0x01, streamID, byte(l >> 8), byte(l), 0x80, 0x00, 0x00}
	payload = append(payload, data...)
	return
}

// The packet pool is where the continuity counter / discontinuity contract is
// enforced: allowed counter resets (adaptation-field-only packets, explicit
// discontinuity indicators, retransmitted duplicates) must be tolerated while
// unexplained counter gaps (real data corruption) must silently drop the
// buffered fragment so it can never be mistaken for valid data.
func TestPacketPoolContinuityDiscontinuityContract(t *testing.T) {
	pkt := func(cc uint8, pusi bool, payload []byte) *Packet {
		return &Packet{
			Header: PacketHeader{
				ContinuityCounter:         cc,
				HasPayload:                true,
				PayloadUnitStartIndicator: pusi,
				PID:                       0x100,
			},
			Payload: payload,
		}
	}
	pktWithAF := func(cc uint8, pusi bool, discontinuityIndicator bool, payload []byte) *Packet {
		p := pkt(cc, pusi, payload)
		p.Header.HasAdaptationField = true
		p.AdaptationField = &PacketAdaptationField{DiscontinuityIndicator: discontinuityIndicator}
		return p
	}
	concat := func(ps []*Packet) (payload []byte) {
		for _, p := range ps {
			payload = append(payload, p.Payload...)
		}
		return
	}

	pp := newPacketPool(nil)

	// A packet carrying only an adaptation field is ignored by the pool: it does
	// not increment the continuity counter and must not disturb the reassembly
	// chain, even if it carries a discontinuity indicator.
	afOnly := &Packet{
		Header:          PacketHeader{ContinuityCounter: 0, HasAdaptationField: true, PID: 0x100},
		AdaptationField: &PacketAdaptationField{DiscontinuityIndicator: true},
	}
	assert.Empty(t, pp.addUnlocked(afOnly))

	// PES #1 spans 2 packets, the first one being retransmitted (duplicate).
	pes1 := buildPESPayloadForContinuityTest(0xe0, bytes.Repeat([]byte{0xa1}, 10))
	p1 := pkt(0, true, pes1[:14])
	p1dup := pkt(0, true, pes1[:14])
	p2 := pkt(1, false, pes1[14:])
	assert.Empty(t, pp.addUnlocked(p1))
	assert.Empty(t, pp.addUnlocked(p1dup), "a duplicate packet must not be delivered on its own")
	ps := pp.addUnlocked(p2)
	assert.Len(t, ps, 2)
	assert.Equal(t, pes1, concat(ps), "PES #1 must be reassembled exactly once despite the duplicate")

	// PES #2 spans 4 packets, the 3rd one is lost (continuity counter 4 never
	// shows up). The unexplained gap is real data corruption: the buffered
	// fragment must be dropped silently and never flushed.
	pes2 := buildPESPayloadForContinuityTest(0xe0, bytes.Repeat([]byte{0xb2}, 46))
	assert.Empty(t, pp.addUnlocked(pkt(2, true, pes2[:14])))
	assert.Empty(t, pp.addUnlocked(pkt(3, false, pes2[14:29])))
	// Packet with continuity counter 4 is lost.
	assert.Empty(t, pp.addUnlocked(pkt(5, false, pes2[29:44])), "the fragment queued after the gap must not be delivered")

	// PES #3 starts right after the corrupted fragment. Its start packet also
	// exhibits a counter jump (6 is skipped), which drops the leftover fragment
	// of PES #2. Inside PES #3 the counter reset (8 is skipped) is explicitly
	// signaled with a discontinuity indicator: this is an allowed reset and the
	// data must be preserved.
	pes3 := buildPESPayloadForContinuityTest(0xe0, append(bytes.Repeat([]byte{0xc3}, 5), bytes.Repeat([]byte{0xc4}, 5)...))
	assert.Empty(t, pp.addUnlocked(pkt(7, true, pes3[:14])))
	ps = pp.addUnlocked(pktWithAF(9, false, true, pes3[14:]))
	assert.Len(t, ps, 2)
	assert.Equal(t, pes3, concat(ps), "PES #3 must be delivered complete: the discontinuity indicator allows the counter reset")

	// PES #4 is complete within a single packet and must be delivered
	// independently right after the previous errors.
	pes4 := buildPESPayloadForContinuityTest(0xe0, bytes.Repeat([]byte{0xa4}, 10))
	ps = pp.addUnlocked(pkt(10, true, pes4))
	assert.Len(t, ps, 1)
	assert.Equal(t, pes4, concat(ps))

	// No corrupted fragment is left behind in the pool.
	assert.Empty(t, pp.dumpUnlocked())
}

// End to end contract: a transport stream mixing a lost packet, a duplicate
// packet, an adaptation-field-only packet and an explicit discontinuity
// indicator must only yield the intact PES packets. The corrupted PES must
// never be reported as valid audio/video data, and the first complete PES
// after the error must still be delivered on its own.
func TestDemuxerNextDataContinuityDiscontinuity(t *testing.T) {
	const pid = 0x100

	// PES payloads are crafted so that each TS packet is exactly filled with PES
	// bytes (no stuffing in the middle of a PES):
	//   - without adaptation field a packet holds 184 payload bytes
	//   - with a minimal adaptation field (length + flags) it holds 182 bytes
	pes1Data := bytes.Repeat([]byte{0xa1}, 359) // 175 + 184
	pes1 := buildPESPayloadForContinuityTest(0xe0, pes1Data)
	pes2 := buildPESPayloadForContinuityTest(0xe0, bytes.Repeat([]byte{0xb2}, 460))
	pes3Data := append(bytes.Repeat([]byte{0xc3}, 175), bytes.Repeat([]byte{0xc4}, 182)...)
	pes3 := buildPESPayloadForContinuityTest(0xe0, pes3Data)
	pes4Data := bytes.Repeat([]byte{0xa4}, 359)
	pes4 := buildPESPayloadForContinuityTest(0xe0, pes4Data)

	buf := &bytes.Buffer{}
	w := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: buf})
	write := func(cc uint8, pusi bool, af *PacketAdaptationField, payload []byte) {
		_, err := writePacket(w, &Packet{
			AdaptationField: af,
			Header: PacketHeader{
				ContinuityCounter:         cc,
				HasAdaptationField:        af != nil,
				HasPayload:                payload != nil,
				PayloadUnitStartIndicator: pusi,
				PID:                       pid,
			},
			Payload: payload,
		}, MpegTsPacketSize)
		assert.NoError(t, err)
	}

	// PES #1: complete, continuity counters 0 and 1.
	write(0, true, nil, pes1[:184])
	write(1, false, nil, pes1[184:])
	// PES #2: the packet with continuity counter 4 is lost. The remaining
	// fragment is real data corruption and must be dropped.
	write(2, true, nil, pes2[:184])
	write(3, false, nil, pes2[184:368])
	write(5, false, nil, pes2[368:])
	// Adaptation-field-only packet: it does not increment the continuity
	// counter and must be ignored by the demuxer.
	write(6, false, &PacketAdaptationField{StuffingLength: 10}, nil)
	// PES #3: starts on counter 7 (the unexplained jump drops the corrupted
	// fragment of PES #2), then the reset skipping counter 8 is explicitly
	// signaled with a discontinuity indicator and must be tolerated.
	write(7, true, nil, pes3[:184])
	write(9, false, &PacketAdaptationField{DiscontinuityIndicator: true}, pes3[184:])
	// PES #4: its first packet is retransmitted (duplicate on counter 10).
	write(10, true, nil, pes4[:184])
	write(10, true, nil, pes4[:184])
	write(11, false, nil, pes4[184:])

	dmx := NewDemuxer(context.Background(), bytes.NewReader(buf.Bytes()), DemuxerOptPacketSize(MpegTsPacketSize))

	// PES #1 is delivered intact.
	d, err := dmx.NextData()
	assert.NoError(t, err)
	assert.NotNil(t, d.PES)
	assert.Equal(t, uint16(pid), d.PID)
	assert.Equal(t, uint8(0xe0), d.PES.Header.StreamID)
	assert.Equal(t, pes1Data, d.PES.Data)
	assert.Equal(t, uint8(0), d.FirstPacket.Header.ContinuityCounter)

	// PES #2 is corrupted and must never be delivered: the next data is PES #3,
	// the first complete PES after the error, delivered independently.
	d, err = dmx.NextData()
	assert.NoError(t, err)
	assert.NotNil(t, d.PES)
	assert.Equal(t, pes3Data, d.PES.Data, "both sides of the allowed counter reset must be reassembled")
	assert.Equal(t, uint8(7), d.FirstPacket.Header.ContinuityCounter)

	// PES #4 is delivered intact, its payload is not duplicated.
	d, err = dmx.NextData()
	assert.NoError(t, err)
	assert.NotNil(t, d.PES)
	assert.Equal(t, pes4Data, d.PES.Data)
	assert.Equal(t, uint8(10), d.FirstPacket.Header.ContinuityCounter)

	// Nothing else is left: the corrupted PES #2 fragment was not misreported
	// as valid audio/video data.
	_, err = dmx.NextData()
	assert.EqualError(t, err, ErrNoMorePackets.Error())
}

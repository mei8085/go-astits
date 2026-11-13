package astits

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestHasDiscontinuity(t *testing.T) {
	assert.False(t, hasDiscontinuity([]*Packet{{Header: PacketHeader{ContinuityCounter: 15}}}, &Packet{Header: PacketHeader{ContinuityCounter: 0, HasPayload: true}}))
	assert.False(t, hasDiscontinuity([]*Packet{{Header: PacketHeader{ContinuityCounter: 15}}}, &Packet{Header: PacketHeader{ContinuityCounter: 15}}))
	assert.True(t, hasDiscontinuity([]*Packet{{Header: PacketHeader{ContinuityCounter: 15}}}, &Packet{AdaptationField: &PacketAdaptationField{DiscontinuityIndicator: true}, Header: PacketHeader{ContinuityCounter: 0, HasAdaptationField: true, HasPayload: true}}))
	assert.False(t, hasDiscontinuity([]*Packet{{Header: PacketHeader{ContinuityCounter: 15}}}, &Packet{Header: PacketHeader{PID: PIDNull}}))
	assert.False(t, hasDiscontinuity([]*Packet{}, &Packet{Header: PacketHeader{ContinuityCounter: 5}}))
	assert.True(t, hasDiscontinuity([]*Packet{{Header: PacketHeader{ContinuityCounter: 15}}}, &Packet{Header: PacketHeader{ContinuityCounter: 1, HasPayload: true}}))
	assert.True(t, hasDiscontinuity([]*Packet{{Header: PacketHeader{ContinuityCounter: 15}}}, &Packet{Header: PacketHeader{ContinuityCounter: 0}}))
}

func TestIsSameAsPrevious(t *testing.T) {
	assert.False(t, isSameAsPrevious([]*Packet{{Header: PacketHeader{ContinuityCounter: 1}}}, &Packet{Header: PacketHeader{ContinuityCounter: 1}}))
	assert.False(t, isSameAsPrevious([]*Packet{{Header: PacketHeader{ContinuityCounter: 1}}}, &Packet{Header: PacketHeader{ContinuityCounter: 2, HasPayload: true}}))
	assert.True(t, isSameAsPrevious([]*Packet{{Header: PacketHeader{ContinuityCounter: 1}}}, &Packet{Header: PacketHeader{ContinuityCounter: 1, HasPayload: true}}))
}

func TestPacketPool(t *testing.T) {
	b := newPacketPool(nil)
	ps := b.addUnlocked(&Packet{Header: PacketHeader{ContinuityCounter: 0, HasPayload: true, PID: 1}})
	assert.Len(t, ps, 0)
	ps = b.addUnlocked(&Packet{Header: PacketHeader{ContinuityCounter: 1, HasPayload: true, PayloadUnitStartIndicator: true, PID: 1}})
	assert.Len(t, ps, 1)
	ps = b.addUnlocked(&Packet{Header: PacketHeader{ContinuityCounter: 1, HasPayload: true, PayloadUnitStartIndicator: true, PID: 2}})
	assert.Len(t, ps, 0)
	ps = b.addUnlocked(&Packet{Header: PacketHeader{ContinuityCounter: 2, HasPayload: true, PID: 1}})
	assert.Len(t, ps, 0)
	ps = b.addUnlocked(&Packet{Header: PacketHeader{ContinuityCounter: 3, HasPayload: true, PayloadUnitStartIndicator: true, PID: 1}})
	assert.Len(t, ps, 2)
	ps = b.addUnlocked(&Packet{Header: PacketHeader{ContinuityCounter: 4, HasPayload: true, PID: 1}})
	assert.Len(t, ps, 0)
	ps = b.addUnlocked(&Packet{Header: PacketHeader{ContinuityCounter: 5, HasPayload: true, PayloadUnitStartIndicator: true, PID: 1}})
	assert.Len(t, ps, 2)
	ps = b.addUnlocked(&Packet{Header: PacketHeader{ContinuityCounter: 6, HasPayload: true, PID: 1}})
	assert.Len(t, ps, 0)
	ps = b.dumpUnlocked()
	assert.Len(t, ps, 0)
	ps = b.dumpUnlocked()
	assert.Len(t, ps, 0)
	ps = b.dumpUnlocked()
	assert.Len(t, ps, 0)

	stats := b.Stats()
	assert.Equal(t, 2, stats.DroppedPayloadsByReason[PacketPoolDropReasonEndOfInput])
}

func TestPacketPoolWithRarePackets(t *testing.T) {
	payloadDVBTeletext := hexToBytes(`000001bd00b2848024293c972af5ffffffffffffffffffffffffffffffff
	ffffffffffffffffffffffffffffff10032cf5e4a8a80b0ba80ba80b2692
	040404040404040404040404040404040404040404040404040404040404
	0404032cd5e4a8a85757a8a8a80b26a80404040404040404040404040404
	040404040404040404040404040404040404ff2cffffffffffffffffffff
	ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff
	ffffffff`)
	b := newPacketPool(nil)
	ps := b.addUnlocked(&Packet{Header: PacketHeader{ContinuityCounter: 0, HasPayload: true, PayloadUnitStartIndicator: true, PID: 1004}, Payload: payloadDVBTeletext})
	assert.Len(t, ps, 1)
	ps = b.addUnlocked(&Packet{Header: PacketHeader{ContinuityCounter: 1, HasPayload: true, PayloadUnitStartIndicator: true, PID: 1004}, Payload: payloadDVBTeletext})
	assert.Len(t, ps, 1)
	ps = b.addUnlocked(&Packet{Header: PacketHeader{ContinuityCounter: 2, HasPayload: true, PayloadUnitStartIndicator: true, PID: 1004}, Payload: payloadDVBTeletext})
	assert.Len(t, ps, 1)
	ps = b.addUnlocked(&Packet{Header: PacketHeader{ContinuityCounter: 3, HasPayload: true, PayloadUnitStartIndicator: true, PID: 1004}, Payload: payloadDVBTeletext})
	assert.Len(t, ps, 1)
	ps = b.addUnlocked(&Packet{Header: PacketHeader{ContinuityCounter: 3, HasPayload: true, PayloadUnitStartIndicator: true, PID: 1004}, Payload: payloadDVBTeletext})
	assert.Len(t, ps, 1)
	ps = b.addUnlocked(&Packet{Header: PacketHeader{ContinuityCounter: 4, HasPayload: true, PayloadUnitStartIndicator: true, PID: 1004}, Payload: payloadDVBTeletext})
	assert.Len(t, ps, 1)
	ps = b.addUnlocked(&Packet{Header: PacketHeader{ContinuityCounter: 6, HasPayload: true, PayloadUnitStartIndicator: true, PID: 1004}, Payload: payloadDVBTeletext})
	assert.Len(t, ps, 1)
	ps = b.addUnlocked(&Packet{Header: PacketHeader{ContinuityCounter: 7, HasPayload: true, PayloadUnitStartIndicator: true, PID: 1004}, Payload: payloadDVBTeletext})
	assert.Len(t, ps, 1)
	ps = b.addUnlocked(&Packet{Header: PacketHeader{ContinuityCounter: 7, HasPayload: true, PayloadUnitStartIndicator: true, PID: 1004}, Payload: payloadDVBTeletext})
	assert.Len(t, ps, 1)
	ps = b.addUnlocked(&Packet{Header: PacketHeader{ContinuityCounter: 9, HasPayload: true, PayloadUnitStartIndicator: true, PID: 1004}, Payload: payloadDVBTeletext})
	assert.Len(t, ps, 1)
	ps = b.addUnlocked(&Packet{Header: PacketHeader{ContinuityCounter: 10, HasPayload: true, PayloadUnitStartIndicator: true, PID: 1004}, Payload: payloadDVBTeletext})
	assert.Len(t, ps, 1)
	ps = b.dumpUnlocked()
	assert.Len(t, ps, 0)
}

// An unbounded PES (packet length 0, e.g. a large video access unit written by
// GStreamer's mpegtsmux) must not be dropped when the PES that starts right
// after it is complete within that single packet.
func TestPacketPoolUnboundedPESFollowedByOnePacketPES(t *testing.T) {
	// pesPacketPayload builds a PES payload: start code, stream id, packet length
	// (0 = unbounded), minimal optional header, then n data bytes.
	pesPacketPayload := func(packetLength uint16, n int) []byte {
		p := []byte{0, 0, 1, 0xe0, byte(packetLength >> 8), byte(packetLength), 0x80, 0x00, 0x00}
		return append(p, make([]byte, n)...)
	}

	packet := func(cc uint8, pusi bool, payload []byte) *Packet {
		return &Packet{
			Header: PacketHeader{
				ContinuityCounter:         cc,
				HasPayload:                true,
				PID:                       256,
				PayloadUnitStartIndicator: pusi,
			},
			Payload: payload,
		}
	}

	pp := newPacketPool(nil)

	// Unbounded PES spanning two packets.
	p1 := packet(0, true, pesPacketPayload(0, 175))
	p2 := packet(1, false, make([]byte, 184))
	// PES that is complete within one packet: length covers optional header + data.
	p3 := packet(2, true, pesPacketPayload(3+16, 16))
	// Next payload start, flushes whatever is still queued.
	p4 := packet(3, true, pesPacketPayload(0, 175))

	assert.Empty(t, pp.addUnlocked(p1))
	assert.Empty(t, pp.addUnlocked(p2))

	ps := pp.addUnlocked(p3)
	assert.Equal(t, []*Packet{p1, p2}, ps, "the unbounded PES must be returned, not dropped")

	ps = pp.addUnlocked(p4)
	assert.Equal(t, []*Packet{p3}, ps, "the one-packet PES must be returned by the next flush")
}

func TestPacketPoolBoundedPESCompletesWithoutNextStart(t *testing.T) {
	b := newPacketPool(nil)
	packets := []*Packet{
		{Header: PacketHeader{PID: 256, ContinuityCounter: 0, HasPayload: true, PayloadUnitStartIndicator: true}, Payload: []byte{0, 0, 1, 0xe0, 0}},
		{Header: PacketHeader{PID: 256, ContinuityCounter: 1, HasPayload: true}, Payload: []byte{5, 0x80, 0, 0, 42}},
		{Header: PacketHeader{PID: 256, ContinuityCounter: 2, HasPayload: true}, Payload: []byte{43}},
	}
	assert.Empty(t, b.addUnlocked(packets[0]))
	assert.Empty(t, b.addUnlocked(packets[1]))
	assert.Equal(t, packets, b.addUnlocked(packets[2]))
	assert.Empty(t, b.dumpUnlocked())
}

func TestPacketPoolBoundedPESWithTrailingBytes(t *testing.T) {
	pp := newPacketPool(nil)
	first := testPESPacket(256, 0, true, 7, 3)
	trailing := &Packet{
		Header: PacketHeader{
			ContinuityCounter: 1,
			HasPayload:        true,
			PID:               256,
		},
		Payload: []byte{0xff},
	}
	next := testPESPacket(256, 2, true, 7, 3)

	assert.Empty(t, pp.addUnlocked(first))
	assert.Equal(t, []*Packet{first, trailing}, pp.addUnlocked(trailing))
	assert.Empty(t, pp.addUnlocked(next))
}

func TestPacketPoolReclaimsInactivePIDWithoutBlockingOthers(t *testing.T) {
	pp := newPacketPoolWithLimits(nil, nil, 2, 3)

	stalledStart := testPESPacket(256, 0, true, 0, 10)
	stalledContinue := testPESPacket(256, 1, false, 0, 10)
	otherStart := testPESPacket(257, 0, true, 3, 0)
	otherNext := testPESPacket(257, 1, true, 3, 0)
	activeStart := testPESPacket(258, 0, true, 0, 10)
	activeContinue := testPESPacket(258, 1, false, 0, 10)

	assert.Empty(t, pp.addUnlocked(stalledStart))
	assert.Empty(t, pp.addUnlocked(stalledContinue))

	// Packets on another PID remain deliverable while the stalled PID is aging.
	assert.Equal(t, []*Packet{otherStart}, pp.addUnlocked(otherStart))
	assert.Equal(t, []*Packet{otherNext}, pp.addUnlocked(otherNext))
	assert.Empty(t, pp.addUnlocked(activeStart))
	assert.Empty(t, pp.addUnlocked(activeContinue))

	stats := pp.Stats()
	assert.Equal(t, 2, stats.BufferedPackets)
	assert.Equal(t, 1, stats.DroppedPayloadsByReason[PacketPoolDropReasonInactivity])
	assert.Equal(t, 2, stats.DroppedPackets)
	assert.Equal(t, 38, stats.DroppedBytes)
	assert.Equal(t, 3, stats.HighWaterMarkPackets)
	assert.Equal(t, map[uint32]int{258: 2}, stats.BufferedPacketsPerPID)
	assert.Equal(t, map[uint32]int{258: 38}, stats.BufferedBytesPerPID)
}

func TestPacketPoolCanDisableInactivityReclamation(t *testing.T) {
	pp := newPacketPoolWithLimits(nil, nil, -1, 1)
	pp.addUnlocked(testPESPacket(256, 0, true, 0, 10))
	pp.addUnlocked(testPESPacket(257, 0, true, 0, 10))
	for i := 1; i < 10; i++ {
		pp.addUnlocked(testPESPacket(257, uint8(i), true, 3, 0))
	}

	stats := pp.Stats()
	assert.Empty(t, stats.DroppedPayloadsByReason)
	assert.Equal(t, 2, stats.BufferedPackets)
}

func TestPacketPoolKeepsActiveLongUnboundedPES(t *testing.T) {
	pp := newPacketPoolWithLimits(nil, nil, 1, 4)

	videoStart := testPESPacket(256, 0, true, 0, 100)
	videoContinue := testPESPacket(256, 1, false, 0, 100)
	otherStart := testPESPacket(257, 0, true, 0, 11)
	otherContinue := testPESPacket(257, 1, false, 0, 11)

	assert.Empty(t, pp.addUnlocked(videoStart))
	assert.Empty(t, pp.addUnlocked(videoContinue))
	pp.addUnlocked(testPESPacket(258, 0, true, 6, 0))

	// The video PID is still active, so it is eligible to exceed the watermark.
	assert.Empty(t, pp.addUnlocked(otherStart))
	assert.Empty(t, pp.addUnlocked(otherContinue))

	stats := pp.Stats()
	assert.Equal(t, 5, stats.BufferedPackets)
	assert.Empty(t, stats.DroppedPayloadsByReason)
	assert.Equal(t, 5, stats.HighWaterMarkPackets)

	nextVideo := testPESPacket(256, 2, true, 0, 100)
	assert.Equal(t, []*Packet{videoStart, videoContinue}, pp.addUnlocked(nextVideo))
}

func TestPacketPoolPSIDoesNotWaitForStalledPES(t *testing.T) {
	pp := newPacketPool(nil)

	pesStart := testPESPacket(256, 0, true, 0, 10)
	pesContinue := testPESPacket(256, 1, false, 0, 10)
	psi := &Packet{
		Header: PacketHeader{
			ContinuityCounter:         0,
			HasPayload:                true,
			PID:                       PIDPAT,
			PayloadUnitStartIndicator: true,
		},
		Payload: []byte{0, 0x00, 0x00, 0x00, 0xff, 0xff},
	}

	assert.Empty(t, pp.addUnlocked(pesStart))
	assert.Empty(t, pp.addUnlocked(pesContinue))
	assert.Equal(t, []*Packet{psi}, pp.addUnlocked(psi))

	stats := pp.Stats()
	assert.Equal(t, 2, stats.BufferedPackets)
	assert.Empty(t, stats.DroppedPayloadsByReason)
}

func TestPacketPoolDropsIncompletePSIOnNextSection(t *testing.T) {
	pp := newPacketPool(nil)
	first := &Packet{
		Header: PacketHeader{
			ContinuityCounter:         0,
			HasPayload:                true,
			PID:                       PIDPAT,
			PayloadUnitStartIndicator: true,
		},
		Payload: []byte{0, 0x00, 0x00, 0x01},
	}
	next := &Packet{
		Header: PacketHeader{
			ContinuityCounter:         1,
			HasPayload:                true,
			PID:                       PIDPAT,
			PayloadUnitStartIndicator: true,
		},
		Payload: []byte{0, 0x00, 0x00, 0x00, 0xff, 0xff},
	}

	assert.Empty(t, pp.addUnlocked(first))
	assert.Empty(t, pp.addUnlocked(next))

	stats := pp.Stats()
	assert.Equal(t, 1, stats.DroppedPayloadsByReason[PacketPoolDropReasonNewPayloadUnit])
	assert.Equal(t, 1, stats.DroppedPackets)
	assert.Equal(t, 1, stats.BufferedPackets)
}

func TestPacketPoolDiscardsIncompleteUnitsAtRecoveryAndEOF(t *testing.T) {
	pp := newPacketPool(nil)

	start := testPESPacket(256, 0, true, 50, 11)
	continued := testPESPacket(256, 1, false, 50, 11)

	// A CC gap without a payload start cannot continue the old PES.
	recovery := testPESPacket(256, 5, false, 50, 11)
	recovery.Header.HasAdaptationField = true
	recovery.AdaptationField = &PacketAdaptationField{DiscontinuityIndicator: true}
	assert.Empty(t, pp.addUnlocked(start))
	assert.Empty(t, pp.addUnlocked(continued))
	assert.Empty(t, pp.addUnlocked(recovery))

	stats := pp.Stats()
	assert.Equal(t, 2, stats.DroppedPayloadsByReason[PacketPoolDropReasonTransportDiscontinuity])
	assert.Equal(t, 3, stats.DroppedPackets)
	assert.Equal(t, 0, stats.BufferedPackets)

	// A continuation that is not a replacement payload start is also discarded.
	orphan := testPESPacket(256, 5, false, 50, 11)
	assert.Empty(t, pp.addUnlocked(orphan))
	assert.Equal(t, 3, pp.Stats().DroppedPayloads)
	assert.Equal(t, 4, pp.Stats().DroppedPackets)
	assert.Equal(t, 0, pp.Stats().BufferedPackets)

	// A replacement start followed by EOF before its declared length is also dropped.
	replacementStart := testPESPacket(256, 6, true, 50, 11)
	replacementContinue := testPESPacket(256, 7, false, 50, 11)
	assert.Empty(t, pp.addUnlocked(replacementStart))
	assert.Empty(t, pp.addUnlocked(replacementContinue))
	assert.Empty(t, pp.dumpUnlocked())

	stats = pp.Stats()
	assert.Equal(t, 1, stats.DroppedPayloadsByReason[PacketPoolDropReasonEndOfInput])
	assert.Equal(t, 6, stats.DroppedPackets)
	assert.Equal(t, 0, stats.BufferedPackets)
}

func TestPacketPoolDropsPIDOnStreamSwitch(t *testing.T) {
	pp := newPacketPool(nil)
	stalledStart := testPESPacket(256, 0, true, 0, 10)
	stalledContinue := testPESPacket(256, 1, false, 0, 10)
	other := testPESPacket(257, 0, true, 0, 10)

	assert.Empty(t, pp.addUnlocked(stalledStart))
	assert.Empty(t, pp.addUnlocked(stalledContinue))
	assert.Empty(t, pp.addUnlocked(other))

	pp.dropPIDUnlocked(256, PacketPoolDropReasonStreamSwitch)

	stats := pp.Stats()
	assert.Equal(t, 1, stats.BufferedPackets)
	assert.Equal(t, 1, stats.DroppedPayloadsByReason[PacketPoolDropReasonStreamSwitch])
	assert.Equal(t, 2, stats.DroppedPackets)
	assert.Equal(t, map[uint32]int{257: 1}, stats.BufferedPacketsPerPID)
}

func testPESPacket(pid uint16, cc uint8, pusi bool, packetLength uint16, dataLen int) *Packet {
	payload := []byte{0, 0, 1, 0xe0, byte(packetLength >> 8), byte(packetLength), 0x80, 0, 0}
	payload = append(payload, make([]byte, dataLen)...)
	return &Packet{
		Header: PacketHeader{
			ContinuityCounter:         cc,
			HasPayload:                true,
			PID:                       pid,
			PayloadUnitStartIndicator: pusi,
		},
		Payload: payload,
	}
}

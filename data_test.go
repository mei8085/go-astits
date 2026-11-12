package astits

import (
	"bytes"
	"testing"

	"github.com/asticode/go-astikit"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseData(t *testing.T) {
	// Init
	pm := newProgramMap()
	esm := newElementaryStreamMap()
	ps := []*Packet{}

	// Custom parser
	cds := []*DemuxerData{{PID: 1}}
	var c = func(ps []*Packet) (o []*DemuxerData, skip bool, err error) {
		o = cds
		skip = true
		return
	}
	ds, err := parseData(ps, c, pm, esm)
	assert.NoError(t, err)
	assert.Equal(t, cds, ds)

	// Do nothing for CAT
	ps = []*Packet{{Header: PacketHeader{PID: PIDCAT}}}
	ds, err = parseData(ps, nil, pm, esm)
	assert.NoError(t, err)
	assert.Empty(t, ds)

	// PES
	p := pesWithHeaderBytes()
	ps = []*Packet{
		{
			Header:  PacketHeader{PID: uint16(256)},
			Payload: p[:33],
		},
		{
			Header:  PacketHeader{PID: uint16(256)},
			Payload: p[33:],
		},
	}
	ds, err = parseData(ps, nil, pm, esm)
	assert.NoError(t, err)
	assert.Equal(t, []*DemuxerData{
		{
			FirstPacket: &Packet{Header: ps[0].Header, AdaptationField: ps[0].AdaptationField},
			PES:         pesWithHeader(),
			PID:         uint16(256),
		}}, ds)

	// PSI
	pm.setUnlocked(uint16(256), uint16(1))
	p = psiBytes()
	ps = []*Packet{
		{
			Header:  PacketHeader{PID: uint16(256)},
			Payload: p[:33],
		},
		{
			Header:  PacketHeader{PID: uint16(256)},
			Payload: p[33:],
		},
	}
	ds, err = parseData(ps, nil, pm, esm)
	assert.NoError(t, err)
	assert.Equal(t, psi.toData(
		&Packet{Header: ps[0].Header, AdaptationField: ps[0].AdaptationField},
		uint16(256),
	), ds)
}

func TestParseDataReturnedBytesOutliveNextParse(t *testing.T) {
	pm := newProgramMap()
	esm := newElementaryStreamMap()
	pm.setUnlocked(uint16(256), uint16(1))

	// A complete bounded PES. Parsing another unit reuses the temporary aggregate
	// buffer, but PES.Data must remain owned by the returned PESData.
	pes := []byte{0, 0, 1, 0xe0, 0, 4, 0x80, 0, 0, 42}
	pesPackets := []*Packet{
		{Header: PacketHeader{PID: 257}, Payload: pes[:4]},
		{Header: PacketHeader{PID: 257}, Payload: pes[4:]},
	}
	pesData, err := parseData(pesPackets, nil, pm, esm)
	assert.NoError(t, err)
	require.Len(t, pesData, 1)

	// Reuse the parser and its temporary payload pool with a PSI section.
	psiPayload := psiBytes()
	psiPackets := []*Packet{
		{Header: PacketHeader{PID: 256}, Payload: psiPayload[:73]},
		{Header: PacketHeader{PID: 256}, Payload: psiPayload[73:]},
	}
	_, err = parseData(psiPackets, nil, pm, esm)
	assert.NoError(t, err)
	assert.Equal(t, []byte{42}, pesData[0].PES.Data)

	// A second PSI parse must leave the previously delivered program tables intact.
	psiData, err := parseData(psiPackets, nil, pm, esm)
	assert.NoError(t, err)
	_, err = parseData(pesPackets, nil, pm, esm)
	assert.NoError(t, err)
	assert.Equal(t, psi.toData(&Packet{Header: psiPackets[0].Header}, 256), psiData)
}

func TestIsPSIPayload(t *testing.T) {
	pm := newProgramMap()
	esm := newElementaryStreamMap()
	var pids []int
	for i := 0; i <= 255; i++ {
		if isPSIPayload(uint16(i), pm, esm) {
			pids = append(pids, i)
		}
	}
	assert.Equal(t, []int{0, 16, 17, 18, 19, 20, 30, 31}, pids)
	pm.setUnlocked(uint16(1), uint16(0))
	assert.True(t, isPSIPayload(uint16(1), pm, esm))

	esm.setUnlocked(uint16(256), uint16(1))
	assert.False(t, isPSIPayload(uint16(256), pm, esm))
	esm.setUnlocked(uint16(0x11), uint16(1))
	assert.False(t, isPSIPayload(uint16(0x11), pm, esm))
}

func TestIsPESPayload(t *testing.T) {
	buf := &bytes.Buffer{}
	w := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: buf})
	w.Write("0000000000000001")
	assert.False(t, isPESPayload(buf.Bytes()))
	buf.Reset()
	w.Write("000000000000000000000001")
	assert.True(t, isPESPayload(buf.Bytes()))
}

func TestIsPSIComplete(t *testing.T) {
	// Pointer field 0, PAT table ID, section_length 5, followed by exactly the
	// five declared bytes.
	complete := []byte{0, uint8(PSITableIDPAT), 0xb0, 0x05, 0, 1, 0xc1, 0x00, 0x01}
	assert.True(t, isPSIComplete([]*Packet{{Payload: complete}}))

	// The same section is waiting for its final transport payload.
	partial := complete[:len(complete)-1]
	assert.False(t, isPSIComplete([]*Packet{{Payload: partial}}))

	// A terminating/stuffing table ID means all preceding complete sections are
	// enough; the parser stops there and does not require trailing stuffing.
	withStuffing := append(append([]byte{}, complete...), 0xff)
	assert.True(t, isPSIComplete([]*Packet{{Payload: withStuffing}}))

	// A stuffing table ID on its own is already a complete PSI packet boundary.
	assert.True(t, isPSIComplete([]*Packet{{Payload: []byte{0, 0xff}}}))
	assert.True(t, isPSIComplete([]*Packet{{Payload: []byte{0}}}))

	// A damaged length that cannot fit into any MPEG-TS PSI section must be
	// rejected instead of accumulating packets forever.
	oversized := []byte{0, uint8(PSITableIDPAT), 0xbf, 0xfd}
	assert.False(t, isPSIComplete([]*Packet{{Payload: oversized}}))

	// A pointer field cannot point past the bytes currently available.
	invalidPointer := []byte{42, uint8(PSITableIDPAT), 0xb0, 0x05, 0, 1, 0xc1, 0, 1}
	assert.False(t, isPSIComplete([]*Packet{{Payload: invalidPointer}}))
}

func BenchmarkIsPESComplete(b *testing.B) {
	for _, tc := range []struct {
		name    string
		packets int
		bounded bool
		partial bool
	}{
		{"unbounded_1", 1, false, false},
		{"unbounded_256", 256, false, false},
		{"bounded_partial_256", 256, true, true},
		{"bounded_complete_256", 256, true, false},
		{"bounded_complete_1", 1, true, false},
	} {
		b.Run(tc.name, func(b *testing.B) {
			payload := make([]byte, tc.packets*184)
			copy(payload, []byte{0, 0, 1, 0xe0, 0, 0, 0x80, 0, 0})
			if tc.bounded {
				length := len(payload) - 6
				if tc.partial {
					length++
				}
				payload[4], payload[5] = byte(length>>8), byte(length)
			}
			packets := make([]*Packet, tc.packets)
			for i := range packets {
				packets[i] = &Packet{Payload: payload[i*184 : (i+1)*184]}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				isPESComplete(packets)
			}
		})
	}
}

func TestIsPESComplete_PreservesHeaderValidation(t *testing.T) {
	for _, payload := range [][]byte{
		{0, 0, 1, 0xbe, 0, 1, 42}, // No optional header.
		{0, 0, 1, 0xe0, 0, 4, 0x80, 0, 0, 42},
		{0, 0, 1, 0xe0, 0, 0, 0x80, 0, 0, 42}, // Unbounded video.
		{0, 0, 1, 0xe0, 0, 1, 0x80, 0x80, 5},  // Truncated PTS despite full declared length.
		{0, 0, 1, 0xe0, 0, 8, 0x80, 0x80, 5, 0x21, 0, 1, 0, 1},
		{0, 0, 1, 0xe0, 0, 4, 0x80, 1, 1, 0x80}, // Truncated extension.
	} {
		for end := 0; end <= len(payload); end++ {
			data := payload[:end]
			want := false
			if len(data) >= 3 {
				i := astikit.NewBytesIterator(data)
				i.Seek(3)
				h, _, dataEnd, err := parsePESHeader(i)
				want = err == nil && h.PacketLength != 0 && i.Len() >= dataEnd
			}
			for split := 0; split <= end; split++ {
				packets := []*Packet{{Payload: data[:split]}, {}, {Payload: data[split:]}}
				assert.Equal(t, want, isPESComplete(packets), "payload=%x end=%d split=%d", payload, end, split)
			}
		}
	}
}

func TestIsPESComplete_UnboundedAndPartialDoNotAllocate(t *testing.T) {
	for _, length := range []byte{0, 255} {
		packets := []*Packet{{Payload: []byte{0, 0, 1, 0xe0, 0, length, 0x80, 0, 0}}}
		assert.False(t, isPESComplete(packets))
		assert.Zero(t, testing.AllocsPerRun(100, func() { isPESComplete(packets) }))
	}
}

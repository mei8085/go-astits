package astits

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/asticode/go-astikit"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// psiPacket builds a single 188-byte TS packet carrying one PSI section with an
// explicit version number. PAT uses PID 0; PMTs use their advertised PID.
func psiPacket(t *testing.T, pid uint16, tableID PSITableID, tableIDExt uint16, version uint8, data *PSISectionSyntaxData, cc uint8) []byte {
	t.Helper()
	section := &PSISection{
		Header: &PSISectionHeader{
			SectionSyntaxIndicator: true,
			TableID:                tableID,
		},
		Syntax: &PSISectionSyntax{
			Data: data,
			Header: &PSISectionSyntaxHeader{
				CurrentNextIndicator: true,
				LastSectionNumber:    0,
				SectionNumber:        0,
				TableIDExtension:     tableIDExt,
				VersionNumber:        version,
			},
		},
	}
	section.Header.SectionLength = calcPSISectionLength(section)

	payload := &bytes.Buffer{}
	pw := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: payload})
	_, err := writePSIData(pw, &PSIData{Sections: []*PSISection{section}})
	require.NoError(t, err)

	pkt := &Packet{
		Header: PacketHeader{
			PID:                       pid,
			ContinuityCounter:         cc,
			HasPayload:                true,
			PayloadUnitStartIndicator: true,
		},
		Payload: payload.Bytes(),
	}
	out := &bytes.Buffer{}
	ow := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: out})
	_, err = writePacket(ow, pkt, MpegTsPacketSize)
	require.NoError(t, err)
	return out.Bytes()
}

func patPacket(t *testing.T, version uint8, pmtPID, programNumber uint16, cc uint8) []byte {
	return psiPacket(t, PIDPAT, PSITableIDPAT, 1, version,
		&PSISectionSyntaxData{PAT: &PATData{
			TransportStreamID: 1,
			Programs:          []*PATProgram{{ProgramMapID: pmtPID, ProgramNumber: programNumber}},
		}}, cc)
}

func pmtPacket(t *testing.T, pid, programNumber uint16, version uint8, esPID uint16, st StreamType, cc uint8) []byte {
	return psiPacket(t, pid, PSITableIDPMT, programNumber, version,
		&PSISectionSyntaxData{PMT: &PMTData{
			ProgramNumber:     programNumber,
			PCRPID:            esPID,
			ElementaryStreams: []*PMTElementaryStream{{ElementaryPID: esPID, StreamType: st}},
		}}, cc)
}

// pesPackets builds one complete PES unit on a PID, split across two TS
// packets, with an explicit starting continuity counter.
func pesPackets(pid uint16, streamID byte, fill byte, startCC uint8) [][]byte {
	pesHeader := []byte{0x00, 0x00, 0x01, streamID, 0x00, 0x00, 0x80, 0x00, 0x00}
	unit := append(append([]byte{}, pesHeader...), bytes.Repeat([]byte{fill}, 200)...)

	first := make([]byte, MpegTsPacketSize-4) // 4-byte TS header
	copy(first, unit)
	secondPayload := unit[len(first):]

	mk := func(cc uint8, pusi bool, payload []byte) []byte {
		out := &bytes.Buffer{}
		w := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: out})
		pkt := &Packet{
			Header:  PacketHeader{PID: pid, ContinuityCounter: cc, HasPayload: true, PayloadUnitStartIndicator: pusi},
			Payload: payload,
		}
		_, _ = writePacket(w, pkt, MpegTsPacketSize)
		return out.Bytes()
	}
	return [][]byte{mk(startCC, true, first), mk(startCC+1, false, secondPayload)}
}

// End-to-end behavior across an explicit PAT/PMT switch:
//   - the complete unit before the switch is emitted under the old mapping;
//   - a partial PES queued on a PID that is reassigned by the switch is
//     dropped at the switch boundary, never stitched onto the new stream;
//   - the first complete unit after an interruption follows the new mapping.
func TestDemuxerProgramSwitchKeepsStreamConsistency(t *testing.T) {
	const esPID uint16 = 0x0100
	var stream bytes.Buffer

	// Old configuration: PMT PID 0x1000, video on 0x0100.
	stream.Write(patPacket(t, 1, 0x1000, 1, 0))
	stream.Write(pmtPacket(t, 0x1000, 1, 1, esPID, StreamTypeH264Video, 0))
	for _, p := range pesPackets(esPID, 0xe0, 0xaa, 0) {
		stream.Write(p)
	}

	// A stale, incomplete PES fragment starts right before the interruption.
	stale := &bytes.Buffer{}
	sw := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: stale})
	_, err := writePacket(sw, &Packet{
		Header:  PacketHeader{PID: esPID, ContinuityCounter: 2, HasPayload: true, PayloadUnitStartIndicator: true},
		Payload: []byte{0x00, 0x00, 0x01, 0xe0, 0x00, 0x00, 0x80, 0x00, 0x00, 0xde, 0xad, 0xbe, 0xef},
	}, MpegTsPacketSize)
	require.NoError(t, err)
	stream.Write(stale.Bytes())

	// New configuration: new PMT PID 0x1001 and bumped table versions; the
	// same elementary PID now carries audio.
	stream.Write(patPacket(t, 2, 0x1001, 1, 1))
	stream.Write(pmtPacket(t, 0x1001, 1, 2, esPID, StreamTypeADTS, 0))

	// Resume: a complete audio unit under the new mapping.
	for _, p := range pesPackets(esPID, 0xc0, 0xbb, 3) {
		stream.Write(p)
	}

	dmx := NewDemuxer(context.Background(), bytes.NewReader(stream.Bytes()), DemuxerOptPacketSize(MpegTsPacketSize))

	var (
		patVersions []uint8
		pmtPIDs     []uint16
		pesFills    []byte
	)
	for {
		d, err := dmx.NextData()
		if err != nil {
			require.True(t, errors.Is(err, ErrNoMorePackets))
			break
		}
		switch {
		case d.PAT != nil:
			patVersions = append(patVersions, d.psi.VersionNumber)
		case d.PMT != nil:
			pmtPIDs = append(pmtPIDs, d.PID)
		case d.PES != nil:
			assert.NotContains(t, d.PES.Data, []byte{0xde, 0xad, 0xbe, 0xef}, "stale fragment must never reach an emitted PES")
			pesFills = append(pesFills, d.PES.Data[0])
		}
	}

	// Each PAT version is delivered to the consumer even though retransmissions
	// do not re-apply the mapping.
	assert.Equal(t, []uint8{1, 2}, patVersions)
	assert.Equal(t, []uint16{0x1000, 0x1001}, pmtPIDs)

	// Only the two complete units come out, old then new, with no stitching.
	assert.Equal(t, []byte{0xaa, 0xbb}, pesFills)

	// Final active state reflects the new mapping.
	assert.Equal(t, map[uint32]uint16{0x1001: 1}, dmx.programMap.p)
	assert.Equal(t, map[uint32]uint16{0x100: 1}, dmx.streamMap.es)
}

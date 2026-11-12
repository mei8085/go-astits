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

type transportSection struct {
	bytes []byte
}

func boundaryPATSection(t *testing.T) transportSection {
	t.Helper()
	const sectionLength = 5 + 4 + 4
	buf := &bytes.Buffer{}
	buf.WriteByte(byte(PSITableIDPAT))
	buf.WriteByte(0xb0 | byte(sectionLength>>8))
	buf.WriteByte(byte(sectionLength))
	buf.Write([]byte{0x00, 0x01, 0xc1, 0x00, 0x00})
	buf.Write([]byte{0x00, 0x2a, 0xe1, 0x00})
	return transportSection{bytes: appendCRC32(buf.Bytes())}
}

func boundaryPMTSection(t *testing.T, programNumber uint16) transportSection {
	t.Helper()
	unknownExtension := []byte{0xa1, 0xb2, 0xc3, 0xd4, 0xe5, 0xf6}
	data := &PSIData{
		PointerField: 0,
		Sections: []*PSISection{{
			Header: &PSISectionHeader{
				TableID:                PSITableIDPMT,
				PrivateBit:             true,
				SectionSyntaxIndicator: true,
				SectionLength:          0,
			},
			Syntax: &PSISectionSyntax{
				Header: &PSISectionSyntaxHeader{TableIDExtension: programNumber, VersionNumber: 0, CurrentNextIndicator: true, SectionNumber: 0, LastSectionNumber: 0},
				Data: &PSISectionSyntaxData{PMT: &PMTData{
					PCRPID:             0x1000,
					ProgramNumber:      programNumber,
					ProgramDescriptors: []*Descriptor{{Tag: DescriptorTagExtension, Length: 7, Extension: &DescriptorExtension{Tag: 0x23, Unknown: &unknownExtension}}},
					ElementaryStreams: []*PMTElementaryStream{{
						StreamType:    StreamTypePrivateData,
						ElementaryPID: 0x0142,
						ElementaryStreamDescriptors: []*Descriptor{
							{Tag: DescriptorTagStreamIdentifier, Length: 1, StreamIdentifier: &DescriptorStreamIdentifier{ComponentTag: 0x07}},
							{Tag: DescriptorTagPrivateDataSpecifier, Length: 4, PrivateDataSpecifier: &DescriptorPrivateDataSpecifier{Specifier: 0x12345678}},
						},
					}},
				}},
			},
		}},
	}
	section := data.Sections[0]
	section.Header.SectionLength = calcPSISectionLength(section)
	buf := &bytes.Buffer{}
	_, err := writePSIData(astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: buf}), data)
	require.NoError(t, err)
	return transportSection{bytes: buf.Bytes()[1:]}
}

func appendCRC32(section []byte) []byte {
	crc := computeCRC32(section)
	return append(section, byte(crc>>24), byte(crc>>16), byte(crc>>8), byte(crc))
}

func boundaryPSIPayload(section []byte) []byte {
	return append([]byte{0x00}, section...)
}

func boundaryTSPackets(t *testing.T, pid uint16, payload []byte, startCounter byte, firstChunkSize int) [][]byte {
	t.Helper()
	require.LessOrEqual(t, len(payload), 2*(MpegTsPacketSize-4))

	var packets [][]byte
	for packetIndex := 0; len(payload) > 0; packetIndex++ {
		chunkSize := MpegTsPacketSize - 4
		if packetIndex == 0 && firstChunkSize > 0 {
			chunkSize = firstChunkSize
		}
		chunk := payload
		if len(chunk) > chunkSize {
			chunk = chunk[:chunkSize]
		}
		payload = payload[len(chunk):]

		packet := make([]byte, MpegTsPacketSize)
		packet[0] = syncByte
		packet[1] = byte(pid >> 8)
		if packetIndex == 0 {
			packet[1] |= 0x40
		}
		packet[2] = byte(pid)
		packet[3] = 0x30 | ((startCounter + byte(packetIndex)) & 0x0f)
		stuffingLength := MpegTsPacketSize - 4 - len(chunk)
		packet[4] = byte(stuffingLength - 1)
		if stuffingLength > 1 {
			packet[5] = 0
			for i := 6; i < 4+stuffingLength; i++ {
				packet[i] = 0xff
			}
		}
		copy(packet[4+stuffingLength:], chunk)
		packets = append(packets, packet)
	}
	return packets
}

func joinTSPackets(packetGroups ...[][]byte) []byte {
	var totalLength int
	for _, packets := range packetGroups {
		totalLength += len(packets) * MpegTsPacketSize
	}

	output := make([]byte, 0, totalLength)
	for _, packets := range packetGroups {
		for _, packet := range packets {
			output = append(output, packet...)
		}
	}
	return output
}

func boundaryTestDescriptors() (programDescriptors, esDescriptors []byte) {
	programDescriptors = []byte{
		DescriptorTagExtension, 7,
		0x23,
		0xa1, 0xb2, 0xc3, 0xd4, 0xe5, 0xf6,
	}
	esDescriptors = append(
		[]byte{DescriptorTagStreamIdentifier, 1, 0x07},
		[]byte{
			DescriptorTagPrivateDataSpecifier, 4,
			0x12, 0x34, 0x56, 0x78,
		}...,
	)
	return programDescriptors, esDescriptors
}

func TestParseDataCrossPacketNestedDescriptor(t *testing.T) {
	programDescriptors, _ := boundaryTestDescriptors()
	section := boundaryPMTSection(t, 42).bytes
	payload := boundaryPSIPayload(section)

	// Split the extension descriptor across two TS payloads. Its outer tag and
	// length are in the first packet, while the nested extension tag begins the
	// continuation packet.
	require.Equal(t, programDescriptors[0], payload[13])
	require.Equal(t, programDescriptors[1], payload[14])
	require.Equal(t, programDescriptors[2], payload[15])
	packets := []*Packet{{
		Header:  PacketHeader{PID: 0x100, HasPayload: true, PayloadUnitStartIndicator: true, ContinuityCounter: 0},
		Payload: payload[:15],
	}, {
		Header:  PacketHeader{PID: 0x100, HasPayload: true, ContinuityCounter: 1},
		Payload: payload[15:],
	}}

	programMap := newProgramMap()
	programMap.setUnlocked(0x100, 42)
	parsed, err := parseData(packets, nil, programMap, newElementaryStreamMap())
	require.NoError(t, err)
	require.Len(t, parsed, 1)
	require.NotNil(t, parsed[0].PMT)

	programDescriptor := parsed[0].PMT.ProgramDescriptors[0]
	require.NotNil(t, programDescriptor.Extension)
	assert.Equal(t, []byte{0xa1, 0xb2, 0xc3, 0xd4, 0xe5, 0xf6}, *programDescriptor.Extension.Unknown)
	streamDescriptors := parsed[0].PMT.ElementaryStreams[0].ElementaryStreamDescriptors
	require.Len(t, streamDescriptors, 2)
	require.NotNil(t, streamDescriptors[1].PrivateDataSpecifier)
	assert.Equal(t, uint32(0x12345678), streamDescriptors[1].PrivateDataSpecifier.Specifier)
}

func TestDemuxerCRCErrorDoesNotStopLaterValidPMT(t *testing.T) {
	validSection := boundaryPMTSection(t, 42)
	badCRCSection := append([]byte(nil), validSection.bytes...)
	badCRCSection[len(badCRCSection)-1] ^= 0xff

	pat := boundaryPATSection(t)
	patPackets := boundaryTSPackets(t, PIDPAT, boundaryPSIPayload(pat.bytes), 0, 0)
	badCRCPackets := boundaryTSPackets(t, 0x100, boundaryPSIPayload(badCRCSection), 0, 15)
	validPackets := boundaryTSPackets(t, 0x100, boundaryPSIPayload(validSection.bytes), 2, 15)
	dmx := NewDemuxer(
		context.Background(),
		bytes.NewReader(joinTSPackets(patPackets, badCRCPackets, validPackets)),
		DemuxerOptPacketSize(MpegTsPacketSize),
	)

	patData, err := dmx.NextData()
	require.NoError(t, err)
	require.NotNil(t, patData.PAT)

	crcData, err := dmx.NextData()
	assert.True(t, errors.Is(err, ErrCRC32Mismatch))
	assert.Nil(t, crcData)

	// The failed section was flushed by the next payload-unit-start, so the
	// following valid section can still be demuxed from the same PID.
	validData, err := dmx.NextData()
	require.NoError(t, err)
	require.NotNil(t, validData.PMT)
	assert.Equal(t, uint16(42), validData.PMT.ProgramNumber)
	require.Len(t, validData.PMT.ProgramDescriptors, 1)
	assert.Equal(t, uint8(DescriptorTagExtension), validData.PMT.ProgramDescriptors[0].Tag)
}

func TestParsePSIDataTruncatedAndOverlongDescriptorBoundaries(t *testing.T) {
	validSection := boundaryPMTSection(t, 42)

	t.Run("cross packet truncation", func(t *testing.T) {
		truncated := validSection.bytes[:len(validSection.bytes)-9]
		packets := []*Packet{{
			Header:  PacketHeader{PID: 0x100, HasPayload: true, PayloadUnitStartIndicator: true, ContinuityCounter: 0},
			Payload: boundaryPSIPayload(truncated[:14]),
		}, {
			Header:  PacketHeader{PID: 0x100, HasPayload: true, ContinuityCounter: 1},
			Payload: truncated[14:],
		}}

		programMap := newProgramMap()
		programMap.setUnlocked(0x100, 42)
		assert.False(t, isPSIComplete(packets))
		data, err := parseData(packets, nil, programMap, newElementaryStreamMap())
		assert.Error(t, err)
		assert.Nil(t, data)
		assert.False(t, errors.Is(err, ErrCRC32Mismatch))
	})

	t.Run("declared section and descriptor boundaries cannot be overrun", func(t *testing.T) {
		section := append([]byte(nil), boundaryPMTSection(t, 43).bytes...)
		// Shrink the declared section while keeping the descriptor payload,
		// forcing parsing to stop before the declared section boundary is reached.
		section[2] &= 0xf0
		section[3] = 8
		section = appendCRC32(section[:len(section)-4])
		parsed, err := parsePSIData(astikit.NewBytesIterator(boundaryPSIPayload(section)))
		assert.Error(t, err)
		assert.Nil(t, parsed.Sections)
	})
}

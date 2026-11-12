package astits

import (
	"encoding/binary"
	"fmt"
	"github.com/asticode/go-astikit"
)

// PIDs
const (
	PIDPAT  uint16 = 0x0    // Program Association Table (PAT) contains a directory listing of all Program Map Tables.
	PIDCAT  uint16 = 0x1    // Conditional Access Table (CAT) contains a directory listing of all ITU-T Rec. H.222 entitlement management message streams used by Program Map Tables.
	PIDTSDT uint16 = 0x2    // Transport Stream Description Table (TSDT) contains descriptors related to the overall transport stream
	PIDNull uint16 = 0x1fff // Null Packet (used for fixed bandwidth padding)
)

// DemuxerData represents a data parsed by Demuxer
type DemuxerData struct {
	EIT         *EITData
	FirstPacket *Packet
	NIT         *NITData
	PAT         *PATData
	PES         *PESData
	PID         uint16
	PMT         *PMTData
	SDT         *SDTData
	TOT         *TOTData
}

// MuxerData represents a data to be written by Muxer
type MuxerData struct {
	PID             uint16
	AdaptationField *PacketAdaptationField
	PES             *PESData
}

// parseData parses a payload spanning over multiple packets and returns a set of data
func parseData(ps []*Packet, prs PacketsParser, pm *programMap, esm *elementaryStreamMap) (ds []*DemuxerData, err error) {
	// Use custom parser first
	if prs != nil {
		var skip bool
		if ds, skip, err = prs(ps); err != nil {
			err = fmt.Errorf("astits: custom packets parsing failed: %w", err)
			return
		} else if skip {
			return
		}
	}

	// Build a temporary, demuxer-owned aggregate. Parsers copy every byte field
	// that becomes part of PES/PSI output; scalar no-copy reads do not outlive this
	// call, so the buffer can be pooled immediately after parsing returns.
	payload := joinPacketPayloads(ps)
	defer bytesPool.put(payload)

	// Create reader
	i := astikit.NewBytesIterator(payload.s)

	// Parse PID
	pid := ps[0].Header.PID

	// Keep only value headers in the returned data. The packet's payload remains
	// owned by the assembly layer until the complete packet set is released.
	fp := &Packet{
		Header:          ps[0].Header,
		AdaptationField: ps[0].AdaptationField,
	}

	// Parse payload
	if pid == PIDCAT {
		// Information in a CAT payload is private and dependent on the CA system. Use the PacketsParser
		// to parse this type of payload
	} else if isPSIPayload(pid, pm, esm) {
		// Parse PSI data
		var psiData *PSIData
		if psiData, err = parsePSIData(i); err != nil {
			err = fmt.Errorf("astits: parsing PSI data failed: %w", err)
			return
		}

		// Append data
		ds = psiData.toData(fp, pid)
	} else if isPESPayload(payload.s) {
		// Parse PES data
		var pesData *PESData
		if pesData, err = parsePESData(i); err != nil {
			err = fmt.Errorf("astits: parsing PES data failed: %w", err)
			return
		}

		// Append data
		ds = []*DemuxerData{
			{
				FirstPacket: fp,
				PES:         pesData,
				PID:         pid,
			},
		}
	}
	return
}

// isPSIPayload checks whether the payload is a PSI one
func isPSIPayload(pid uint16, pm *programMap, esm *elementaryStreamMap) bool {
	return pid == PIDPAT || // PAT
		pm.existsUnlocked(pid) || // PMT
		(((pid >= 0x10 && pid <= 0x14) || (pid >= 0x1e && pid <= 0x1f)) && // DVB
			!esm.existsUnlocked(pid)) // for non-DVB
}

func joinPacketPayloads(ps []*Packet) *bytesPoolItem {
	var l int
	for _, p := range ps {
		l += len(p.Payload)
	}

	payload := bytesPool.get(l)
	var offset int
	for _, p := range ps {
		offset += copy(payload.s[offset:], p.Payload)
	}
	return payload
}

// isPESPayload checks whether the payload is a PES one
func isPESPayload(i []byte) bool {
	// Packet is not big enough
	if len(i) < 3 {
		return false
	}

	// Check prefix
	return uint32(i[0])<<16|uint32(i[1])<<8|uint32(i[2]) == 1
}

// isPSIComplete checks whether we have sufficient amount of packets to parse PSI
func isPSIComplete(ps []*Packet) bool {
	payload := joinPacketPayloads(ps)
	defer bytesPool.put(payload)

	// Create reader
	i := astikit.NewBytesIterator(payload.s)

	// Get next byte
	b, err := i.NextByte()
	if err != nil {
		return false
	}

	// Pointer filler bytes
	pointerField := int(b)
	if pointerField > i.Len()-i.Offset() {
		return false
	}
	i.Skip(pointerField)

	for i.HasBytesLeft() {

		// Get PSI table ID
		b, err = i.NextByte()
		if err != nil {
			return false
		}

		// A null/unknown table ID marks stuffing or an unparsable table boundary.
		// Every section before it must already be fully covered by the packets.
		if shouldStopPSIParsing(PSITableID(b)) {
			return true
		}

		// Get PSI section length
		var bs []byte
		bs, err = i.NextBytesNoCopy(2)
		if err != nil {
			return false
		}

		sectionLength := int(binary.BigEndian.Uint16(bs) & 0x0fff)
		// A section_length field covers the bytes following its own two bytes.
		// The MPEG-TS limit keeps a damaged stream from accumulating an unbounded
		// table while waiting for a section that cannot fit in a TS payload.
		if sectionLength == 0 || sectionLength > 1021 || i.Offset()+sectionLength > i.Len() {
			return false
		}
		i.Skip(sectionLength)
	}

	// All bytes must be covered by a declared section. Any short section means
	// another transport packet is still required.
	return i.Offset() == i.Len()
}

// isPESComplete checks whether payload fully contains PES packet
func isPESComplete(ps []*Packet) bool {
	var header [6]byte
	var n int
	for _, p := range ps {
		n += copy(header[n:], p.Payload)
		if n == len(header) {
			break
		}
	}
	if n < len(header) || binary.BigEndian.Uint16(header[4:]) == 0 {
		return false
	}

	declaredLength := int(binary.BigEndian.Uint16(header[4:]))

	var l int
	for _, p := range ps {
		l += len(p.Payload)
	}

	// PES_packet_length excludes the six-byte fixed header.
	if l < len(header)+declaredLength {
		return false
	}

	payload := joinPacketPayloads(ps)
	defer bytesPool.put(payload)

	// Create reader
	i := astikit.NewBytesIterator(payload.s)

	// Skip first 3 bytes that are there to identify the PES payload
	i.Seek(3)

	// Parse header
	h, _, dataEnd, err := parsePESHeader(i)
	if err != nil {
		err = fmt.Errorf("astits: parsing PES header failed: %w", err)
		return false
	}

	if h.PacketLength == 0 {
		// There's no other way to know whether the packet is complete
		return false
	}

	return dataEnd <= i.Len()
}

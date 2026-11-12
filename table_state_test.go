package astits

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func patDataWith(version uint8, section, last uint8, programs ...*PATProgram) (*DemuxerData, *PSISectionSyntaxHeader) {
	h := &PSISectionSyntaxHeader{
		CurrentNextIndicator: true,
		LastSectionNumber:    last,
		SectionNumber:        section,
		TableIDExtension:     1,
		VersionNumber:        version,
	}
	return &DemuxerData{PID: PIDPAT, PAT: &PATData{Programs: programs, TransportStreamID: 1}, psi: h}, h
}

func pmtDataWith(pid uint16, version uint8, section, last uint8, esPIDs ...uint16) (*DemuxerData, *PSISectionSyntaxHeader) {
	h := &PSISectionSyntaxHeader{
		CurrentNextIndicator: true,
		LastSectionNumber:    last,
		SectionNumber:        section,
		TableIDExtension:     1,
		VersionNumber:        version,
	}
	streams := make([]*PMTElementaryStream, 0, len(esPIDs))
	for _, esPID := range esPIDs {
		streams = append(streams, &PMTElementaryStream{ElementaryPID: esPID, StreamType: StreamTypeH264Video})
	}
	return &DemuxerData{
		PID: pid,
		PMT: &PMTData{
			ElementaryStreams: streams,
			PCRPID:            esPIDs[0],
			ProgramNumber:     1,
		},
		psi: h,
	}, h
}

func newTestTableState() (*programTableState, *programMap, *elementaryStreamMap, *packetPool) {
	pm := newProgramMap()
	esm := newElementaryStreamMap()
	pool := newPacketPool(pm)
	return newProgramTableState(pm, esm, pool), pm, esm, pool
}

// A retransmitted table version must leave the active mapping untouched.
func TestPSITableStateRetransmissionKeepsOldState(t *testing.T) {
	s := newPSITableState()
	d, h := patDataWith(3, 0, 0, &PATProgram{ProgramMapID: 0x1000, ProgramNumber: 1})

	assert.NotNil(t, s.ingest(d, h))
	assert.Equal(t, 3, s.version)

	// Same version retransmitted: no commit, still reporting version 3.
	assert.Nil(t, s.ingest(d, h))
	assert.Equal(t, 3, s.version)
}

// A partially received new multi-section table must keep the old version
// active until every section of the new version is available.
func TestPSITableStatePartialNewVersionWaits(t *testing.T) {
	s := newPSITableState()

	// Commit version 1 (single section).
	d1, h1 := patDataWith(1, 0, 0, &PATProgram{ProgramMapID: 0x1000, ProgramNumber: 1})
	assert.NotNil(t, s.ingest(d1, h1))

	// Version 2 spans two sections: only section 0 has arrived.
	d20, h20 := patDataWith(2, 0, 1, &PATProgram{ProgramMapID: 0x1000, ProgramNumber: 1})
	assert.Nil(t, s.ingest(d20, h20), "partial new table must not be committed")
	assert.Equal(t, 1, s.version, "old version stays active while the new one is partial")

	// The remaining section completes version 2; sections come back in order.
	d21, h21 := patDataWith(2, 1, 1, &PATProgram{ProgramMapID: 0x1001, ProgramNumber: 1})
	committed := s.ingest(d21, h21)
	assert.Len(t, committed, 2)
	assert.Equal(t, uint8(0), committed[0].psi.SectionNumber)
	assert.Equal(t, uint8(1), committed[1].psi.SectionNumber)
	assert.Equal(t, 2, s.version)
}

// A next (not yet current) table must not affect the active configuration.
func TestPSITableStateNextIndicatorIgnored(t *testing.T) {
	s := newPSITableState()
	d, h := patDataWith(1, 0, 0)
	h.CurrentNextIndicator = false
	assert.Nil(t, s.ingest(d, h))
	assert.Equal(t, -1, s.version)
}

// Switching the PMT PID in the PAT must reset the old PID accumulator and
// remove the stale program entry.
func TestProgramTableStatePATSwitchResetsReplacedPID(t *testing.T) {
	state, pm, _, pool := newTestTableState()

	// Seed an in-flight PSI accumulator on the PMT PID that is about to be
	// replaced.
	pool.addUnlocked(&Packet{Header: PacketHeader{PID: 0x1000, ContinuityCounter: 0, HasPayload: true, PayloadUnitStartIndicator: true}, Payload: []byte{0xff}})
	assert.NotNil(t, pool.b[0x1000])

	d1, h1 := patDataWith(1, 0, 0, &PATProgram{ProgramMapID: 0x1000, ProgramNumber: 1})
	state.handlePAT(d1, h1)
	assert.Equal(t, map[uint32]uint16{0x1000: 1}, pm.p)

	// New PAT version moves the program to another PMT PID.
	d2, h2 := patDataWith(2, 0, 0, &PATProgram{ProgramMapID: 0x1001, ProgramNumber: 1})
	state.handlePAT(d2, h2)

	assert.Equal(t, map[uint32]uint16{0x1001: 1}, pm.p)
	if acc, ok := pool.b[0x1000]; ok {
		assert.Empty(t, acc.q, "the replaced PMT PID accumulator must be flushed")
	}
	_, tracked := state.pmts[0x1000]
	assert.False(t, tracked, "the replaced PMT PID table state must be forgotten")
}

// A new PMT version must reset the accumulators of the elementary streams it
// advertises so an interrupted PES is never continued on the new stream.
func TestProgramTableStatePMTSwitchFlushesElementaryPIDs(t *testing.T) {
	state, _, esm, pool := newTestTableState()

	pat, patH := patDataWith(1, 0, 0, &PATProgram{ProgramMapID: 0x1000, ProgramNumber: 1})
	state.handlePAT(pat, patH)

	pmt1, pmt1H := pmtDataWith(0x1000, 1, 0, 0, 0x0100, 0x0101)
	state.handlePMT(pmt1, pmt1H)
	assert.Equal(t, map[uint32]uint16{0x100: 1, 0x101: 1}, esm.es)

	// A PES is interrupted mid-flight on both elementary PIDs.
	for _, pid := range []uint16{0x0100, 0x0101} {
		pool.addUnlocked(&Packet{Header: PacketHeader{PID: pid, ContinuityCounter: 0, HasPayload: true, PayloadUnitStartIndicator: true}, Payload: []byte{0, 0, 1, 0xe0, 0, 0, 0x80}})
		assert.NotEmpty(t, pool.b[uint32(pid)].q)
	}

	// New PMT version: same PIDs, but a new program mapping after a resume.
	pmt2, pmt2H := pmtDataWith(0x1000, 2, 0, 0, 0x0100, 0x0101)
	state.handlePMT(pmt2, pmt2H)

	for _, pid := range []uint16{0x0100, 0x0101} {
		assert.Empty(t, pool.b[uint32(pid)].q, "queued PES fragments of PID %d must be dropped at the switch", pid)
	}
	assert.Equal(t, map[uint32]uint16{0x100: 1, 0x101: 1}, esm.es)
}

// PMTs of interleaved multi-program transports are tracked independently by
// PID: a retransmission of one program never blocks a version change of
// another.
func TestProgramTableStateInterleavedPMTsTrackedPerPID(t *testing.T) {
	state, pm, esm, _ := newTestTableState()

	pat, patH := patDataWith(1, 0, 0,
		&PATProgram{ProgramMapID: 0x1000, ProgramNumber: 1},
		&PATProgram{ProgramMapID: 0x1001, ProgramNumber: 2},
	)
	state.handlePAT(pat, patH)
	assert.Equal(t, map[uint32]uint16{0x1000: 1, 0x1001: 2}, pm.p)

	pmtA1, hA1 := pmtDataWith(0x1000, 1, 0, 0, 0x0100)
	pmtA1.PMT.ProgramNumber = 1
	pmtB1, hB1 := pmtDataWith(0x1001, 1, 0, 0, 0x0200)
	pmtB1.PMT.ProgramNumber = 2
	state.handlePMT(pmtA1, hA1)
	state.handlePMT(pmtB1, hB1)

	// Retransmission of program A's PMT.
	state.handlePMT(pmtA1, hA1)

	// Program B advances to version 2; it must commit even though A only
	// retransmitted.
	pmtB2, hB2 := pmtDataWith(0x1001, 2, 0, 0, 0x0201)
	pmtB2.PMT.ProgramNumber = 2
	state.handlePMT(pmtB2, hB2)

	assert.Equal(t, 1, state.pmts[0x1000].version)
	assert.Equal(t, 2, state.pmts[0x1001].version)
	assert.Equal(t, uint16(2), esm.es[0x201])
	_, oldES := esm.es[0x200]
	assert.False(t, oldES, "the removed elementary PID must leave the stream map")
}

// A partially received multi-section PMT keeps the previous, complete
// mapping active. The new elementary PID only appears once every section of
// the new PMT version has arrived.
func TestProgramTableStatePartialPMTKeepsOldMapping(t *testing.T) {
	state, _, esm, _ := newTestTableState()

	pat, patH := patDataWith(1, 0, 0, &PATProgram{ProgramMapID: 0x1000, ProgramNumber: 1})
	state.handlePAT(pat, patH)

	// Version 1 (single section) maps PID 0x0100.
	pmt1, h1 := pmtDataWith(0x1000, 1, 0, 0, 0x0100)
	state.handlePMT(pmt1, h1)
	assert.Equal(t, map[uint32]uint16{0x100: 1}, esm.es)

	// Version 2 spans two sections; feed section 0 first.
	h20 := &PSISectionSyntaxHeader{CurrentNextIndicator: true, VersionNumber: 2, SectionNumber: 0, LastSectionNumber: 1, TableIDExtension: 1}
	d20 := &DemuxerData{
		PID: 0x1000,
		PMT: &PMTData{
			ProgramNumber:     1,
			PCRPID:            0x0100,
			ElementaryStreams: []*PMTElementaryStream{{ElementaryPID: 0x0100, StreamType: StreamTypeH264Video}},
		},
		psi: h20,
	}
	state.handlePMT(d20, h20)

	// Old mapping stays explainable while the new table is incomplete.
	assert.Equal(t, map[uint32]uint16{0x100: 1}, esm.es, "partial new PMT must not change the active mapping")
	_, hasNewPID := esm.es[0x101]
	assert.False(t, hasNewPID)

	// Section 1 completes version 2 and introduces PID 0x0101.
	h21 := &PSISectionSyntaxHeader{CurrentNextIndicator: true, VersionNumber: 2, SectionNumber: 1, LastSectionNumber: 1, TableIDExtension: 1}
	d21 := &DemuxerData{
		PID: 0x1000,
		PMT: &PMTData{
			ProgramNumber:     1,
			ElementaryStreams: []*PMTElementaryStream{{ElementaryPID: 0x0101, StreamType: StreamTypeADTS}},
		},
		psi: h21,
	}
	state.handlePMT(d21, h21)

	assert.Equal(t, map[uint32]uint16{0x100: 1, 0x101: 1}, esm.es)
	assert.Equal(t, 2, state.pmts[0x1000].version)
}

// resetPIDUnlocked only clears the queued packets; a later packet on the same
// PID starts a fresh accumulator without stitching old data.
func TestPacketPoolResetPIDUnlocked(t *testing.T) {
	pool := newPacketPool(nil)
	pid := uint16(0x0200)
	pool.addUnlocked(&Packet{Header: PacketHeader{PID: pid, ContinuityCounter: 0, HasPayload: true, PayloadUnitStartIndicator: true}, Payload: []byte{1, 2, 3}})
	require.NotEmpty(t, pool.b[uint32(pid)].q)

	pool.resetPIDUnlocked(pid)
	require.Empty(t, pool.b[uint32(pid)].q)

	// A fresh payload unit starts from zero.
	pool.addUnlocked(&Packet{Header: PacketHeader{PID: pid, ContinuityCounter: 9, HasPayload: true, PayloadUnitStartIndicator: true}, Payload: []byte{9, 9, 9}})
	assert.Len(t, pool.b[uint32(pid)].q, 1)
	assert.Equal(t, byte(9), pool.b[uint32(pid)].q[0].Payload[0])
}

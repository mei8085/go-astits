package astits

// psiTableState tracks the sections of one logical PSI table (one PAT or one
// PMT) and only exposes a complete, consistent version of it.
//
// PSI tables can span several sections. A new version may arrive while the
// previous one is still being retransmitted, and section loss is possible.
// The committed table therefore only changes once every section of one single
// version has been received and CRC-validated. While the new table is only
// partially available, the previous complete table remains the active one.
type psiTableState struct {
	// version is the version number of the currently committed table.
	// A negative value means no complete table has been seen yet.
	version int

	// pendingVersion / pendingLastSection describe the table currently being
	// assembled. pendingVersion < 0 means nothing is being assembled.
	pendingVersion     int
	pendingLastSection int

	// pendingSections holds the already received sections of the pending
	// table, indexed by section number.
	pendingSections map[uint8]*DemuxerData
}

func newPSITableState() *psiTableState {
	return &psiTableState{
		version:            -1,
		pendingVersion:     -1,
		pendingLastSection: -1,
		pendingSections:    make(map[uint8]*DemuxerData),
	}
}

// ingest stores one CRC-validated section.
//
// It returns the ordered sections of a complete table when a new version must
// be committed. Sections belonging to the already committed version are
// ignored (plain retransmissions) and a partially received new version never
// replaces the active table: ingest then returns nil and keeps accumulating.
func (s *psiTableState) ingest(d *DemuxerData, h *PSISectionSyntaxHeader) (committed []*DemuxerData) {
	// A next table (current/next indicator cleared) is not effective yet.
	// Keep the current table untouched until the same version is announced as
	// current.
	if h == nil || !h.CurrentNextIndicator {
		return nil
	}

	// Retransmission of the committed version: the active state stays valid
	// and explainable, nothing is assembled or switched.
	if int(h.VersionNumber) == s.version {
		return nil
	}

	// A newer version supersedes a partially assembled older one.
	if int(h.VersionNumber) != s.pendingVersion {
		s.pendingVersion = int(h.VersionNumber)
		s.pendingSections = make(map[uint8]*DemuxerData)
	}
	// All sections of one table version advertise the same last section
	// number; refresh it on every section anyway to stay robust against an
	// out-of-order first section.
	s.pendingLastSection = int(h.LastSectionNumber)

	s.pendingSections[h.SectionNumber] = d

	// A well-formed single-section table (section 0 of 0) is complete on its
	// own: commit it immediately without entering a multi-section assembly.
	if h.SectionNumber == 0 && h.LastSectionNumber == 0 {
		s.version = int(h.VersionNumber)
		s.pendingVersion = -1
		s.pendingLastSection = -1
		s.pendingSections = make(map[uint8]*DemuxerData)
		return []*DemuxerData{d}
	}

	// The table is complete only once every section number up to
	// last_section_number has been collected.
	if s.pendingLastSection < 0 || len(s.pendingSections) != s.pendingLastSection+1 {
		return nil
	}
	for i := 0; i <= s.pendingLastSection; i++ {
		if _, ok := s.pendingSections[uint8(i)]; !ok {
			return nil
		}
	}

	committed = make([]*DemuxerData, 0, s.pendingLastSection+1)
	for i := 0; i <= s.pendingLastSection; i++ {
		committed = append(committed, s.pendingSections[uint8(i)])
	}

	s.version = s.pendingVersion
	s.pendingVersion = -1
	s.pendingLastSection = -1
	s.pendingSections = make(map[uint8]*DemuxerData)
	return committed
}

// programTableState owns the demuxer's view of the program configuration.
//
// It updates the program/elementary-stream maps atomically, only at explicit
// table switch boundaries (a complete new PAT or PMT version, or a PID that is
// reassigned by such a table), and asks the packet pool to drop the packet
// accumulators of every PID whose meaning changed. That prevents PES fragments
// accumulated under the previous mapping from being stitched onto the new
// stream once the interruption ends.
type programTableState struct {
	programMap *programMap
	streamMap  *elementaryStreamMap
	pool       *packetPool

	pat *psiTableState
	// PMT states are indexed by the PID carrying the PMT so that PMTs of
	// interleaved multi-program transports never overwrite each other.
	pmts map[uint32]*psiTableState
}

func newProgramTableState(pm *programMap, esm *elementaryStreamMap, pool *packetPool) *programTableState {
	return &programTableState{
		programMap: pm,
		streamMap:  esm,
		pool:       pool,
		pat:        newPSITableState(),
		pmts:       make(map[uint32]*psiTableState),
	}
}

// handlePAT ingests one PAT section and applies the new program mapping once
// the whole PAT version is available.
func (s *programTableState) handlePAT(d *DemuxerData, h *PSISectionSyntaxHeader) {
	if d.PAT == nil {
		return
	}

	sections := s.pat.ingest(d, h)
	if sections == nil {
		return
	}

	// Build the mapping advertised by the complete new PAT. Program number 0
	// is reserved for the NIT and must never become a PMT entry.
	newPMTPIDs := make(map[uint16]uint16) // PMT PID -> program number
	for _, section := range sections {
		for _, pgm := range section.PAT.Programs {
			if pgm.ProgramNumber > 0 {
				newPMTPIDs[pgm.ProgramMapID] = pgm.ProgramNumber
			}
		}
	}

	// Drop accumulators and PMT state of PMT PIDs that disappear or get
	// reassigned to another program. On the first complete PAT there is no
	// previous mapping, so nothing can carry stale fragments yet.
	for pid := range s.programMap.p {
		pid16 := uint16(pid)
		newPnr, ok := newPMTPIDs[pid16]
		if !ok {
			// The PMT PID is no longer advertised: its table and any queued
			// PSI packets belong to a removed program.
			s.pool.resetPIDUnlocked(pid16)
			delete(s.pmts, pid)
			continue
		}
		if newPnr != s.programMap.p[pid] {
			// The same PID now carries another program's PMT: start clean and
			// forget the previous program's table version tracking.
			s.pool.resetPIDUnlocked(pid16)
			delete(s.pmts, pid)
			continue
		}
		// A still-advertised PMT PID may carry a new table version. Flush its
		// queued PSI packets at the PAT boundary so the next PMT parsed there
		// belongs to the new configuration; the PMT version state is retained
		// so the PMT handler can distinguish a retransmission from a change.
		s.pool.resetPIDUnlocked(pid16)
	}

	// Atomic switch of the program map: replace the whole content instead of
	// mutating entries one by one.
	for pid := range s.programMap.p {
		delete(s.programMap.p, pid)
	}
	for pid, pnr := range newPMTPIDs {
		s.programMap.setUnlocked(pid, pnr)
	}
}

// handlePMT ingests one PMT section and applies the new elementary-stream
// mapping for its program once the whole PMT version is available.
func (s *programTableState) handlePMT(d *DemuxerData, h *PSISectionSyntaxHeader) {
	if d.PMT == nil {
		return
	}

	key := uint32(d.PID)
	state, ok := s.pmts[key]
	if !ok {
		state = newPSITableState()
		s.pmts[key] = state
	}

	sections := state.ingest(d, h)
	if sections == nil {
		return
	}

	// Merge every section of the committed PMT. Section 0 carries the PCR PID
	// and program-level descriptors; elementary streams are appended in order.
	merged := &PMTData{ProgramNumber: d.PMT.ProgramNumber}
	newESPIDs := make(map[uint16]struct{})
	for i, section := range sections {
		pmt := section.PMT
		if i == 0 {
			merged.PCRPID = pmt.PCRPID
			merged.ProgramDescriptors = pmt.ProgramDescriptors
		}
		for _, es := range pmt.ElementaryStreams {
			merged.ElementaryStreams = append(merged.ElementaryStreams, es)
			newESPIDs[es.ElementaryPID] = struct{}{}
		}
	}

	// This is an explicit PMT version switch boundary. Reset every
	// elementary-stream PID touched by the new mapping so a PES interrupted by
	// the switch can never be continued with packets from the new stream. This
	// also covers a PID that keeps its number but now carries different content
	// (interruption followed by a resume).
	for esPID := range newESPIDs {
		s.pool.resetPIDUnlocked(esPID)
	}

	// Reset PIDs that leave the program or are taken over from another one.
	for pid, pnr := range s.streamMap.es {
		pid16 := uint16(pid)
		if pnr == merged.ProgramNumber {
			if _, ok := newESPIDs[pid16]; !ok {
				s.pool.resetPIDUnlocked(pid16)
			}
			continue
		}
		if _, taken := newESPIDs[pid16]; taken {
			s.pool.resetPIDUnlocked(pid16)
		}
	}

	// Atomic switch of the elementary streams owned by this program.
	for pid, pnr := range s.streamMap.es {
		if pnr == merged.ProgramNumber {
			delete(s.streamMap.es, pid)
		}
	}
	for _, es := range merged.ElementaryStreams {
		s.streamMap.setUnlocked(es.ElementaryPID, merged.ProgramNumber)
	}
}

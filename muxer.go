package astits

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strconv"

	"github.com/asticode/go-astikit"
)

const (
	startPID           uint16 = 0x0100
	pmtStartPID        uint16 = 0x1000
	programNumberStart uint16 = 1
)

var (
	ErrPIDNotFound      = errors.New("astits: PID not found")
	ErrPIDAlreadyExists = errors.New("astits: PID already exists")
	ErrPCRPIDInvalid    = errors.New("astits: PCR PID invalid")
	ErrPMTTooLong       = errors.New("astits: PMT does not fit into a single TS packet")
	ErrNoPendingUpdate  = errors.New("astits: no pending elementary streams update")
)

// MuxerStreamsUpdate describes a batch reconfiguration of the muxer elementary
// streams.
//
// The whole update is atomic: it is either fully applied or fully rejected, the
// previously working state always stays available for output.
type MuxerStreamsUpdate struct {
	// Add lists elementary streams to add. When ElementaryPID is zero a free PID
	// is generated automatically, exactly like AddElementaryStream does.
	Add []PMTElementaryStream

	// Remove lists elementary stream PIDs to remove. Removing a PID that is not
	// present results in ErrPIDNotFound.
	Remove []uint16

	// PCRPID selects the PID carrying the Program Clock Reference after the
	// update. It must reference one of the streams once the update is applied.
	PCRPID uint16
}

// TablesWriteError reports a partial WriteTables/FlushTables failure.
//
// PacketsWritten counts complete 188-byte TS packets already handed to the
// underlying writer when the failure happened (0 = PAT only partially written,
// 1 = PAT fully written but PMT failed, 2 = both tables fully written).
//
// The muxer state never advances on failure: the new configuration is still
// pending and no version/continuity counter is consumed by the failed attempt.
// The caller can either fix the writer and retry (a seekable writer can rewind
// PacketsWritten packets first to avoid duplicate tables), or call
// RollbackElementaryStreamsUpdate to keep outputting the previous configuration.
type TablesWriteError struct {
	// PacketsWritten is the number of complete TS packets already written.
	PacketsWritten int

	// Err is the underlying generation or writer error.
	Err error
}

func (e *TablesWriteError) Error() string {
	return "astits: writing PSI tables failed after " +
		strconv.Itoa(e.PacketsWritten) + " packet(s): " + e.Err.Error()
}

func (e *TablesWriteError) Unwrap() error {
	return e.Err
}

// streamsSnapshot captures the whole elementary stream configuration so a
// pending batch update can be rolled back after a writer failure.
type streamsSnapshot struct {
	elementaryStreams []*PMTElementaryStream
	esContexts        map[uint32]*esContext
	pcrPID            uint16
	nextPID           uint16
}

// tablesGenState captures PSI generation counters and cached table bytes so a
// failed tables write can restore the exact pre-generation state.
type tablesGenState struct {
	patVersion wrappingCounter
	pmtVersion wrappingCounter
	patCC      wrappingCounter
	pmtCC      wrappingCounter
	pmUpdated  bool
	pmtUpdated bool
	patBytes   []byte
	pmtBytes   []byte
}

func (s tablesGenState) restore(m *Muxer) {
	m.patVersion = s.patVersion
	m.pmtVersion = s.pmtVersion
	m.patCC = s.patCC
	m.pmtCC = s.pmtCC
	m.pmUpdated = s.pmUpdated
	m.pmtUpdated = s.pmtUpdated
	m.patBytes.Reset()
	m.patBytes.Write(s.patBytes)
	m.pmtBytes.Reset()
	m.pmtBytes.Write(s.pmtBytes)
}

type Muxer struct {
	ctx        context.Context
	w          io.Writer
	bitsWriter *astikit.BitsWriter

	packetSize             int
	tablesRetransmitPeriod int // period in PES packets

	transportStreamID uint16
	pmtPID            uint16

	pm         *programMap // pid -> programNumber
	pmUpdated  bool
	pmt        PMTData
	pmtUpdated bool
	nextPID    uint16
	patVersion wrappingCounter
	pmtVersion wrappingCounter
	patCC      wrappingCounter
	pmtCC      wrappingCounter

	patBytes bytes.Buffer
	pmtBytes bytes.Buffer

	buf       bytes.Buffer
	bufWriter *astikit.BitsWriter

	// We use map[uint32] instead map[uint16] as go runtime provide optimized hash functions for (u)int32/64 keys
	esContexts              map[uint32]*esContext
	tablesRetransmitCounter int

	// tablesDirty marks a configuration change that must be emitted as a full
	// PAT+PMT pair before any media packet of the new configuration is written.
	// It is set by batch updates and only cleared once both tables have been
	// written successfully, which makes the change take effect at a single,
	// explainable boundary.
	tablesDirty bool

	// snapshot holds the previous, still-outputtable configuration while a batch
	// update waits for its tables to be emitted. It is consumed on rollback or on
	// the first successful tables write after the update.
	snapshot *streamsSnapshot
}

type esContext struct {
	es *PMTElementaryStream
	cc wrappingCounter
}

func newEsContext(es *PMTElementaryStream) *esContext {
	return &esContext{
		es: es,
		cc: newWrappingCounter(0b1111), // CC is 4 bits
	}
}

func MuxerOptTablesRetransmitPeriod(newPeriod int) func(*Muxer) {
	return func(m *Muxer) {
		m.tablesRetransmitPeriod = newPeriod
	}
}

// WithTransportStreamID sets the transport stream ID written into PAT.
// Default is 0.
func WithTransportStreamID(id uint16) func(*Muxer) {
	return func(m *Muxer) {
		m.transportStreamID = id
	}
}

// WithPMTPID sets the PID used for PMT packets and advertised in PAT.
// Default is 0x1000.
func WithPMTPID(pid uint16) func(*Muxer) {
	return func(m *Muxer) {
		m.pmtPID = pid
	}
}

// TODO MuxerOptAutodetectPCRPID selecting first video PID for each PMT, falling back to first audio, falling back to any other

func NewMuxer(ctx context.Context, w io.Writer, opts ...func(*Muxer)) *Muxer {
	m := &Muxer{
		ctx: ctx,
		w:   w,

		packetSize:             MpegTsPacketSize, // no 192-byte packet support yet
		tablesRetransmitPeriod: 40,

		pmtPID: pmtStartPID,

		pm: newProgramMap(),
		pmt: PMTData{
			ElementaryStreams: []*PMTElementaryStream{},
			ProgramNumber:     programNumberStart,
		},

		// table version is 5-bit field
		patVersion: newWrappingCounter(0b11111),
		pmtVersion: newWrappingCounter(0b11111),

		patCC: newWrappingCounter(0b1111),
		pmtCC: newWrappingCounter(0b1111),

		esContexts: map[uint32]*esContext{},
	}

	m.bufWriter = astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: &m.buf})
	m.bitsWriter = astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: m.w})

	for _, opt := range opts {
		opt(m)
	}

	// TODO multiple programs support
	m.pm.setUnlocked(m.pmtPID, programNumberStart)
	m.pmUpdated = true

	// to output tables at the very start
	m.tablesRetransmitCounter = m.tablesRetransmitPeriod

	return m
}

// if es.ElementaryPID is zero, it will be generated automatically
func (m *Muxer) AddElementaryStream(es PMTElementaryStream) error {
	return m.addElementaryStream(es, false)
}

func (m *Muxer) RemoveElementaryStream(pid uint16) error {
	return m.removeElementaryStream(pid, false)
}

// SetPCRPID marks pid as one to look PCRs in
func (m *Muxer) SetPCRPID(pid uint16) {
	m.pmt.PCRPID = pid
	m.pmtUpdated = true
}

// addElementaryStream applies an elementary stream addition. When atomic is
// true the change is only revealed at the next tables boundary (batch update);
// otherwise the legacy per-stream behavior is kept.
func (m *Muxer) addElementaryStream(es PMTElementaryStream, atomic bool) error {
	if es.ElementaryPID != 0 {
		for _, oes := range m.pmt.ElementaryStreams {
			if oes.ElementaryPID == es.ElementaryPID {
				return ErrPIDAlreadyExists
			}
		}
	} else {
		es.ElementaryPID = m.nextPID
		m.nextPID++
	}

	m.pmt.ElementaryStreams = append(m.pmt.ElementaryStreams, &es)
	m.esContexts[uint32(es.ElementaryPID)] = newEsContext(&es)
	// invalidate pmt cache
	m.pmtBytes.Reset()
	m.pmtUpdated = true
	m.tablesDirty = m.tablesDirty || atomic
	return nil
}

// removeElementaryStream removes an elementary stream. When atomic is true the
// change is only revealed at the next tables boundary (batch update).
func (m *Muxer) removeElementaryStream(pid uint16, atomic bool) error {
	foundIdx := -1
	for i, oes := range m.pmt.ElementaryStreams {
		if oes.ElementaryPID == pid {
			foundIdx = i
			break
		}
	}

	if foundIdx == -1 {
		return ErrPIDNotFound
	}

	m.pmt.ElementaryStreams = append(m.pmt.ElementaryStreams[:foundIdx], m.pmt.ElementaryStreams[foundIdx+1:]...)
	delete(m.esContexts, uint32(pid))
	m.pmtBytes.Reset()
	m.pmtUpdated = true
	m.tablesDirty = m.tablesDirty || atomic
	return nil
}

// UpdateElementaryStreams atomically adds, removes and/or replaces elementary
// streams and changes the PCR PID in a single batch.
//
// The update is validated in full before the current configuration is touched:
// duplicated PIDs, removal of unknown PIDs or a PCR PID absent from the
// resulting stream set make the function return an error and leave the muxer
// (and the output stream) untouched, so the previous state keeps being
// outputtable.
//
// On success the new program table, PCR PID and media PIDs take effect at one
// explainable boundary: the next WriteData call first emits a complete,
// consistent PAT+PMT pair and only then the first media packets of the new
// configuration. The boundary can also be emitted explicitly with
// FlushTables. The on-wire tables always stay consistent with the media that
// follows them: no packet of a newly added PID and no table describing it is
// emitted before that boundary, and a removed PID can no longer be written
// once the update is committed.
//
// If the tables cannot be written completely (writer failure), the muxer state
// does not advance: the update stays pending and can either be retried after
// fixing the writer, or discarded with RollbackElementaryStreamsUpdate to keep
// outputting the previous configuration.
func (m *Muxer) UpdateElementaryStreams(u MuxerStreamsUpdate) error {
	// Build the target configuration from the current one without mutating it.
	target := m.pmt.clone()
	targetContexts := make(map[uint32]*esContext, len(m.esContexts)+len(u.Add))
	for pid, ctx := range m.esContexts {
		targetContexts[pid] = ctx
	}

	targetNextPID := m.nextPID
	for i := range u.Add {
		es := u.Add[i]
		if es.ElementaryPID != 0 {
			for _, oes := range target.ElementaryStreams {
				if oes.ElementaryPID == es.ElementaryPID {
					return ErrPIDAlreadyExists
				}
			}
		} else {
			es.ElementaryPID = targetNextPID
			targetNextPID++
		}
		target.ElementaryStreams = append(target.ElementaryStreams, &es)
	}

	for _, pid := range u.Remove {
		foundIdx := -1
		for i, oes := range target.ElementaryStreams {
			if oes.ElementaryPID == pid {
				foundIdx = i
				break
			}
		}
		if foundIdx == -1 {
			return ErrPIDNotFound
		}
		target.ElementaryStreams = append(target.ElementaryStreams[:foundIdx], target.ElementaryStreams[foundIdx+1:]...)
		delete(targetContexts, uint32(pid))
	}

	target.PCRPID = u.PCRPID

	// Validate the whole resulting configuration off-screen before mutating
	// anything: PCR PID must reference an existing stream and the PMT must fit
	// into a single TS packet.
	if err := m.dryRunTables(*target); err != nil {
		return err
	}

	// All checks passed: snapshot the still-outputtable previous state and
	// commit the new configuration atomically.
	m.snapshot = m.takeSnapshot()

	for _, es := range target.ElementaryStreams {
		if _, ok := targetContexts[uint32(es.ElementaryPID)]; !ok {
			targetContexts[uint32(es.ElementaryPID)] = newEsContext(es)
		}
	}

	m.pmt = *target
	m.esContexts = targetContexts
	m.nextPID = targetNextPID
	m.pmtBytes.Reset()
	m.pmtUpdated = true
	m.tablesDirty = true
	return nil
}

// FlushTables emits the pending PAT+PMT boundary explicitly. It has the same
// partial-write semantics as WriteTables: on failure a *TablesWriteError
// reports how many complete packets were already written and the pending
// configuration is preserved for a retry or a rollback.
func (m *Muxer) FlushTables() (int, error) {
	return m.writeTables(true)
}

// TablesDirty reports whether a batch configuration update is waiting for its
// PAT+PMT boundary to be emitted.
func (m *Muxer) TablesDirty() bool {
	return m.tablesDirty
}

// RollbackElementaryStreamsUpdate restores the configuration captured before
// the latest pending batch update and arms its PAT+PMT boundary, so the output
// converges back to the previous configuration without ever mixing old and new
// tables with media PIDs. It returns ErrNoPendingUpdate when there is nothing
// to roll back.
func (m *Muxer) RollbackElementaryStreamsUpdate() error {
	if m.snapshot == nil || !m.tablesDirty {
		return ErrNoPendingUpdate
	}

	m.restoreSnapshot(m.snapshot)
	m.snapshot = nil
	m.tablesDirty = true
	return nil
}

func (m *Muxer) takeSnapshot() *streamsSnapshot {
	streams := make([]*PMTElementaryStream, len(m.pmt.ElementaryStreams))
	copy(streams, m.pmt.ElementaryStreams)

	contexts := make(map[uint32]*esContext, len(m.esContexts))
	for pid, ctx := range m.esContexts {
		contexts[pid] = ctx
	}

	return &streamsSnapshot{
		elementaryStreams: streams,
		esContexts:        contexts,
		pcrPID:            m.pmt.PCRPID,
		nextPID:           m.nextPID,
	}
}

func (m *Muxer) restoreSnapshot(s *streamsSnapshot) {
	streams := make([]*PMTElementaryStream, len(s.elementaryStreams))
	copy(streams, s.elementaryStreams)

	m.pmt.ElementaryStreams = streams
	m.pmt.PCRPID = s.pcrPID
	m.esContexts = s.esContexts
	m.nextPID = s.nextPID
	m.pmtBytes.Reset()
	m.pmtUpdated = true
}

// dryRunTables builds both PSI tables for the given candidate PMT into local
// buffers without touching muxer counters or cached tables. It fails if the
// PCR PID is unknown or the PMT cannot fit into a single TS packet.
func (m *Muxer) dryRunTables(candidate PMTData) error {
	hasPCRPID := false
	for _, es := range candidate.ElementaryStreams {
		if es.ElementaryPID == candidate.PCRPID {
			hasPCRPID = true
			break
		}
	}
	if !hasPCRPID {
		return ErrPCRPIDInvalid
	}

	// calcPMTSectionLength returns the section_length field value; the section
	// itself adds table_id (1 byte) and the length field (2 bytes). A table
	// section must leave room for the TS packet header in a 188-byte packet.
	const tsPacketHeaderSize = 4
	if int(calcPMTSectionLength(&candidate))+3 > m.packetSize-tsPacketHeaderSize {
		return ErrPMTTooLong
	}

	var patBuf, pmtBuf bytes.Buffer
	patWriter := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: &patBuf})
	pmtWriter := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: &pmtBuf})

	patSection := PSISection{
		Header: &PSISectionHeader{
			SectionLength:          calcPATSectionLength(m.pm.toPATDataUnlocked()),
			SectionSyntaxIndicator: true,
			TableID:                PSITableIDPAT,
		},
		Syntax: &PSISectionSyntax{
			Data: &PSISectionSyntaxData{PAT: m.pm.toPATDataUnlocked()},
			Header: &PSISectionSyntaxHeader{
				CurrentNextIndicator: true,
				TableIDExtension:     m.transportStreamID,
			},
		},
	}
	if _, err := writePSIData(patWriter, &PSIData{Sections: []*PSISection{&patSection}}); err != nil {
		return err
	}

	pmtSection := PSISection{
		Header: &PSISectionHeader{
			SectionLength:          calcPMTSectionLength(&candidate),
			SectionSyntaxIndicator: true,
			TableID:                PSITableIDPMT,
		},
		Syntax: &PSISectionSyntax{
			Data: &PSISectionSyntaxData{PMT: &candidate},
			Header: &PSISectionSyntaxHeader{
				CurrentNextIndicator: true,
				TableIDExtension:     candidate.ProgramNumber,
			},
		},
	}
	if _, err := writePSIData(pmtWriter, &PSIData{Sections: []*PSISection{&pmtSection}}); err != nil {
		return err
	}

	return nil
}

// WriteData writes MuxerData to TS stream
// Currently only PES packets are supported
// Be aware that after successful call WriteData will set d.AdaptationField.StuffingLength value to zero
func (m *Muxer) WriteData(d *MuxerData) (int, error) {
	ctx, ok := m.esContexts[uint32(d.PID)]
	if !ok {
		return 0, ErrPIDNotFound
	}

	bytesWritten := 0

	forceTables := d.AdaptationField != nil &&
		d.AdaptationField.RandomAccessIndicator &&
		d.PID == m.pmt.PCRPID

	// A pending batch update owns the next boundary: its complete PAT+PMT pair
	// must be emitted before the first media packet of the new configuration.
	// The lookup below already reflects the committed target configuration, so a
	// removed PID cannot sneak a media packet in ahead of the new tables.
	if m.tablesDirty {
		n, err := m.writeTables(true)
		if err != nil {
			return n, err
		}
		bytesWritten += n
	} else {
		n, err := m.retransmitTables(forceTables)
		if err != nil {
			return n, err
		}
		bytesWritten += n
	}

	payloadStart := true
	writeAf := d.AdaptationField != nil
	payloadBytesWritten := 0
	var n int
	var err error
	for payloadBytesWritten < len(d.PES.Data) {
		pktLen := 1 + mpegTsPacketHeaderSize // sync byte + header
		pkt := Packet{
			Header: PacketHeader{
				ContinuityCounter:         uint8(ctx.cc.inc()),
				HasAdaptationField:        writeAf,
				HasPayload:                false,
				PayloadUnitStartIndicator: false,
				PID:                       d.PID,
			},
		}

		if writeAf {
			pkt.AdaptationField = d.AdaptationField
			// one byte for adaptation field length field
			pktLen += 1 + int(calcPacketAdaptationFieldLength(d.AdaptationField))
			writeAf = false
		}

		bytesAvailable := m.packetSize - pktLen
		if payloadStart {
			pesHeaderLengthCurrent := pesHeaderLength + int(calcPESOptionalHeaderLength(d.PES.Header.OptionalHeader))
			// d.AdaptationField with pes header are too big, we don't have space to write pes header
			if bytesAvailable < pesHeaderLengthCurrent {
				pkt.Header.HasAdaptationField = true
				if pkt.AdaptationField == nil {
					pkt.AdaptationField = newStuffingAdaptationField(bytesAvailable)
				} else {
					pkt.AdaptationField.StuffingLength = bytesAvailable
				}
			} else {
				pkt.Header.HasPayload = true
				pkt.Header.PayloadUnitStartIndicator = true
			}
		} else {
			pkt.Header.HasPayload = true
		}

		if pkt.Header.HasPayload {
			m.buf.Reset()
			if d.PES.Header.StreamID == 0 {
				d.PES.Header.StreamID = ctx.es.StreamType.ToPESStreamID()
			}

			var ntot, npayload int
			ntot, npayload, err = writePESData(
				m.bufWriter,
				d.PES.Header,
				d.PES.Data[payloadBytesWritten:],
				payloadStart,
				bytesAvailable,
			)
			if err != nil {
				return bytesWritten, err
			}

			payloadBytesWritten += npayload

			pkt.Payload = m.buf.Bytes()

			bytesAvailable -= ntot
			// if we still have some space in packet, we should stuff it with adaptation field stuffing
			// we can't stuff packets with 0xff at the end of a packet since it's not uncommon for PES payloads to have length unspecified
			if bytesAvailable > 0 {
				pkt.Header.HasAdaptationField = true
				if pkt.AdaptationField == nil {
					pkt.AdaptationField = newStuffingAdaptationField(bytesAvailable)
				} else {
					pkt.AdaptationField.StuffingLength = bytesAvailable
				}
			}

			n, err = writePacket(m.bitsWriter, &pkt, m.packetSize)
			if err != nil {
				return bytesWritten, err
			}

			bytesWritten += n

			payloadStart = false
		}
	}

	if d.AdaptationField != nil {
		d.AdaptationField.StuffingLength = 0
	}

	return bytesWritten, nil
}

// WritePacket writes a raw packet to the MPEG-TS stream, stuffing it with
// 0xffs if it turns out shorter than the target packet length.
//
// It is a low-level passthrough: it does not emit the pending PAT+PMT boundary
// and does not validate the PID against the current program table. Callers
// driving a batch switch should let WriteData emit the boundary (or call
// FlushTables) before writing raw packets of the new PIDs.
func (m *Muxer) WritePacket(p *Packet) (int, error) {
	return writePacket(m.bitsWriter, p, m.packetSize)
}

func (m *Muxer) retransmitTables(force bool) (int, error) {
	m.tablesRetransmitCounter++
	if !force && m.tablesRetransmitCounter < m.tablesRetransmitPeriod {
		return 0, nil
	}

	n, err := m.WriteTables()
	if err != nil {
		// Keep the counter armed so the next WriteData retries the tables
		// immediately instead of waiting for a full retransmit period.
		m.tablesRetransmitCounter = m.tablesRetransmitPeriod
		return n, err
	}

	m.tablesRetransmitCounter = 0
	return n, nil
}

func (m *Muxer) WriteTables() (int, error) {
	return m.writeTables(false)
}

// writeTables generates and writes a full PAT+PMT pair.
//
// Both tables are built in memory before any byte reaches the writer, so a
// generation failure (e.g. invalid PCR PID) never emits a half of the pair.
// On a writer failure the function returns a *TablesWriteError carrying the
// number of complete packets already written; the muxer state and counters do
// not advance, leaving the pending update available for retry or rollback.
func (m *Muxer) writeTables(boundary bool) (int, error) {
	// The generators bump table versions and continuity counters and clear the
	// updated flags. Snapshot them first so a writer failure can roll the muxer
	// back to the exact pre-generation state: a retry then reproduces the same
	// tables byte for byte and no counter is ever skipped or double-emitted.
	genState := tablesGenState{
		patVersion: m.patVersion,
		pmtVersion: m.pmtVersion,
		patCC:      m.patCC,
		pmtCC:      m.pmtCC,
		pmUpdated:  m.pmUpdated,
		pmtUpdated: m.pmtUpdated,
		patBytes:   append([]byte(nil), m.patBytes.Bytes()...),
		pmtBytes:   append([]byte(nil), m.pmtBytes.Bytes()...),
	}

	if err := m.generatePAT(); err != nil {
		genState.restore(m)
		return 0, &TablesWriteError{PacketsWritten: 0, Err: err}
	}

	if err := m.generatePMT(); err != nil {
		genState.restore(m)
		return 0, &TablesWriteError{PacketsWritten: 0, Err: err}
	}

	pat := append([]byte(nil), m.patBytes.Bytes()...)
	pmt := append([]byte(nil), m.pmtBytes.Bytes()...)

	bytesWritten := 0
	packetsWritten := 0

	n, err := m.w.Write(pat)
	if err != nil {
		genState.restore(m)
		return bytesWritten, &TablesWriteError{PacketsWritten: packetsWritten, Err: err}
	}
	bytesWritten += n
	if n == len(pat) {
		packetsWritten++
	}

	n, err = m.w.Write(pmt)
	if err != nil {
		genState.restore(m)
		return bytesWritten, &TablesWriteError{PacketsWritten: packetsWritten, Err: err}
	}
	bytesWritten += n
	if n == len(pmt) {
		packetsWritten++
	}

	// A short write without an error leaves the boundary unconfirmed: keep the
	// pre-generation state so the next attempt can write the full pair again.
	if packetsWritten != 2 {
		genState.restore(m)
		return bytesWritten, &TablesWriteError{
			PacketsWritten: packetsWritten,
			Err:            io.ErrShortWrite,
		}
	}

	// Both packets reached the writer: the new configuration is now effective on
	// the wire, consume the boundary and the rollback snapshot.
	if boundary {
		m.tablesDirty = false
		m.snapshot = nil
	}

	return bytesWritten, nil
}

func (m *Muxer) generatePAT() error {
	d := m.pm.toPATDataUnlocked()
	d.TransportStreamID = m.transportStreamID

	versionNumber := m.patVersion.get()
	if m.pmUpdated {
		versionNumber = m.patVersion.inc()
	}

	syntax := &PSISectionSyntax{
		Data: &PSISectionSyntaxData{PAT: d},
		Header: &PSISectionSyntaxHeader{
			CurrentNextIndicator: true,
			// TODO support for PAT tables longer than 1 TS packet
			//LastSectionNumber:    0,
			//SectionNumber:        0,
			TableIDExtension: d.TransportStreamID,
			VersionNumber:    uint8(versionNumber),
		},
	}
	section := PSISection{
		Header: &PSISectionHeader{
			SectionLength:          calcPATSectionLength(d),
			SectionSyntaxIndicator: true,
			TableID:                PSITableIDPAT,
		},
		Syntax: syntax,
	}
	psiData := PSIData{
		Sections: []*PSISection{&section},
	}

	m.buf.Reset()
	w := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: &m.buf})
	if _, err := writePSIData(w, &psiData); err != nil {
		return err
	}

	m.patBytes.Reset()
	wPacket := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: &m.patBytes})

	pkt := Packet{
		Header: PacketHeader{
			HasPayload:                true,
			PayloadUnitStartIndicator: true,
			PID:                       PIDPAT,
			ContinuityCounter:         uint8(m.patCC.inc()),
		},
		Payload: m.buf.Bytes(),
	}
	if _, err := writePacket(wPacket, &pkt, m.packetSize); err != nil {
		// FIXME save old PAT and rollback to it here maybe?
		return err
	}

	m.pmUpdated = false

	return nil
}

func (m *Muxer) generatePMT() error {
	hasPCRPID := false
	for _, es := range m.pmt.ElementaryStreams {
		if es.ElementaryPID == m.pmt.PCRPID {
			hasPCRPID = true
			break
		}
	}
	if !hasPCRPID {
		return ErrPCRPIDInvalid
	}

	versionNumber := m.pmtVersion.get()
	if m.pmtUpdated {
		versionNumber = m.pmtVersion.inc()
	}

	syntax := &PSISectionSyntax{
		Data: &PSISectionSyntaxData{PMT: &m.pmt},
		Header: &PSISectionSyntaxHeader{
			CurrentNextIndicator: true,
			// TODO support for PMT tables longer than 1 TS packet
			//LastSectionNumber:    0,
			//SectionNumber:        0,
			TableIDExtension: m.pmt.ProgramNumber,
			VersionNumber:    uint8(versionNumber),
		},
	}
	section := PSISection{
		Header: &PSISectionHeader{
			SectionLength:          calcPMTSectionLength(&m.pmt),
			SectionSyntaxIndicator: true,
			TableID:                PSITableIDPMT,
		},
		Syntax: syntax,
	}
	psiData := PSIData{
		Sections: []*PSISection{&section},
	}

	m.buf.Reset()
	w := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: &m.buf})
	if _, err := writePSIData(w, &psiData); err != nil {
		return err
	}

	m.pmtBytes.Reset()
	wPacket := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: &m.pmtBytes})

	pkt := Packet{
		Header: PacketHeader{
			HasPayload:                true,
			PayloadUnitStartIndicator: true,
			PID:                       m.pmtPID, // FIXME multiple programs support
			ContinuityCounter:         uint8(m.pmtCC.inc()),
		},
		Payload: m.buf.Bytes(),
	}
	if _, err := writePacket(wPacket, &pkt, m.packetSize); err != nil {
		// FIXME save old PMT and rollback to it here maybe?
		return err
	}

	m.pmtUpdated = false

	return nil
}

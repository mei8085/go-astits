package astits

import (
	"bytes"
	"context"
	"errors"
	"io"

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
	ErrWriteInProgress  = errors.New("astits: write in progress")
)

type Muxer struct {
	ctx context.Context
	w   io.Writer

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

	pendingPacket    []byte
	pendingCommit    func()
	pendingTables    bool
	pendingPMT       bool
	delayCommit      bool
	pendingWriteData *muxerWriteDataState

	// We use map[uint32] instead map[uint16] as go runtime provide optimized hash functions for (u)int32/64 keys
	esContexts              map[uint32]*esContext
	tablesRetransmitCounter int
}

type muxerWriteDataState struct {
	d                    *MuxerData
	ctx                  *esContext
	payloadStart         bool
	writeAdaptationField bool
	payloadBytesWritten  int
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
	return nil
}

func (m *Muxer) RemoveElementaryStream(pid uint16) error {
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
	return nil
}

// SetPCRPID marks pid as one to look PCRs in
func (m *Muxer) SetPCRPID(pid uint16) {
	m.pmt.PCRPID = pid
	m.pmtUpdated = true
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
	if m.pendingWriteData != nil && m.pendingWriteData.d != d {
		return 0, ErrWriteInProgress
	}

	if m.pendingPacket != nil {
		n, err := m.flushPendingPacket()
		bytesWritten += n
		if m.pendingPacket != nil {
			return bytesWritten, err
		}

		if m.pendingWriteData != nil {
			return m.continueWriteData(bytesWritten)
		}

		if m.pendingTables {
			return m.writePendingTablePacket(bytesWritten)
		}
	}

	forceTables := d.AdaptationField != nil &&
		d.AdaptationField.RandomAccessIndicator &&
		d.PID == m.pmt.PCRPID

	n, err := m.retransmitTables(forceTables)
	bytesWritten += n
	if err != nil {
		if m.pendingPacket != nil {
			m.pendingWriteData = &muxerWriteDataState{
				d:                    d,
				ctx:                  ctx,
				payloadStart:         true,
				writeAdaptationField: d.AdaptationField != nil,
			}
		}
		return bytesWritten, err
	}

	return m.startWriteData(bytesWritten, d, ctx)
}

func (m *Muxer) startWriteData(bytesWritten int, d *MuxerData, ctx *esContext) (int, error) {
	m.pendingWriteData = &muxerWriteDataState{
		d:                    d,
		ctx:                  ctx,
		payloadStart:         true,
		writeAdaptationField: d.AdaptationField != nil,
	}
	return m.continueWriteData(bytesWritten)
}

func (m *Muxer) continueWriteData(bytesWritten int) (int, error) {
	state := m.pendingWriteData
	d := state.d
	ctx := state.ctx

	for state.payloadBytesWritten < len(d.PES.Data) {
		pktLen := 1 + mpegTsPacketHeaderSize // sync byte + header
		pkt := Packet{
			Header: PacketHeader{
				ContinuityCounter:         uint8(ctx.cc.next()),
				HasAdaptationField:        state.writeAdaptationField,
				HasPayload:                false,
				PayloadUnitStartIndicator: false,
				PID:                       d.PID,
			},
		}

		if state.writeAdaptationField {
			af := *d.AdaptationField
			pkt.AdaptationField = &af
			// one byte for adaptation field length field
			pktLen += 1 + int(calcPacketAdaptationFieldLength(pkt.AdaptationField))
		}

		bytesAvailable := m.packetSize - pktLen
		if state.payloadStart {
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
			streamIDWasZero := d.PES.Header.StreamID == 0
			if streamIDWasZero {
				d.PES.Header.StreamID = ctx.es.StreamType.ToPESStreamID()
			}

			ntot, npayload, err := writePESData(
				m.bufWriter,
				d.PES.Header,
				d.PES.Data[state.payloadBytesWritten:],
				state.payloadStart,
				bytesAvailable,
			)
			if streamIDWasZero {
				d.PES.Header.StreamID = 0
			}
			if err != nil {
				return bytesWritten, err
			}

			payload := append([]byte(nil), m.buf.Bytes()...)
			pkt.Payload = payload

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

			packetBytes := bytes.Buffer{}
			packetWriter := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: &packetBytes})
			if _, err = writePacket(packetWriter, &pkt, m.packetSize); err != nil {
				return bytesWritten, err
			}

			n, err := m.writePacket(packetBytes.Bytes(), func() {
				ctx.cc.inc()
				state.payloadBytesWritten += npayload
				state.payloadStart = false
				state.writeAdaptationField = false
			})
			bytesWritten += n
			if err != nil {
				return bytesWritten, err
			}
		} else {
			packetBytes := bytes.Buffer{}
			packetWriter := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: &packetBytes})
			if _, err := writePacket(packetWriter, &pkt, m.packetSize); err != nil {
				return bytesWritten, err
			}

			n, err := m.writePacket(packetBytes.Bytes(), func() {
				ctx.cc.inc()
				state.writeAdaptationField = false
			})
			bytesWritten += n
			if err != nil {
				return bytesWritten, err
			}
		}
	}

	if d.AdaptationField != nil {
		d.AdaptationField.StuffingLength = 0
	}
	m.pendingWriteData = nil

	return bytesWritten, nil
}

// Writes given packet to MPEG-TS stream
// Stuffs with 0xffs if packet turns out to be shorter than target packet length
func (m *Muxer) WritePacket(p *Packet) (int, error) {
	if m.pendingWriteData != nil || m.pendingTables {
		return 0, ErrWriteInProgress
	}

	if m.pendingPacket != nil {
		n, err := m.flushPendingPacket()
		if err != nil || m.pendingPacket != nil {
			return n, firstError(err, ErrWriteInProgress)
		}
		return n, err
	}

	packetBytes := bytes.Buffer{}
	packetWriter := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: &packetBytes})
	if _, err := writePacket(packetWriter, p, m.packetSize); err != nil {
		return 0, err
	}

	return m.writePacket(packetBytes.Bytes(), nil)
}

func firstError(err error, fallback error) error {
	if err != nil {
		return err
	}
	return fallback
}

func (m *Muxer) writePacket(packet []byte, commit func()) (int, error) {
	n, err := m.w.Write(packet)
	if err != nil {
		return n, err
	}
	if n < 0 || n > len(packet) {
		return n, io.ErrShortWrite
	}
	if n < len(packet) {
		m.pendingPacket = append([]byte(nil), packet[n:]...)
		m.pendingCommit = commit
		m.delayCommit = m.pendingTables
		return n, io.ErrShortWrite
	}

	if commit != nil {
		commit()
	}
	return n, nil
}

func (m *Muxer) flushPendingPacket() (int, error) {
	remaining := m.pendingPacket
	n, err := m.w.Write(remaining)
	if err != nil {
		return n, err
	}
	if n < 0 || n > len(remaining) {
		return n, io.ErrShortWrite
	}

	bytesWritten := n
	if n < len(remaining) {
		m.pendingPacket = append([]byte(nil), remaining[n:]...)
		m.delayCommit = m.pendingTables
		return bytesWritten, io.ErrShortWrite
	}

	commit := m.pendingCommit
	delayed := m.delayCommit
	m.pendingPacket = nil
	m.pendingCommit = nil
	m.delayCommit = false
	if commit != nil {
		if delayed && n < len(remaining) {
			m.pendingCommit = commit
		} else {
			commit()
		}
	}

	return bytesWritten, nil
}

func (m *Muxer) retransmitTables(force bool) (int, error) {
	m.tablesRetransmitCounter++
	if !force && m.tablesRetransmitCounter < m.tablesRetransmitPeriod {
		return 0, nil
	}

	n, err := m.writeTablesInternal()
	if err != nil {
		return n, err
	}

	m.tablesRetransmitCounter = 0
	return n, nil
}

func (m *Muxer) WriteTables() (int, error) {
	if m.pendingPacket != nil {
		if m.pendingTables {
			return m.finishPendingTablesPacket(0)
		}
		bytesWritten, err := m.flushPendingPacket()
		if m.pendingPacket != nil {
			return bytesWritten, firstError(err, ErrWriteInProgress)
		}
	}
	if m.pendingWriteData != nil {
		return 0, ErrWriteInProgress
	}
	if m.pendingTables {
		return m.writePendingTablePacket(0)
	}

	return m.writeTablesInternal()
}

func (m *Muxer) writeTablesInternal() (int, error) {
	bytesWritten := 0
	if m.pendingTables {
		return m.writePendingTablePacket(0)
	}

	patPacket, patCommit, err := m.buildPAT()
	if err != nil {
		return 0, err
	}

	pmtPacket, pmtCommit, err := m.buildPMT()
	if err != nil {
		return 0, err
	}

	m.pendingTables = true
	m.pendingPMT = true
	m.delayCommit = false
	n, err := m.writePacket(patPacket, patCommit)
	bytesWritten += n
	if err != nil {
		return bytesWritten, err
	}

	n, err = m.writePacket(pmtPacket, pmtCommit)
	bytesWritten += n
	if err != nil {
		return bytesWritten, err
	}

	m.pendingTables = false
	m.pendingPMT = false
	return bytesWritten, nil
}

func (m *Muxer) writePendingTablePacket(bytesWritten int) (int, error) {
	if m.pendingPacket != nil {
		return m.finishPendingTablesPacket(bytesWritten)
	}

	return m.writeNextPendingTablePacket(bytesWritten)
}

func (m *Muxer) finishPendingTablesPacket(bytesWritten int) (int, error) {
	n, err := m.flushPendingPacket()
	bytesWritten += n
	if err != nil {
		return bytesWritten, err
	}
	if m.pendingPacket != nil {
		return bytesWritten, nil
	}

	return m.writeNextPendingTablePacket(bytesWritten)
}

func (m *Muxer) writeNextPendingTablePacket(bytesWritten int) (int, error) {
	if m.pendingPMT {
		pmtPacket, pmtCommit, err := m.buildPMT()
		if err != nil {
			return bytesWritten, err
		}

		m.pendingPMT = false
		n, err := m.writePacket(pmtPacket, pmtCommit)
		bytesWritten += n
		if err != nil {
			m.pendingPMT = true
			return bytesWritten, err
		}
	}

	m.pendingTables = false
	m.pendingPMT = false
	return bytesWritten, nil
}

func (m *Muxer) buildPAT() ([]byte, func(), error) {
	d := m.pm.toPATDataUnlocked()
	d.TransportStreamID = m.transportStreamID

	versionNumber := m.patVersion.get()
	if m.pmUpdated {
		versionNumber = m.patVersion.next()
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

	sectionBytes := bytes.Buffer{}
	w := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: &sectionBytes})
	if _, err := writePSIData(w, &psiData); err != nil {
		return nil, nil, err
	}

	packetBytes := bytes.Buffer{}
	wPacket := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: &packetBytes})

	pkt := Packet{
		Header: PacketHeader{
			HasPayload:                true,
			PayloadUnitStartIndicator: true,
			PID:                       PIDPAT,
			ContinuityCounter:         uint8(m.patCC.next()),
		},
		Payload: sectionBytes.Bytes(),
	}
	if _, err := writePacket(wPacket, &pkt, m.packetSize); err != nil {
		return nil, nil, err
	}

	updated := m.pmUpdated
	commit := func() {
		if updated {
			m.patVersion.inc()
		}
		m.patCC.inc()
		m.pmUpdated = false
	}

	return packetBytes.Bytes(), commit, nil
}

func (m *Muxer) generatePAT() error {
	packet, commit, err := m.buildPAT()
	if err != nil {
		return err
	}
	m.patBytes.Reset()
	if _, err = m.patBytes.Write(packet); err != nil {
		return err
	}
	commit()
	return nil
}

func (m *Muxer) buildPMT() ([]byte, func(), error) {
	hasPCRPID := false
	for _, es := range m.pmt.ElementaryStreams {
		if es.ElementaryPID == m.pmt.PCRPID {
			hasPCRPID = true
			break
		}
	}
	if !hasPCRPID {
		return nil, nil, ErrPCRPIDInvalid
	}

	versionNumber := m.pmtVersion.get()
	if m.pmtUpdated {
		versionNumber = m.pmtVersion.next()
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

	sectionBytes := bytes.Buffer{}
	w := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: &sectionBytes})
	if _, err := writePSIData(w, &psiData); err != nil {
		return nil, nil, err
	}

	packetBytes := bytes.Buffer{}
	wPacket := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: &packetBytes})

	pkt := Packet{
		Header: PacketHeader{
			HasPayload:                true,
			PayloadUnitStartIndicator: true,
			PID:                       m.pmtPID, // FIXME multiple programs support
			ContinuityCounter:         uint8(m.pmtCC.next()),
		},
		Payload: sectionBytes.Bytes(),
	}
	if _, err := writePacket(wPacket, &pkt, m.packetSize); err != nil {
		return nil, nil, err
	}

	updated := m.pmtUpdated
	commit := func() {
		if updated {
			m.pmtVersion.inc()
		}
		m.pmtCC.inc()
		m.pmtUpdated = false
	}

	return packetBytes.Bytes(), commit, nil
}

func (m *Muxer) generatePMT() error {
	packet, commit, err := m.buildPMT()
	if err != nil {
		return err
	}
	m.pmtBytes.Reset()
	if _, err = m.pmtBytes.Write(packet); err != nil {
		return err
	}
	commit()
	return nil
}

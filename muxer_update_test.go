package astits

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func videoES(pid uint16) PMTElementaryStream {
	return PMTElementaryStream{ElementaryPID: pid, StreamType: StreamTypeH264Video}
}

func audioES(pid uint16) PMTElementaryStream {
	return PMTElementaryStream{ElementaryPID: pid, StreamType: StreamTypeAACAudio}
}

func writeMuxerData(t *testing.T, m *Muxer, pid uint16) {
	t.Helper()
	_, err := m.WriteData(&MuxerData{
		PID: pid,
		PES: &PESData{
			Data: testPayload(),
			Header: &PESHeader{
				OptionalHeader: &PESOptionalHeader{},
			},
		},
	})
	require.NoError(t, err)
}

// failingWriter fails after accepting failAfter bytes.
type failingWriter struct {
	buf       bytes.Buffer
	failAfter int
	err       error
	disabled  bool
}

func (w *failingWriter) Write(p []byte) (int, error) {
	if w.disabled {
		return w.buf.Write(p)
	}
	if w.buf.Len() >= w.failAfter {
		return 0, w.err
	}
	remaining := w.failAfter - w.buf.Len()
	if len(p) <= remaining {
		return w.buf.Write(p)
	}
	n, _ := w.buf.Write(p[:remaining])
	return n, w.err
}

func TestMuxer_UpdateElementaryStreams_InvalidKeepsOldState(t *testing.T) {
	buf := bytes.Buffer{}
	m := NewMuxer(context.Background(), &buf)
	require.NoError(t, m.AddElementaryStream(videoES(0x1234)))
	m.SetPCRPID(0x1234)

	before := append([]*PMTElementaryStream(nil), m.pmt.ElementaryStreams...)
	beforePCR := m.pmt.PCRPID

	// A PMT that cannot fit a single TS packet must be rejected off-screen.
	tooBig := MuxerStreamsUpdate{PCRPID: 0x1234}
	for pid := uint16(0x0100); pid < 0x0100+40; pid++ {
		tooBig.Add = append(tooBig.Add, audioES(pid))
	}
	err := m.UpdateElementaryStreams(tooBig)
	assert.True(t, errors.Is(err, ErrPMTTooLong))
	assert.False(t, m.TablesDirty())
	assert.Equal(t, before, m.pmt.ElementaryStreams)
	assert.Equal(t, beforePCR, m.pmt.PCRPID)
}

func TestMuxer_UpdateElementaryStreams_ValidationErrors(t *testing.T) {
	buf := bytes.Buffer{}
	m := NewMuxer(context.Background(), &buf)
	require.NoError(t, m.AddElementaryStream(videoES(0x1234)))
	m.SetPCRPID(0x1234)

	before := append([]*PMTElementaryStream(nil), m.pmt.ElementaryStreams...)
	beforePCR := m.pmt.PCRPID

	// Duplicated PID inside the batch.
	err := m.UpdateElementaryStreams(MuxerStreamsUpdate{
		Add:    []PMTElementaryStream{audioES(0x1234)},
		PCRPID: 0x1234,
	})
	assert.True(t, errors.Is(err, ErrPIDAlreadyExists))

	// Removing an unknown PID.
	err = m.UpdateElementaryStreams(MuxerStreamsUpdate{
		Remove: []uint16{0x9999},
		PCRPID: 0x1234,
	})
	assert.True(t, errors.Is(err, ErrPIDNotFound))

	// PCR PID not part of the resulting stream set.
	err = m.UpdateElementaryStreams(MuxerStreamsUpdate{
		Add:    []PMTElementaryStream{audioES(0x0234)},
		PCRPID: 0x0234,
		Remove: []uint16{0x0234},
	})
	assert.True(t, errors.Is(err, ErrPCRPIDInvalid))

	// Nothing changed: same streams, same PCR PID, no pending boundary.
	assert.Equal(t, before, m.pmt.ElementaryStreams)
	assert.Equal(t, beforePCR, m.pmt.PCRPID)
	assert.False(t, m.TablesDirty())
	assert.True(t, errors.Is(m.RollbackElementaryStreamsUpdate(), ErrNoPendingUpdate))
}

func TestMuxer_UpdateElementaryStreams_AtomicBoundary(t *testing.T) {
	buf := bytes.Buffer{}
	m := NewMuxer(context.Background(), &buf)
	require.NoError(t, m.AddElementaryStream(videoES(0x1234)))
	m.SetPCRPID(0x1234)
	require.NoError(t, m.AddElementaryStream(audioES(0x0234)))

	// Emit the initial tables and some old-configuration media.
	_, err := m.WriteTables()
	require.NoError(t, err)
	writeMuxerData(t, m, 0x1234)
	oldMediaLen := buf.Len()
	assert.False(t, m.TablesDirty())

	// Batch switch: drop both streams, introduce a video stream on a new PID and
	// move the PCR to it.
	err = m.UpdateElementaryStreams(MuxerStreamsUpdate{
		Remove: []uint16{0x1234, 0x0234},
		Add:    []PMTElementaryStream{videoES(0x1500)},
		PCRPID: 0x1500,
	})
	require.NoError(t, err)
	assert.True(t, m.TablesDirty())

	// No media of the new PID may be written before the boundary tables.
	_, err = m.WriteData(&MuxerData{PID: 0x1500, PES: &PESData{Data: testPayload(), Header: &PESHeader{OptionalHeader: &PESOptionalHeader{}}}})
	require.NoError(t, err)

	// The two packets right after the old media are exactly one PAT and one PMT
	// packet; only then may the new media packets begin.
	boundary := buf.Bytes()[oldMediaLen : oldMediaLen+2*MpegTsPacketSize]
	assert.Equal(t, byte(syncByte), boundary[0])
	assert.Equal(t, PIDPAT, uint16(boundary[1]&0x1f)<<8|uint16(boundary[2]))
	assert.Equal(t, byte(syncByte), boundary[MpegTsPacketSize])
	firstNewMedia := buf.Bytes()[oldMediaLen+2*MpegTsPacketSize:]
	assert.Equal(t, uint16(0x1500), uint16(firstNewMedia[1]&0x1f)<<8|uint16(firstNewMedia[2]))
	assert.False(t, m.TablesDirty())

	// Parse the boundary and verify internal consistency: the new PMT must only
	// advertise 0x1500, with PCR pointing at it.
	dmx := NewDemuxer(context.Background(), bytes.NewReader(boundary))
	var pmts []*PMTData
	for {
		d, derr := dmx.NextData()
		if errors.Is(derr, ErrNoMorePackets) {
			break
		}
		require.NoError(t, derr)
		if d.PMT != nil {
			pmts = append(pmts, d.PMT)
		}
	}
	require.NotEmpty(t, pmts)
	gotPMT := pmts[len(pmts)-1]
	assert.Equal(t, uint16(0x1500), gotPMT.PCRPID)
	require.Len(t, gotPMT.ElementaryStreams, 1)
	assert.Equal(t, uint16(0x1500), gotPMT.ElementaryStreams[0].ElementaryPID)
	assert.Equal(t, StreamTypeH264Video, gotPMT.ElementaryStreams[0].StreamType)

	// Old PID media is rejected after the boundary, new PID media is accepted.
	_, err = m.WriteData(&MuxerData{PID: 0x1234, PES: &PESData{Data: testPayload(), Header: &PESHeader{OptionalHeader: &PESOptionalHeader{}}}})
	assert.True(t, errors.Is(err, ErrPIDNotFound))
	writeMuxerData(t, m, 0x1500)
}

func TestMuxer_UpdateElementaryStreams_AutoPIDAndRollback(t *testing.T) {
	buf := bytes.Buffer{}
	m := NewMuxer(context.Background(), &buf)
	require.NoError(t, m.AddElementaryStream(videoES(0x1234)))
	m.SetPCRPID(0x1234)
	_, err := m.WriteTables()
	require.NoError(t, err)

	err = m.UpdateElementaryStreams(MuxerStreamsUpdate{
		Add:    []PMTElementaryStream{{StreamType: StreamTypeAACAudio}},
		PCRPID: 0x1234,
	})
	require.NoError(t, err)
	autoPID := m.nextPID - 1
	assert.True(t, m.TablesDirty())

	// Roll back before the boundary reaches the wire.
	require.NoError(t, m.RollbackElementaryStreamsUpdate())
	assert.True(t, m.TablesDirty())
	_, ok := m.esContexts[uint32(autoPID)]
	assert.False(t, ok, "rolled back stream must disappear from the contexts")

	// The next write emits tables describing the original configuration again.
	before := buf.Len()
	writeMuxerData(t, m, 0x1234)
	assert.False(t, m.TablesDirty())
	assert.Greater(t, buf.Len(), before+2*MpegTsPacketSize, "rollback boundary tables must precede media packets")
	// The first two packets after the cut are the rolled-back PAT and PMT.
	assert.Equal(t, byte(syncByte), buf.Bytes()[before])
	assert.Equal(t, byte(syncByte), buf.Bytes()[before+MpegTsPacketSize])

	err = m.RollbackElementaryStreamsUpdate()
	assert.True(t, errors.Is(err, ErrNoPendingUpdate))
}

func TestMuxer_WriteTables_PartialFailureAndRetry(t *testing.T) {
	writerErr := errors.New("disk is full")
	w := &failingWriter{failAfter: 1 * MpegTsPacketSize, err: writerErr}
	m := NewMuxer(context.Background(), w)
	require.NoError(t, m.AddElementaryStream(videoES(0x1234)))
	m.SetPCRPID(0x1234)

	// Counters are lazily initialized: the first successful generation produces
	// version 0 / CC 0.
	const expectedVersion, expectedCC = 0, 0

	// First attempt: the writer accepts the full PAT then fails on the PMT.
	n, err := m.WriteTables()
	var twe *TablesWriteError
	require.True(t, errors.As(err, &twe))
	assert.True(t, errors.Is(err, writerErr))
	assert.Equal(t, 1, twe.PacketsWritten)
	assert.Equal(t, MpegTsPacketSize, n)

	// State did not advance: PMT still marked updated, counters unchanged so a
	// retry produces the same tables.
	assert.True(t, m.pmtUpdated)

	// Fix the writer and retry: a full consistent PAT+PMT pair is emitted. The
	// previously written PAT packet stays at offset 0, so the retry appends a
	// fresh complete pair without losing track of any emitted byte.
	w.disabled = true
	n, err = m.WriteTables()
	require.NoError(t, err)
	assert.Equal(t, 2*MpegTsPacketSize, n)
	assert.Equal(t, expectedVersion, m.pmtVersion.get(), "retry must not skip a table version")
	assert.Equal(t, expectedCC, m.pmtCC.get(), "retry must not skip a continuity counter")

	// The retried PAT+PMT match the canonical bytes (version 0, CC 0), proving no
	// version/CC was consumed by the failed attempt.
	expected := append(patExpectedBytes(0, 0), pmtExpectedBytesVideoOnly(0, 0)...)
	assert.Equal(t, expected, w.buf.Bytes()[MpegTsPacketSize:])
}

func TestMuxer_UpdateElementaryStreams_PartialFailureRollbackAndRetry(t *testing.T) {
	writerErr := errors.New("disk is full")
	w := &failingWriter{failAfter: 3 * MpegTsPacketSize, err: writerErr}
	m := NewMuxer(context.Background(), w)
	require.NoError(t, m.AddElementaryStream(videoES(0x1234)))
	m.SetPCRPID(0x1234)
	_, err := m.WriteTables()
	require.NoError(t, err)

	require.NoError(t, m.UpdateElementaryStreams(MuxerStreamsUpdate{
		Add:    []PMTElementaryStream{audioES(0x0234)},
		Remove: []uint16{0x1234},
		PCRPID: 0x0234,
	}))

	// The boundary write fails after the PAT packet.
	_, err = m.FlushTables()
	var twe *TablesWriteError
	require.True(t, errors.As(err, &twe))
	assert.Equal(t, 1, twe.PacketsWritten)
	assert.True(t, m.TablesDirty(), "failed boundary must stay pending")

	// Roll back: the output can continue with the old configuration.
	require.NoError(t, m.RollbackElementaryStreamsUpdate())
	assert.True(t, m.TablesDirty())

	// Fix the writer: the retry emits a full pair describing the old config.
	w.disabled = true
	n, err := m.FlushTables()
	require.NoError(t, err)
	assert.Equal(t, 2*MpegTsPacketSize, n)
	assert.False(t, m.TablesDirty())

	// The retried PMT is the single-video one (PCR PID 0x1234), not the failed
	// audio-only configuration.
	retryBytes := w.buf.Bytes()[3*MpegTsPacketSize:]
	dmx := NewDemuxer(context.Background(), bytes.NewReader(retryBytes))
	var gotPMT *PMTData
	for {
		d, derr := dmx.NextData()
		if errors.Is(derr, ErrNoMorePackets) {
			break
		}
		require.NoError(t, derr)
		if d.PMT != nil {
			gotPMT = d.PMT
		}
	}
	require.NotNil(t, gotPMT)
	assert.Equal(t, uint16(0x1234), gotPMT.PCRPID)
	require.Len(t, gotPMT.ElementaryStreams, 1)
	assert.Equal(t, uint16(0x1234), gotPMT.ElementaryStreams[0].ElementaryPID)

	// Old stream is back and writable, the new one is gone.
	writeMuxerData(t, m, 0x1234)
	_, err = m.WriteData(&MuxerData{PID: 0x0234, PES: &PESData{Data: testPayload(), Header: &PESHeader{OptionalHeader: &PESOptionalHeader{}}}})
	assert.True(t, errors.Is(err, ErrPIDNotFound))
}

func TestMuxer_UpdateElementaryStreams_ChainedUpdates(t *testing.T) {
	buf := bytes.Buffer{}
	m := NewMuxer(context.Background(), &buf)
	require.NoError(t, m.AddElementaryStream(videoES(0x1234)))
	m.SetPCRPID(0x1234)
	_, err := m.WriteTables()
	require.NoError(t, err)

	// Two pending updates before any boundary is emitted.
	require.NoError(t, m.UpdateElementaryStreams(MuxerStreamsUpdate{
		Add:    []PMTElementaryStream{audioES(0x0234)},
		PCRPID: 0x1234,
	}))
	require.NoError(t, m.UpdateElementaryStreams(MuxerStreamsUpdate{
		Remove: []uint16{0x0234},
		Add:    []PMTElementaryStream{videoES(0x1500)},
		PCRPID: 0x1500,
	}))
	assert.True(t, m.TablesDirty())

	// The single boundary advertises only the final state: 0x1234 + 0x1500.
	writeMuxerData(t, m, 0x1500)
	assert.False(t, m.TablesDirty())

	dmx := NewDemuxer(context.Background(), bytes.NewReader(buf.Bytes()))
	var pmts []*PMTData
	for {
		d, derr := dmx.NextData()
		if errors.Is(derr, ErrNoMorePackets) {
			break
		}
		require.NoError(t, derr)
		if d.PMT != nil {
			pmts = append(pmts, d.PMT)
		}
	}
	gotPMT := pmts[len(pmts)-1]
	assert.Equal(t, uint16(0x1500), gotPMT.PCRPID)
	require.Len(t, gotPMT.ElementaryStreams, 2)
	pids := map[uint16]bool{}
	for _, es := range gotPMT.ElementaryStreams {
		pids[es.ElementaryPID] = true
	}
	assert.True(t, pids[0x1234] && pids[0x1500])
}

func TestMuxer_LegacyAPIIsUnchanged(t *testing.T) {
	buf := bytes.Buffer{}
	m := NewMuxer(context.Background(), &buf)

	// The legacy per-stream API keeps working and never arms a forced boundary.
	require.NoError(t, m.AddElementaryStream(videoES(0x1234)))
	require.NoError(t, m.AddElementaryStream(audioES(0x0234)))
	m.SetPCRPID(0x1234)
	assert.False(t, m.TablesDirty())

	// Legacy errors are preserved.
	assert.True(t, errors.Is(m.AddElementaryStream(videoES(0x1234)), ErrPIDAlreadyExists))
	assert.True(t, errors.Is(m.RemoveElementaryStream(0x9999), ErrPIDNotFound))
	require.NoError(t, m.RemoveElementaryStream(0x0234))
	assert.False(t, m.TablesDirty())

	// A batch update after legacy edits still validates against the live state.
	err := m.UpdateElementaryStreams(MuxerStreamsUpdate{
		Remove: []uint16{0x9999},
		PCRPID: 0x1234,
	})
	assert.True(t, errors.Is(err, ErrPIDNotFound))
	assert.False(t, m.TablesDirty())
}

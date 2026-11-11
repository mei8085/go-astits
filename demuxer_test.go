package astits

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"unicode"

	"github.com/asticode/go-astikit"
	"github.com/stretchr/testify/assert"
)

func hexToBytes(in string) []byte {
	cin := strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, in)
	o, err := hex.DecodeString(cin)
	if err != nil {
		panic(err)
	}
	return o
}

func TestDemuxerNew(t *testing.T) {
	ps := 1
	pp := func(ps []*Packet) (ds []*DemuxerData, skip bool, err error) { return }
	sp := func(p *Packet) bool { return true }
	dmx := NewDemuxer(context.Background(), nil, DemuxerOptPacketSize(ps), DemuxerOptPacketsParser(pp), DemuxerOptPacketSkipper(sp))
	assert.Equal(t, ps, dmx.optPacketSize)
	assert.Equal(t, fmt.Sprintf("%p", pp), fmt.Sprintf("%p", dmx.optPacketsParser))
	assert.Equal(t, fmt.Sprintf("%p", sp), fmt.Sprintf("%p", dmx.optPacketSkipper))
}

func TestDemuxerNextPacket(t *testing.T) {
	// Ctx error
	ctx, cancel := context.WithCancel(context.Background())
	dmx := NewDemuxer(ctx, bytes.NewReader([]byte{}))
	cancel()
	_, err := dmx.NextPacket()
	assert.Error(t, err)

	// Valid
	buf := &bytes.Buffer{}
	w := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: buf})
	b1, p1 := packet(packetHeader, *packetAdaptationField, []byte("1"), true)
	w.Write(b1)
	b2, p2 := packet(packetHeader, *packetAdaptationField, []byte("2"), true)
	w.Write(b2)
	dmx = NewDemuxer(context.Background(), bytes.NewReader(buf.Bytes()))

	// First packet
	p, err := dmx.NextPacket()
	assert.NoError(t, err)
	assert.Equal(t, p1, p)
	assert.Equal(t, 192, dmx.packetBuffer.packetSize)

	// Second packet
	p, err = dmx.NextPacket()
	assert.NoError(t, err)
	assert.Equal(t, p2, p)

	// EOF
	_, err = dmx.NextPacket()
	assert.EqualError(t, err, ErrNoMorePackets.Error())
}

func TestDemuxerNextData(t *testing.T) {
	// Init
	buf := &bytes.Buffer{}
	w := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: buf})
	b := psiBytes()
	b1, _ := packet(PacketHeader{ContinuityCounter: uint8(0), PayloadUnitStartIndicator: true, PID: PIDPAT}, PacketAdaptationField{}, b[:147], true)
	w.Write(b1)
	b2, _ := packet(PacketHeader{ContinuityCounter: uint8(1), PID: PIDPAT}, PacketAdaptationField{}, b[147:], true)
	w.Write(b2)
	dmx := NewDemuxer(context.Background(), bytes.NewReader(buf.Bytes()))
	p, err := dmx.NextPacket()
	assert.NoError(t, err)
	_, err = dmx.Rewind()
	assert.NoError(t, err)

	// Next data
	var ds []*DemuxerData
	for _, s := range psi.Sections {
		if !s.Header.TableID.isUnknown() {
			d, err := dmx.NextData()
			assert.NoError(t, err)
			ds = append(ds, d)
		}
	}
	assert.Equal(t, psi.toData(
		&Packet{Header: p.Header, AdaptationField: p.AdaptationField},
		PIDPAT,
	), ds)
	assert.Equal(t, map[uint32]uint16{0x3: 0x2, 0x5: 0x4}, dmx.programMap.p)

	// No more packets
	_, err = dmx.NextData()
	assert.EqualError(t, err, ErrNoMorePackets.Error())
}

func TestDemuxerNextDataUnknownDataPackets(t *testing.T) {
	buf := &bytes.Buffer{}
	bufWriter := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: buf})

	// Packet that isn't a data packet (PSI or PES)
	b1, _ := packet(PacketHeader{
		ContinuityCounter:         uint8(0),
		PID:                       256,
		PayloadUnitStartIndicator: true,
		HasPayload:                true,
	}, PacketAdaptationField{}, []byte{0x01, 0x02, 0x03, 0x04}, true)
	bufWriter.Write(b1)

	// The demuxer must return "no more packets"
	dmx := NewDemuxer(context.Background(), bytes.NewReader(buf.Bytes()),
		DemuxerOptPacketSize(188))
	d, err := dmx.NextData()
	assert.Equal(t, (*DemuxerData)(nil), d)
	assert.EqualError(t, err, ErrNoMorePackets.Error())
}

func TestDemuxerNextDataPATPMT(t *testing.T) {
	pat := hexToBytes(`474000100000b00d0001c100000001f0002ab104b2ffffffffffffffff
	ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff
	ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff
	ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff
	ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff
	ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff
	ffffffffffffffffff`)
	pmt := hexToBytes(`475000100002b0170001c10000e100f0001be100f0000fe101f0002f44
	b99bffffffffffffffffffffffffffffffffffffffffffffffffffffffff
	ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff
	ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff
	ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff
	ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff
	ffffffffffffffffff`)
	r := bytes.NewReader(append(pat, pmt...))
	dmx := NewDemuxer(context.Background(), r, DemuxerOptPacketSize(188))
	assert.Equal(t, 188*2, r.Len())

	d, err := dmx.NextData()
	assert.NoError(t, err)
	assert.Equal(t, uint16(0), d.FirstPacket.Header.PID)
	assert.NotNil(t, d.PAT)
	assert.Equal(t, 188, r.Len())

	d, err = dmx.NextData()
	assert.NoError(t, err)
	assert.Equal(t, uint16(0x1000), d.FirstPacket.Header.PID)
	assert.NotNil(t, d.PMT)
}

func patPacketBytes() []byte {
	return hexToBytes(`474000100000b00d0001c100000001f0002ab104b2ffffffffffffffff
	ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff
	ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff
	ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff
	ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff
	ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff
	ffffffffffffffffff`)
}

func pmtPacketBytes() []byte {
	return hexToBytes(`475000100002b0170001c10000e100f0001be100f0000fe101f0002f44
	b99bffffffffffffffffffffffffffffffffffffffffffffffffffffffff
	ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff
	ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff
	ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff
	ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff
	ffffffffffffffffff`)
}

func TestDemuxerNextDataResync(t *testing.T) {
	// Build a stream made of:
	//   - a PAT and a PMT packet
	//   - the first packet of a PES payload that will never be completed
	//   - 37 garbage bytes, one of them being a sync byte to make sure random
	//     sync bytes are not mistaken for packet headers
	//   - a complete PES packet
	//   - a PAT packet
	pesIncomplete, _ := resyncPacket(PacketHeader{
		ContinuityCounter:         0,
		HasPayload:                true,
		PayloadUnitStartIndicator: true,
		PID:                       0x100,
	}, hexToBytes(`000001be01f4`)) // PES packet length is 500, the payload is incomplete
	pesComplete, _ := resyncPacket(PacketHeader{
		ContinuityCounter:         1,
		HasPayload:                true,
		PayloadUnitStartIndicator: true,
		PID:                       0x100,
	}, hexToBytes(`000001be0004deadbeef`))
	garbage := make([]byte, 37)
	garbage[1] = syncByte
	var stream []byte
	stream = append(stream, patPacketBytes()...)
	stream = append(stream, pmtPacketBytes()...)
	stream = append(stream, pesIncomplete...)
	stream = append(stream, garbage...)
	stream = append(stream, pesComplete...)
	stream = append(stream, patPacketBytes()...)

	// Without resync, fetching data fails
	dmx := NewDemuxer(context.Background(), bytes.NewReader(stream), DemuxerOptPacketSize(188))
	d, err := dmx.NextData()
	assert.NoError(t, err)
	assert.NotNil(t, d.PAT)
	d, err = dmx.NextData()
	assert.NoError(t, err)
	assert.NotNil(t, d.PMT)
	_, err = dmx.NextData()
	assert.True(t, errors.Is(err, ErrPacketMustStartWithASyncByte))

	// With resync, complete data keeps being delivered after the desync
	dmx = NewDemuxer(context.Background(), bytes.NewReader(stream), DemuxerOptPacketSize(188), DemuxerOptResync(1024, 2048))
	d, err = dmx.NextData()
	assert.NoError(t, err)
	assert.NotNil(t, d.PAT)
	d, err = dmx.NextData()
	assert.NoError(t, err)
	assert.NotNil(t, d.PMT)

	// The incomplete PES payload has been dropped, the complete one is delivered
	d, err = dmx.NextData()
	assert.NoError(t, err)
	assert.Equal(t, uint16(0x100), d.PID)
	assert.NotNil(t, d.PES)
	assert.Equal(t, hexToBytes(`deadbeef`), d.PES.Data)

	// The program and stream maps have been kept
	assert.True(t, dmx.programMap.existsUnlocked(0x1000))
	assert.True(t, dmx.streamMap.existsUnlocked(0x100))

	// Data following the desync is delivered
	d, err = dmx.NextData()
	assert.NoError(t, err)
	assert.NotNil(t, d.PAT)

	// No more packets
	_, err = dmx.NextData()
	assert.EqualError(t, err, ErrNoMorePackets.Error())
}

func TestDemuxerNextDataResyncSyncedStream(t *testing.T) {
	// Enabling resync on a synced stream doesn't change the output
	r := bytes.NewReader(append(patPacketBytes(), pmtPacketBytes()...))
	dmx := NewDemuxer(context.Background(), r, DemuxerOptPacketSize(188), DemuxerOptResync(1024, 2048))

	d, err := dmx.NextData()
	assert.NoError(t, err)
	assert.Equal(t, uint16(0), d.FirstPacket.Header.PID)
	assert.NotNil(t, d.PAT)

	d, err = dmx.NextData()
	assert.NoError(t, err)
	assert.Equal(t, uint16(0x1000), d.FirstPacket.Header.PID)
	assert.NotNil(t, d.PMT)

	_, err = dmx.NextData()
	assert.EqualError(t, err, ErrNoMorePackets.Error())
}

func TestDemuxerRewind(t *testing.T) {
	r := bytes.NewReader([]byte("content"))
	dmx := NewDemuxer(context.Background(), r)
	dmx.packetPool.addUnlocked(&Packet{Header: PacketHeader{PID: 1}})
	dmx.dataBuffer = append(dmx.dataBuffer, &DemuxerData{})
	b := make([]byte, 2)
	_, err := r.Read(b)
	assert.NoError(t, err)
	n, err := dmx.Rewind()
	assert.NoError(t, err)
	assert.Equal(t, int64(0), n)
	assert.Equal(t, 7, r.Len())
	assert.Equal(t, 0, len(dmx.dataBuffer))
	assert.Equal(t, 0, len(dmx.packetPool.b))
	assert.Nil(t, dmx.packetBuffer)
}

func BenchmarkDemuxer_NextData(b *testing.B) {
	b.ReportAllocs()

	buf := &bytes.Buffer{}
	w := astikit.NewBitsWriter(astikit.BitsWriterOptions{Writer: buf})
	bs := psiBytes()
	b1, _ := packet(PacketHeader{ContinuityCounter: uint8(0), PayloadUnitStartIndicator: true, PID: PIDPAT}, PacketAdaptationField{}, bs[:147], true)
	w.Write(b1)
	b2, _ := packet(PacketHeader{ContinuityCounter: uint8(1), PID: PIDPAT}, PacketAdaptationField{}, bs[147:], true)
	w.Write(b2)

	r := bytes.NewReader(buf.Bytes())
	dmx := NewDemuxer(context.Background(), r)

	for i := 0; i < b.N; i++ {
		r.Seek(0, io.SeekStart)
		for _, s := range psi.Sections {
			if !s.Header.TableID.isUnknown() {
				dmx.NextData()
			}
		}
	}
}

func FuzzDemuxer(f *testing.F) {
	f.Fuzz(func(t *testing.T, b []byte) {
		r := bytes.NewReader(b)
		dmx := NewDemuxer(context.Background(), r, DemuxerOptPacketSize(188))
		for {
			_, err := dmx.NextData()
			if err == ErrNoMorePackets {
				break
			}
		}
	})
}

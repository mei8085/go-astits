package astits

import (
	"encoding/binary"
	"sort"
)

const (
	// DefaultPacketPoolHighWaterMark is the default packet count at which
	// inactive packet accumulators can be reclaimed.
	DefaultPacketPoolHighWaterMark = 4096

	// DefaultPacketPoolInactivityLimit is the default number of transport
	// packets that a PID may remain untouched before it is eligible for
	// reclamation once the pool high-water mark is reached.
	DefaultPacketPoolInactivityLimit = 1024
)

// Reasons for discarding an incomplete payload.
const (
	PacketPoolDropReasonEndOfInput             = "end_of_input"
	PacketPoolDropReasonInactivity             = "inactivity"
	PacketPoolDropReasonNewPayloadUnit         = "new_payload_unit"
	PacketPoolDropReasonStreamSwitch           = "stream_switch"
	PacketPoolDropReasonTransportDiscontinuity = "transport_discontinuity"
)

// PacketPoolStats describes packet pool buffering and recovery.
type PacketPoolStats struct {
	BufferedBytes           int
	BufferedBytesPerPID     map[uint32]int
	BufferedPackets         int
	BufferedPacketsPerPID   map[uint32]int
	DroppedBytes            int
	DroppedPackets          int
	DroppedPayloads         int
	DroppedPayloadsByReason map[string]int
	HighWaterMarkBytes      int
	HighWaterMarkPackets    int
	InactivityLimit         int
	ReclaimHighWaterMark    int
}

// packetAccumulator keeps track of packets for a single PID and decides when to flush them
type packetAccumulator struct {
	lastSeen     uint64
	pid          uint16
	programMap   *programMap
	resetPending bool
	streamMap    *elementaryStreamMap
	q            []*Packet
}

// newPacketAccumulator creates a new packet queue for a single PID
func newPacketAccumulator(pid uint16, programMap *programMap, streamMap *elementaryStreamMap) *packetAccumulator {
	return &packetAccumulator{
		pid:        pid,
		programMap: programMap,
		streamMap:  streamMap,
	}
}

// add adds a new packet for this PID to the queue. The returned packets, when
// non-empty, form one complete bounded payload or an unbounded PES ended by its
// next payload-unit-start indicator.
func (b *packetAccumulator) add(p *Packet) (ps, dropped, orphaned []*Packet) {
	mps := b.q

	// A transport discontinuity means the old payload cannot be continued.
	if hasDiscontinuity(mps, p) {
		dropped = mps
		b.reset()
		mps = nil
		b.resetPending = true
	}

	// Flush the previous payload when a new payload starts.
	startedNewPayload := p.Header.PayloadUnitStartIndicator
	if p.Header.PayloadUnitStartIndicator {
		ps = mps
		mps = make([]*Packet, 0, cap(mps))
	}

	// A continuation packet after a reset has no payload start and cannot
	// identify a complete unit. Wait for the next payload-unit-start indicator.
	if len(mps) == 0 && len(b.q) == 0 && !p.Header.PayloadUnitStartIndicator && b.resetPending {
		return nil, dropped, []*Packet{p}
	}

	// A repeated transport packet carries no additional payload.
	if isSameAsPrevious(mps, p) {
		if startedNewPayload {
			b.q = mps
			return ps, dropped, nil
		}
		return nil, dropped, nil
	}

	mps = append(mps, p)
	b.resetPending = false

	// If this packet started a new payload, keep it queued even when the old
	// payload was returned by this same call.
	if len(ps) > 0 {
		b.q = mps
		return ps, dropped, nil
	}

	if b.isComplete(mps) {
		ps = mps
		mps = nil
	}

	b.q = mps
	return ps, dropped, nil
}

func (b *packetAccumulator) isComplete(ps []*Packet) bool {
	if len(ps) == 0 {
		return false
	}
	if b.isPSI(b.pid) && isPSIComplete(ps) {
		return true
	}

	return isPESPayload(ps[0].Payload) && (isPESComplete(ps) || hasPESReachedDeclaredLength(ps))
}

func (b *packetAccumulator) reset() {
	b.q = b.q[:0]
	b.resetPending = false
}

func (b *packetAccumulator) isIncomplete(ps []*Packet) bool {
	if len(ps) == 0 {
		return false
	}
	if ps[0] == nil {
		return true
	}

	// Unbounded video PES has no declared length. A new payload start is the
	// only in-band delimiter, so the previous unit is complete.
	if isUnboundedPES(ps) {
		return false
	}

	// PSI payloads are flushed as soon as their sections are complete. A PSI
	// payload replaced by a start indicator was therefore an incomplete section.
	if b.isPSI(ps[0].Header.PID) {
		return !isPSIComplete(ps)
	}

	// Unknown payloads have no in-band delimiter beyond the next payload-unit
	// start. Preserve the historical flush behavior for them.
	if !isPESPayload(ps[0].Payload) {
		return false
	}

	// A bounded PES that reached its declared length is complete, even if the
	// transport stream includes padding before the next payload start.
	if hasPESReachedDeclaredLength(ps) {
		return false
	}

	// A malformed prefix or unparsable header cannot identify a complete unit.
	// isPESComplete has already validated the header for bounded PES.
	return true
}

func hasPESReachedDeclaredLength(ps []*Packet) bool {
	var header [6]byte
	var n int
	for _, p := range ps {
		n += copy(header[n:], p.Payload)
		if n == len(header) {
			break
		}
	}
	if n < len(header) {
		return false
	}

	declaredLength := int(binary.BigEndian.Uint16(header[4:]))
	if declaredLength == 0 {
		return false
	}

	var payloadLength int
	for _, p := range ps {
		payloadLength += len(p.Payload)
	}

	return payloadLength >= pesHeaderLength+declaredLength
}

func (b *packetAccumulator) isPSI(pid uint16) bool {
	return pid == PIDPAT ||
		(b.programMap != nil && b.programMap.existsUnlocked(pid)) ||
		(((pid >= 0x10 && pid <= 0x14) || (pid >= 0x1e && pid <= 0x1f)) &&
			(b.streamMap == nil || !b.streamMap.existsUnlocked(pid)))
}

func isUnboundedPES(ps []*Packet) bool {
	if len(ps) == 0 || !isPESPayload(ps[0].Payload) {
		return false
	}

	var header [6]byte
	var n int
	for _, p := range ps {
		n += copy(header[n:], p.Payload)
		if n == len(header) {
			break
		}
	}

	return n == len(header) && header[4] == 0 && header[5] == 0
}

// packetPool represents a queue of packets for each PID in the stream
type packetPool struct {
	// We use map[uint32] instead map[uint16] as go runtime provide optimized hash functions for (u)int32/64 keys
	b map[uint32]*packetAccumulator // Indexed by PID

	dropReasons      map[string]int
	highWaterBytes   int
	highWaterPackets int
	inactivityLimit  int
	packetCount      int
	packetCountByPID map[uint32]int
	packetBytesByPID map[uint32]int
	programMap       *programMap
	streamMap        *elementaryStreamMap
	reclaimWaterMark int
	sequence         uint64
	totalBytes       int
	droppedBytes     int
	droppedPackets   int
	droppedPayloads  int
}

// newPacketPool creates a new packet pool with an optional parser and programMap
func newPacketPool(programMap *programMap) *packetPool {
	return newPacketPoolWithLimits(programMap, nil, DefaultPacketPoolHighWaterMark, DefaultPacketPoolInactivityLimit)
}

func newPacketPoolWithLimits(programMap *programMap, streamMap *elementaryStreamMap, highWaterMark, inactivityLimit int) *packetPool {
	if highWaterMark == 0 {
		highWaterMark = DefaultPacketPoolHighWaterMark
	}
	if inactivityLimit == 0 {
		inactivityLimit = DefaultPacketPoolInactivityLimit
	}

	return &packetPool{
		b:                make(map[uint32]*packetAccumulator),
		dropReasons:      make(map[string]int),
		inactivityLimit:  inactivityLimit,
		packetCountByPID: make(map[uint32]int),
		packetBytesByPID: make(map[uint32]int),
		programMap:       programMap,
		streamMap:        streamMap,
		reclaimWaterMark: highWaterMark,
	}
}

// addUnlocked adds a new packet to the pool
func (b *packetPool) addUnlocked(p *Packet) (ps []*Packet) {
	// Throw away packet if error indicator
	if p.Header.TransportErrorIndicator {
		return
	}

	// Throw away packets that don't have a payload until we figure out what we're going to do with them
	// TODO figure out what we're going to do with them :D
	if !p.Header.HasPayload {
		return
	}

	b.reclaimInactive()

	pid := uint32(p.Header.PID)
	acc, ok := b.b[pid]
	if !ok {
		acc = newPacketAccumulator(p.Header.PID, b.programMap, b.streamMap)
		b.b[pid] = acc
	}

	oldLen := len(acc.q)
	oldBytes := payloadSize(acc.q)
	ps, droppedPayload, orphanedPayload := acc.add(p)

	if len(droppedPayload) > 0 {
		b.dropPacketsWithoutAccounting(pid, droppedPayload, PacketPoolDropReasonTransportDiscontinuity)
	}
	if len(orphanedPayload) > 0 {
		b.dropPacketsWithoutAccounting(pid, orphanedPayload, PacketPoolDropReasonTransportDiscontinuity)
	}

	// A new payload start may replace an incomplete queued payload. It can also
	// complete an unbounded PES, in which case the returned packets are valid.
	if len(ps) > 0 {
		if acc.isIncomplete(ps) {
			b.dropPacketsWithoutAccounting(pid, ps, PacketPoolDropReasonNewPayloadUnit)
			ps = nil
		} else {
			// Accounting is reconciled from the accumulator below.
		}
	}

	// Reconcile this PID's accounting from the accumulator. At this point
	// acc.q contains only packets retained for this PID.
	b.totalBytes += payloadSize(acc.q) - oldBytes
	b.packetCount += len(acc.q) - oldLen
	b.packetBytesByPID[pid] = payloadSize(acc.q)
	if len(acc.q) == 0 {
		delete(b.packetCountByPID, pid)
		delete(b.packetBytesByPID, pid)
	} else {
		b.packetCountByPID[pid] = len(acc.q)
	}
	b.sequence++
	acc.lastSeen = b.sequence
	b.updateHighWaterMark()

	return ps
}

// drainCompleteUnlocked returns and removes complete payloads at end of input.
// Incomplete payloads are explicitly discarded.
func (b *packetPool) drainCompleteUnlocked() (ps []*Packet) {
	var keys []int
	for k := range b.b {
		keys = append(keys, int(k))
	}
	sort.Ints(keys)

	for _, k := range keys {
		pid := uint32(k)
		acc := b.b[pid]
		queued := acc.q
		if len(queued) == 0 {
			delete(b.b, pid)
			continue
		}

		if len(queued) > 0 && acc.isComplete(queued) {
			ps = queued
			acc.reset()
			b.totalBytes -= payloadSize(queued)
			b.packetCount -= len(queued)
			delete(b.b, pid)
			delete(b.packetCountByPID, pid)
			delete(b.packetBytesByPID, pid)
			return ps
		}

		if len(queued) > 0 {
			b.dropPacketsWithoutAccounting(pid, queued, PacketPoolDropReasonEndOfInput)
		}
		acc.reset()
		b.totalBytes -= payloadSize(queued)
		b.packetCount -= len(queued)
		delete(b.b, pid)
		delete(b.packetCountByPID, pid)
		delete(b.packetBytesByPID, pid)
	}

	return ps
}

// dumpUnlocked preserves the historical pool entry point while applying the
// current end-of-input rule: complete payloads are returned, incomplete
// payloads are discarded with an explicit reason.
func (b *packetPool) dumpUnlocked() (ps []*Packet) {
	return b.drainCompleteUnlocked()
}

func (b *packetPool) reclaimInactive() {
	if b.reclaimWaterMark < 0 || b.inactivityLimit < 0 || b.packetCount <= b.reclaimWaterMark {
		return
	}

	var pids []uint32
	for pid, acc := range b.b {
		if b.sequence-acc.lastSeen >= uint64(b.inactivityLimit) && len(acc.q) > 0 {
			pids = append(pids, pid)
		}
	}
	sort.Slice(pids, func(i, j int) bool {
		return b.b[pids[i]].lastSeen < b.b[pids[j]].lastSeen
	})

	for _, pid := range pids {
		if b.packetCount <= b.reclaimWaterMark {
			break
		}

		acc := b.b[pid]
		b.dropPacketsWithoutAccounting(pid, acc.q, PacketPoolDropReasonInactivity)
		queued := acc.q
		acc.reset()
		b.totalBytes -= payloadSize(queued)
		b.packetCount -= len(queued)
		delete(b.b, pid)
		delete(b.packetCountByPID, pid)
		delete(b.packetBytesByPID, pid)
	}
}

func (b *packetPool) dropPIDUnlocked(pid uint32, reason string) {
	acc, ok := b.b[pid]
	if !ok || len(acc.q) == 0 {
		return
	}

	queued := acc.q
	b.dropPacketsWithoutAccounting(pid, queued, reason)
	acc.reset()
	b.totalBytes -= payloadSize(queued)
	b.packetCount -= len(queued)
	delete(b.b, pid)
	delete(b.packetCountByPID, pid)
	delete(b.packetBytesByPID, pid)
}

func (b *packetPool) dropPacketsWithoutAccounting(pid uint32, ps []*Packet, reason string) {
	if len(ps) == 0 {
		return
	}

	b.droppedBytes += payloadSize(ps)
	b.droppedPackets += len(ps)
	b.droppedPayloads++
	b.dropReasons[reason]++
}

func (b *packetPool) updateHighWaterMark() {
	if b.packetCount > b.highWaterPackets {
		b.highWaterPackets = b.packetCount
	}
	if b.totalBytes > b.highWaterBytes {
		b.highWaterBytes = b.totalBytes
	}
}

func payloadSize(ps []*Packet) int {
	var total int
	for _, p := range ps {
		if p == nil {
			continue
		}
		total += len(p.Payload)
	}
	return total
}

// Stats returns a snapshot of pool usage and dropped incomplete payloads.
func (b *packetPool) Stats() PacketPoolStats {
	reasons := make(map[string]int, len(b.dropReasons))
	for reason, count := range b.dropReasons {
		reasons[reason] = count
	}

	packetsPerPID := make(map[uint32]int, len(b.packetCountByPID))
	bytesPerPID := make(map[uint32]int, len(b.packetCountByPID))
	for pid, count := range b.packetCountByPID {
		packetsPerPID[pid] = count
	}
	for pid, size := range b.packetBytesByPID {
		bytesPerPID[pid] = size
	}

	return PacketPoolStats{
		BufferedBytes:           b.totalBytes,
		BufferedBytesPerPID:     bytesPerPID,
		BufferedPackets:         b.packetCount,
		BufferedPacketsPerPID:   packetsPerPID,
		DroppedBytes:            b.droppedBytes,
		DroppedPackets:          b.droppedPackets,
		DroppedPayloads:         b.droppedPayloads,
		DroppedPayloadsByReason: reasons,
		HighWaterMarkBytes:      b.highWaterBytes,
		HighWaterMarkPackets:    b.highWaterPackets,
		InactivityLimit:         b.inactivityLimit,
		ReclaimHighWaterMark:    b.reclaimWaterMark,
	}
}

// hasDiscontinuity checks whether a packet is discontinuous with a set of packets
func hasDiscontinuity(ps []*Packet, p *Packet) bool {
	if p.Header.PID == PIDNull {
		return false
	}

	if p.Header.HasAdaptationField && p.AdaptationField != nil && p.AdaptationField.DiscontinuityIndicator {
		return true
	}

	l := len(ps)
	if l == 0 {
		return false
	}

	var expectedContinuityCounter uint8
	if p.Header.HasPayload {
		expectedContinuityCounter = (ps[l-1].Header.ContinuityCounter + 1) & 0x0f
	} else {
		expectedContinuityCounter = ps[l-1].Header.ContinuityCounter
	}
	if expectedContinuityCounter == p.Header.ContinuityCounter {
		return false
	}

	return true
}

// isSameAsPrevious checks whether a packet is the same as the last packet of a set of packets
func isSameAsPrevious(ps []*Packet, p *Packet) bool {
	l := len(ps)
	return l > 0 && p.Header.HasPayload && p.Header.ContinuityCounter == ps[l-1].Header.ContinuityCounter
}

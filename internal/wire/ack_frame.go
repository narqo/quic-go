package wire

import (
	"bytes"
	"errors"
	"sort"
	"time"

	"github.com/lucas-clemente/quic-go/internal/protocol"
	"github.com/lucas-clemente/quic-go/internal/utils"
)

// TODO: use the value sent in the transport parameters
const ackDelayExponent = 3

// An AckFrame is an ACK frame
type AckFrame struct {
	AckRanges []AckRange // has to be ordered. The highest ACK range goes first, the lowest ACK range goes last

	// time when the LargestAcked was receiveid
	// this field will not be set for received ACKs frames
	PacketReceivedTime time.Time
	DelayTime          time.Duration
}

// parseAckFrame reads an ACK frame
func parseAckFrame(r *bytes.Reader, version protocol.VersionNumber) (*AckFrame, error) {
	if !version.UsesIETFFrameFormat() {
		return parseAckFrameLegacy(r, version)
	}

	if _, err := r.ReadByte(); err != nil {
		return nil, err
	}

	frame := &AckFrame{}

	la, err := utils.ReadVarInt(r)
	if err != nil {
		return nil, err
	}
	largestAcked := protocol.PacketNumber(la)
	delay, err := utils.ReadVarInt(r)
	if err != nil {
		return nil, err
	}
	frame.DelayTime = time.Duration(delay*1<<ackDelayExponent) * time.Microsecond
	numBlocks, err := utils.ReadVarInt(r)
	if err != nil {
		return nil, err
	}

	// read the first ACK range
	ab, err := utils.ReadVarInt(r)
	if err != nil {
		return nil, err
	}
	ackBlock := protocol.PacketNumber(ab)
	if ackBlock > largestAcked {
		return nil, errors.New("invalid first ACK range")
	}
	smallest := largestAcked - ackBlock

	// read all the other ACK ranges
	frame.AckRanges = append(frame.AckRanges, AckRange{Smallest: smallest, Largest: largestAcked})
	for i := uint64(0); i < numBlocks; i++ {
		g, err := utils.ReadVarInt(r)
		if err != nil {
			return nil, err
		}
		gap := protocol.PacketNumber(g)
		if smallest < gap+2 {
			return nil, errInvalidAckRanges
		}
		largest := smallest - gap - 2

		ab, err := utils.ReadVarInt(r)
		if err != nil {
			return nil, err
		}
		ackBlock := protocol.PacketNumber(ab)

		if ackBlock > largest {
			return nil, errInvalidAckRanges
		}
		smallest = largest - ackBlock
		frame.AckRanges = append(frame.AckRanges, AckRange{Smallest: smallest, Largest: largest})
	}

	if !frame.validateAckRanges() {
		return nil, errInvalidAckRanges
	}
	return frame, nil
}

// Write writes an ACK frame.
func (f *AckFrame) Write(b *bytes.Buffer, version protocol.VersionNumber) error {
	if !version.UsesIETFFrameFormat() {
		return f.writeLegacy(b, version)
	}

	largestAcked := f.AckRanges[0].Largest
	lowestInFirstRange := f.AckRanges[0].Smallest

	b.WriteByte(0x0d)
	utils.WriteVarInt(b, uint64(largestAcked))
	utils.WriteVarInt(b, encodeAckDelay(f.DelayTime))

	// TODO: limit the number of ACK ranges, such that the frame doesn't grow larger than an upper bound
	utils.WriteVarInt(b, uint64(len(f.AckRanges)-1))

	// write the first range
	utils.WriteVarInt(b, uint64(largestAcked-lowestInFirstRange))

	// write all the other range
	if f.HasMissingRanges() {
		var lowest protocol.PacketNumber
		for i, ackRange := range f.AckRanges {
			if i == 0 {
				lowest = lowestInFirstRange
				continue
			}
			utils.WriteVarInt(b, uint64(lowest-ackRange.Largest-2))
			utils.WriteVarInt(b, uint64(ackRange.Largest-ackRange.Smallest))
			lowest = ackRange.Smallest
		}
	}
	return nil
}

// Length of a written frame
func (f *AckFrame) Length(version protocol.VersionNumber) protocol.ByteCount {
	if !version.UsesIETFFrameFormat() {
		return f.lengthLegacy(version)
	}

	largestAcked := f.AckRanges[0].Largest
	length := 1 + utils.VarIntLen(uint64(largestAcked)) + utils.VarIntLen(encodeAckDelay(f.DelayTime))

	length += utils.VarIntLen(uint64(len(f.AckRanges) - 1))
	lowestInFirstRange := f.AckRanges[0].Smallest
	length += utils.VarIntLen(uint64(largestAcked - lowestInFirstRange))

	if !f.HasMissingRanges() {
		return length
	}
	var lowest protocol.PacketNumber
	for i, ackRange := range f.AckRanges {
		if i == 0 {
			lowest = ackRange.Smallest
			continue
		}
		length += utils.VarIntLen(uint64(lowest - ackRange.Largest - 2))
		length += utils.VarIntLen(uint64(ackRange.Largest - ackRange.Smallest))
		lowest = ackRange.Smallest
	}
	return length
}

// HasMissingRanges returns if this frame reports any missing packets
func (f *AckFrame) HasMissingRanges() bool {
	return len(f.AckRanges) > 1
}

func (f *AckFrame) validateAckRanges() bool {
	if len(f.AckRanges) == 0 {
		return false
	}

	// check the validity of every single ACK range
	for _, ackRange := range f.AckRanges {
		if ackRange.Smallest > ackRange.Largest {
			return false
		}
	}

	// check the consistency for ACK with multiple NACK ranges
	for i, ackRange := range f.AckRanges {
		if i == 0 {
			continue
		}
		lastAckRange := f.AckRanges[i-1]
		if lastAckRange.Smallest <= ackRange.Smallest {
			return false
		}
		if lastAckRange.Smallest <= ackRange.Largest+1 {
			return false
		}
	}

	return true
}

// LargestAcked is the largest acked packet number
func (f *AckFrame) LargestAcked() protocol.PacketNumber {
	return f.AckRanges[0].Largest
}

// LowestAcked is the lowest acked packet number
func (f *AckFrame) LowestAcked() protocol.PacketNumber {
	return f.AckRanges[len(f.AckRanges)-1].Smallest
}

// AcksPacket determines if this ACK frame acks a certain packet number
func (f *AckFrame) AcksPacket(p protocol.PacketNumber) bool {
	if p < f.LowestAcked() || p > f.LargestAcked() {
		return false
	}

	i := sort.Search(len(f.AckRanges), func(i int) bool {
		return p >= f.AckRanges[i].Smallest
	})
	// i will always be < len(f.AckRanges), since we checked above that p is not bigger than the largest acked
	return p <= f.AckRanges[i].Largest
}

func encodeAckDelay(delay time.Duration) uint64 {
	return uint64(delay.Nanoseconds() / (1000 * (1 << ackDelayExponent)))
}

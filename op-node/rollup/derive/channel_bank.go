package derive

import (
	"bytes"
	"context"
	"fmt"
	"io"

	"github.com/ethereum-optimism/optimism/op-node/eth"
	"github.com/ethereum-optimism/optimism/op-node/rollup"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/log"
)

type ChannelBankOutput interface {
	StageProgress
	WriteChannel(data []byte)
}

// ChannelBank buffers channel frames, and emits full channel data
type ChannelBank struct {
	log log.Logger
	cfg *rollup.Config

	channels     map[ChannelID]*ChannelIn // channels by ID
	channelQueue []ChannelID              // channels in FIFO order

	resetting bool

	progress Progress

	next ChannelBankOutput
}

var _ Stage = (*ChannelBank)(nil)

// NewChannelBank creates a ChannelBank, which should be Reset(origin) before use.
func NewChannelBank(log log.Logger, cfg *rollup.Config, next ChannelBankOutput) *ChannelBank {
	return &ChannelBank{
		log:          log,
		cfg:          cfg,
		channels:     make(map[ChannelID]*ChannelIn),
		channelQueue: make([]ChannelID, 0, 10),
		next:         next,
	}
}

func (ib *ChannelBank) Progress() Progress {
	return ib.progress
}

func (ib *ChannelBank) prune() {
	// check total size
	totalSize := uint64(0)
	for _, ch := range ib.channels {
		totalSize += ch.size
	}
	// prune until it is reasonable again. The high-priority channel failed to be read, so we start pruning there.
	for totalSize > MaxChannelBankSize {
		id := ib.channelQueue[0]
		ch := ib.channels[id]
		ib.channelQueue = ib.channelQueue[1:]
		delete(ib.channels, id)
		totalSize -= ch.size
	}
}

// IngestData adds new L1 data to the channel bank.
// Read() should be called repeatedly first, until everything has been read, before adding new data.
// Then NextL1(ref) should be called to move forward to the next L1 input
func (ib *ChannelBank) IngestData(data []byte) error {
	if ib.progress.Closed {
		panic("write data to bank while closed")
	}
	ib.log.Debug("channel bank got new data", "origin", ib.progress.Origin, "data_len", len(data))
	if len(data) < 1 {
		ib.log.Error("data must be at least have a version byte, but got empty string")
		return nil
	}

	if data[0] != DerivationVersion0 {
		return fmt.Errorf("unrecognized derivation version: %d", data)
	}
	buf := bytes.NewBuffer(data[1:])

	ib.prune()

	if buf.Len() < minimumFrameSize {
		return fmt.Errorf("data must be at least have one frame")
	}

	// Iterate over all frames. They may have different channel IDs to indicate that they stream consumer should reset.
	for {
		// Don't try to unmarshal from an empty buffer.
		// The if done checks should catch most/all of this case though.
		if buf.Len() < ChannelIDDataSize+1 {
			return nil
		}
		done := false
		var f Frame
		if err := (&f).UnmarshalBinary(buf); err == io.EOF {
			done = true
		} else if err != nil {
			return fmt.Errorf("failed to unmarshal a frame: %w", err)

		}

		// stop reading and ignore remaining data if we encounter a zeroed ID
		if f.ID == (ChannelID{}) {
			ib.log.Info("empty channel ID")
			return nil
		}

		// check if the channel is not timed out
		if f.ID.Time+ib.cfg.ChannelTimeout < ib.progress.Origin.Time {
			ib.log.Info("channel is timed out, ignore frame", "channel", f.ID, "id_time", f.ID.Time, "frame", f.FrameNumber)
			if done {
				return nil
			}
			continue
		}
		// check if the channel is not included too soon (otherwise timeouts wouldn't be effective)
		if f.ID.Time > ib.progress.Origin.Time {
			ib.log.Info("channel claims to be from the future, ignore frame", "channel", f.ID, "id_time", f.ID.Time, "frame", f.FrameNumber)
			if done {
				return nil
			}
			continue
		}

		currentCh, ok := ib.channels[f.ID]
		if !ok { // create new channel if it doesn't exist yet
			currentCh = &ChannelIn{id: f.ID}
			ib.channels[f.ID] = currentCh
			ib.channelQueue = append(ib.channelQueue, f.ID)
		}

		ib.log.Debug("ingesting frame", "channel", f.ID, "frame_number", f.FrameNumber, "length", len(f.Data))
		if err := currentCh.IngestData(f.FrameNumber, f.IsLast, f.Data); err != nil {
			ib.log.Debug("failed to ingest frame into channel", "channel", f.ID, "frame_number", f.FrameNumber, "err", err)
			if done {
				return nil
			}
			continue
		}

		if done {
			return nil
		}
	}
}

// Read the raw data of the first channel, if it's timed-out or closed.
// Read returns io.EOF if there is nothing new to read.
func (ib *ChannelBank) Read() (data []byte, err error) {
	if len(ib.channelQueue) == 0 {
		return nil, io.EOF
	}
	first := ib.channelQueue[0]
	ch := ib.channels[first]
	timedOut := first.Time+ib.cfg.ChannelTimeout < ib.progress.Origin.Time
	if timedOut {
		ib.log.Debug("channel timed out", "channel", first, "frames", len(ch.inputs))
	}
	if ch.closed {
		ib.log.Debug("channel closed", "channel", first)
	}
	if !timedOut && !ch.closed { // check if channel is done (can then be read)
		return nil, io.EOF
	}
	delete(ib.channels, first)
	ib.channelQueue = ib.channelQueue[1:]
	data = ch.Read()
	return data, nil
}

func (ib *ChannelBank) Step(ctx context.Context, outer Progress) error {
	if changed, err := ib.progress.Update(outer); err != nil || changed {
		return err
	}

	// If the bank is behind the channel reader, then we are replaying old data to prepare the bank.
	// Read if we can, and drop if it gives anything
	if ib.next.Progress().Origin.Number > ib.progress.Origin.Number {
		_, err := ib.Read()
		return err
	}

	// otherwise, read the next channel data from the bank
	data, err := ib.Read()
	if err == io.EOF { // need new L1 data in the bank before we can read more channel data
		return io.EOF
	} else if err != nil {
		return err
	}
	ib.next.WriteChannel(data)
	return nil
}

// ResetStep walks back the L1 chain, starting at the origin of the next stage,
// to find the origin that the channel bank should be reset to,
// to get consistent reads starting at origin.
// Any channel data before this origin will be timed out by the time the channel bank is synced up to the origin,
// so it is not relevant to replay it into the bank.
func (ib *ChannelBank) ResetStep(ctx context.Context, l1Fetcher L1Fetcher) error {
	if !ib.resetting {
		ib.progress = ib.next.Progress()
		ib.resetting = true
		return nil
	}
	if ib.progress.Origin.Time+ib.cfg.ChannelTimeout < ib.next.Progress().Origin.Time || ib.progress.Origin.Number <= ib.cfg.Genesis.L1.Number {
		ib.log.Debug("found reset origin for channel bank", "origin", ib.progress.Origin)
		ib.resetting = false
		return io.EOF
	}

	ib.log.Debug("walking back to find reset origin for channel bank", "origin", ib.progress.Origin)

	// go back in history if we are not distant enough from the next stage
	parent, err := l1Fetcher.L1BlockRefByHash(ctx, ib.progress.Origin.ParentHash)
	if err != nil {
		ib.log.Error("failed to find channel bank block, failed to retrieve L1 reference", "err", err)
		return nil
	}
	ib.progress.Origin = parent
	return nil
}

type L1BlockRefByHashFetcher interface {
	L1BlockRefByHash(context.Context, common.Hash) (eth.L1BlockRef, error)
}

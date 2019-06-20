// Copyright 2019 The Swarm Authors
// This file is part of the Swarm library.
//
// The Swarm library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The Swarm library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the Swarm library. If not, see <http://www.gnu.org/licenses/>.

package syncer

import (
	"context"
	"errors"
	"fmt"

	"github.com/ethersphere/swarm/log"
	"github.com/ethersphere/swarm/network"
)

// ErrMaxPeerServers will be returned if peer server limit is reached.
// It will be sent in the SubscribeErrorMsg.
var ErrMaxPeerServers = errors.New("max peer servers")

// Peer is the Peer extension for the streaming protocol
type Peer struct {
	*network.BzzPeer
	streamCursors map[uint]uint //key: bin, value: session cursor
	syncer        *SwarmSyncer

	quit chan struct{}
}

// NewPeer is the constructor for Peer
func NewPeer(peer *network.BzzPeer, s *SwarmSyncer) *Peer {
	p := &Peer{
		BzzPeer:       peer,
		streamCursors: make(map[uint]uint),
		syncer:        s,
		quit:          make(chan struct{}),
	}
	return p
}

func (p *Peer) Left() {
	close(p.quit)
}

// HandleMsg is the message handler that delegates incoming messages
func (p *Peer) HandleMsg(ctx context.Context, msg interface{}) error {
	switch msg := msg.(type) {
	case *StreamInfoReq:
		go p.handleStreamInfoReq(ctx, msg)
	case *StreamInfoRes:
		go p.handleStreamInfoRes(ctx, msg)

	default:
		return fmt.Errorf("unknown message type: %T", msg)
	}
	return nil
}

func (p *Peer) handleStreamInfoRes(ctx context.Context, msg *StreamInfoRes) {
	log.Debug("handleStreamInfoRes", "msg", msg)
}
func (p *Peer) handleStreamInfoReq(ctx context.Context, msg *StreamInfoReq) {
	log.Debug("handleStreamInfoReq", "msg", msg)
	streamRes := StreamInfoRes{}

	for _, v := range msg.Streams {
		streamCursor, err := p.syncer.netStore.LastPullSubscriptionBinID(uint8(v))
		if err != nil {
			log.Error("error getting last bin id", "bin", v)
		}
		descriptor := StreamDescriptor{
			Name:    "SYNC",
			Cursor:  streamCursor,
			Bounded: false,
		}
		streamRes.Streams = append(streamRes.Streams, descriptor)
	}
	if err := p.Send(ctx, streamRes); err != nil {
		log.Error("failed to send StreamInfoRes to client", "requested bins", msg.Streams)
	}
}

// syncSubscriptionsDiff calculates to which proximity order bins a peer
// (with po peerPO) needs to be subscribed after kademlia neighbourhood depth
// change from prevDepth to newDepth. Max argument limits the number of
// proximity order bins. Returned values are slices of integers which represent
// proximity order bins, the first one to which additional subscriptions need to
// be requested and the second one which subscriptions need to be quit. Argument
// prevDepth with value less then 0 represents no previous depth, used for
// initial syncing subscriptions.
func syncSubscriptionsDiff(peerPO, prevDepth, newDepth, max int) (subBins, quitBins []uint) {
	newStart, newEnd := syncBins(peerPO, newDepth, max)
	if prevDepth < 0 {
		// no previous depth, return the complete range
		// for subscriptions requests and nothing for quitting
		return intRange(newStart, newEnd), nil
	}

	prevStart, prevEnd := syncBins(peerPO, prevDepth, max)

	if newStart < prevStart {
		subBins = append(subBins, intRange(newStart, prevStart)...)
	}

	if prevStart < newStart {
		quitBins = append(quitBins, intRange(prevStart, newStart)...)
	}

	if newEnd < prevEnd {
		quitBins = append(quitBins, intRange(newEnd, prevEnd)...)
	}

	if prevEnd < newEnd {
		subBins = append(subBins, intRange(prevEnd, newEnd)...)
	}

	return subBins, quitBins
}

// syncBins returns the range to which proximity order bins syncing
// subscriptions need to be requested, based on peer proximity and
// kademlia neighbourhood depth. Returned range is [start,end), inclusive for
// start and exclusive for end.
func syncBins(peerPO, depth, max int) (start, end int) {
	if peerPO < depth {
		// subscribe only to peerPO bin if it is not
		// in the nearest neighbourhood
		return peerPO, peerPO + 1
	}
	// subscribe from depth to max bin if the peer
	// is in the nearest neighbourhood
	return depth, max + 1
}

// intRange returns the slice of integers [start,end). The start
// is inclusive and the end is not.
func intRange(start, end int) (r []uint) {
	for i := start; i < end; i++ {
		r = append(r, uint(i))
	}
	return r
}
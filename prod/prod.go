// Copyright 2020 The Swarm Authors
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

package prod

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethersphere/swarm/chunk"
	"github.com/ethersphere/swarm/log"
	"github.com/ethersphere/swarm/pss"
	"github.com/ethersphere/swarm/pss/trojan"
	"github.com/ethersphere/swarm/storage/feed"
	"github.com/ethersphere/swarm/storage/feed/lookup"
)

// RecoveryTopicText is the string used to construct the recovery topic
const RecoveryTopicText = "RECOVERY"

// RecoveryTopic is the topic used for repairing globally pinned chunks
var RecoveryTopic = trojan.NewTopic(RecoveryTopicText)

// ErrPublisher is returned when the publisher string cannot be decoded into bytes
var ErrPublisher = errors.New("failed to decode publisher")

// ErrPubKey is returned when the publisher bytes cannot be decompressed as a public key
var ErrPubKey = errors.New("failed to decompress public key")

// ErrFeedLookup is returned when the recovery feed cannot be successefully looked up
var ErrFeedLookup = errors.New("failed to look up recovery feed")

// ErrFeedContent is returned when there is a failure to retrieve content from the recovery feed
var ErrFeedContent = errors.New("failed to get content for recovery feed")

// ErrTargets is returned when there is a failure to unmarshal the feed content as a trojan.Targets variable
var ErrTargets = errors.New("failed to unmarshal targets in recovery feed content")

// RecoveryHook defines code to be executed upon failing to retrieve pinned chunks
type RecoveryHook func(ctx context.Context, chunkAddress chunk.Address) error

// sender is the function call for sending trojan chunks
type sender func(ctx context.Context, targets trojan.Targets, topic trojan.Topic, payload []byte) (*pss.Monitor, error)

// NewRecoveryHook returns a new RecoveryHook with the sender function defined
func NewRecoveryHook(send sender, handler feed.GenericHandler, publisher string) RecoveryHook {
	return func(ctx context.Context, chunkAddress chunk.Address) error {
		log.Debug("gp recovery hook triggered", "chunk", hex.EncodeToString(chunkAddress))
		targets, err := getPinners(ctx, handler, publisher)
		if err != nil {
			log.Debug("gp error recovering targets", "error", err.Error())
			return err
		}
		for _, t := range targets {
			log.Debug("gp target found", "target", t)
		}
		payload := chunkAddress

		// TODO: returned monitor should be made use of
		if _, err := send(ctx, targets, RecoveryTopic, payload); err != nil {
			return err
		}
		return nil
	}
}

// NewRepairHandler creates a repair function to re-upload globally pinned chunks to the network with the given store
func NewRepairHandler(s *chunk.ValidatorStore) pss.Handler {
	return func(m trojan.Message) {
		chAddr := m.Payload
		s.Set(context.Background(), chunk.ModeSetReUpload, chAddr)
	}
}

// getPinners returns the specific target pinners for a corresponding chunk
func getPinners(ctx context.Context, handler feed.GenericHandler, publisher string) (trojan.Targets, error) {
	log.Debug("gp getPinner", "publisher", publisher)

	// query feed using recovery topic and publisher
	feedContent, err := queryRecoveryFeed(ctx, RecoveryTopicText, publisher, handler)
	if err != nil {
		return nil, err
	}

	// extract targets from feed content
	targets := new(trojan.Targets)
	if err := json.Unmarshal(feedContent, targets); err != nil {
		return nil, ErrTargets
	}

	return *targets, nil
}

// queryRecoveryFeed attempts to create a feed topic and user, and query a feed based on these to fetch its content
func queryRecoveryFeed(ctx context.Context, topicText string, publisher string, handler feed.GenericHandler) ([]byte, error) {
	topic, user, err := getFeedTopicAndUser(topicText, publisher)
	if err != nil {
		return nil, err
	}
	log.Debug("gp queryRecoveryFeed", "user", user, "topic", topic)
	return getFeedContent(ctx, handler, topic, user)
}

// getFeedTopicAndUser creates a feed topic and user from the given topic text and publisher strings
func getFeedTopicAndUser(topicText string, publisher string) (feed.Topic, common.Address, error) {
	// get feed topic from topic text
	topic, err := feed.NewTopic(topicText, nil)
	if err != nil {
		return feed.Topic{}, common.Address{}, err
	}
	// get feed user from publisher
	user, err := publisherToAddress(publisher)
	if err != nil {
		return feed.Topic{}, common.Address{}, err
	}
	return topic, user, nil
}

// getFeedContent creates a feed with the given topic and user, and attempts to fetch its content using the given handler
func getFeedContent(ctx context.Context, handler feed.GenericHandler, topic feed.Topic, user common.Address) ([]byte, error) {
	fd := feed.Feed{
		Topic: topic,
		User:  user,
	}
	query := feed.NewQueryLatest(&fd, lookup.NoClue)
	ctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()

	_, err := handler.Lookup(ctx, query)
	// feed should still be queried even if there are no updates
	if err != nil && err.Error() != "no feed updates found" {
		return nil, ErrFeedLookup
	}

	_, content, err := handler.GetContent(&fd)
	if err != nil {
		return nil, ErrFeedContent
	}

	return content, nil
}

// publisherToAddress derives an address based on the given publisher string
func publisherToAddress(publisher string) (common.Address, error) {
	/* publisherBytes, err := hex.DecodeString(publisher)
	if err != nil {
		return common.Address{}, ErrPublisher
	}
	pubKey, err := crypto.DecompressPubkey(publisherBytes)
	if err != nil {
		return common.Address{}, ErrPubKey
	}
	return crypto.PubkeyToAddress(*pubKey), nil */
	return common.HexToAddress(publisher), nil
}
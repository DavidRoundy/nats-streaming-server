// Copyright 2016 Apcera Inc. All rights reserved.

package stores

import (
	"sort"
	"sync"

	"github.com/nats-io/stan-server/spb"
	"github.com/nats-io/stan/pb"
)

// commonStore contains everything that is common to any type of store
type commonStore struct {
	sync.RWMutex
	limits ChannelLimits
	closed bool
}

// genericStore is the generic store implementation with a map of channels.
type genericStore struct {
	commonStore
	name     string
	channels map[string]*ChannelStore
}

// genericSubStore is the generic store implementation that manages subscriptions
// for a given channel.
type genericSubStore struct {
	commonStore
	subject   string // Can't be wildcard
	subsCount int
	nextSubID uint64
}

// genericMsgStore is the generic store implementation that manages messages
// for a given channel.
type genericMsgStore struct {
	commonStore
	subject    string // Can't be wildcard
	first      uint64
	last       uint64
	msgs       map[uint64]*pb.MsgProto
	totalCount int
	totalBytes uint64
}

////////////////////////////////////////////////////////////////////////////
// genericStore methods
////////////////////////////////////////////////////////////////////////////

// init initializes the structure of a generic store
func (gs *genericStore) init(name string, limits *ChannelLimits) {
	gs.name = name
	if limits == nil {
		gs.limits = DefaultChannelLimits
	} else {
		gs.limits = *limits
	}
	// Do not use limits values to create the map.
	gs.channels = make(map[string]*ChannelStore, 16)
}

// Name returns the type name of this store
func (gs *genericStore) Name() string {
	return gs.name
}

// SetChannelLimits sets the limit for the messages and subscriptions stores.
func (gs *genericStore) SetChannelLimits(limits ChannelLimits) {
	gs.Lock()
	defer gs.Unlock()
	gs.limits = limits
}

// LookupChannel returns a ChannelStore for the given channel.
func (gs *genericStore) LookupChannel(channel string) *ChannelStore {
	gs.RLock()
	defer gs.RUnlock()

	return gs.channels[channel]
}

// HasChannel returns true if this store has any channel
func (gs *genericStore) HasChannel() bool {
	gs.RLock()
	defer gs.RUnlock()

	return len(gs.channels) > 0
}

// State returns message store statistics for a given channel ('*' for all)
func (gs *genericStore) MsgsState(channel string) (numMessages int, byteSize uint64, err error) {
	numMessages = 0
	byteSize = 0
	err = nil

	if channel == AllChannels {
		gs.RLock()
		cs := gs.channels
		gs.RUnlock()

		for _, c := range cs {
			n, b, lerr := c.Msgs.State()
			if lerr != nil {
				err = lerr
				return
			}
			numMessages += n
			byteSize += b
		}
	} else {
		cs := gs.LookupChannel(channel)
		if cs != nil {
			numMessages, byteSize, err = cs.Msgs.State()
		}
	}
	return
}

// canAddChannel returns true if the current number of channels is below the limit.
// Store lock is assumed to be locked.
func (gs *genericStore) canAddChannel() error {
	if len(gs.channels) >= gs.limits.MaxChannels {
		return ErrTooManyChannels
	}
	return nil
}

// Close closes all stores
func (gs *genericStore) Close() error {
	gs.Lock()
	defer gs.Unlock()

	if gs.closed {
		return nil
	}

	gs.closed = true

	var err error
	var lerr error

	for _, cs := range gs.channels {
		lerr = cs.Subs.Close()
		if lerr != nil && err == nil {
			err = lerr
		}
		lerr = cs.Msgs.Close()
		if lerr != nil && err == nil {
			err = lerr
		}
	}

	return err
}

////////////////////////////////////////////////////////////////////////////
// genericMsgStore methods
////////////////////////////////////////////////////////////////////////////

// init initializes this generic message store
func (gms *genericMsgStore) init(subject string, limits ChannelLimits) {
	gms.subject = subject
	gms.limits = limits
	gms.first = 1
	// FIXME(ik) - Long term, msgs map should probably not be part of the
	// generic store.
	// We could use limits.MaxNumMsgs for the size of the map, but that
	// may be too big if there is lots of channels with only few messages.
	// The map will grow as needed.
	gms.msgs = make(map[uint64]*pb.MsgProto, 64)
}

// State returns some statistics related to this store
func (gms *genericMsgStore) State() (numMessages int, byteSize uint64, err error) {
	gms.RLock()
	defer gms.RUnlock()

	return gms.totalCount, gms.totalBytes, nil
}

// FirstSequence returns sequence for first message stored.
func (gms *genericMsgStore) FirstSequence() uint64 {
	gms.RLock()
	defer gms.RUnlock()
	return gms.first
}

// LastSequence returns sequence for last message stored.
func (gms *genericMsgStore) LastSequence() uint64 {
	gms.RLock()
	defer gms.RUnlock()
	return gms.last
}

// FirstAndLastSequence returns sequences for the first and last messages stored.
func (gms *genericMsgStore) FirstAndLastSequence() (uint64, uint64) {
	gms.RLock()
	defer gms.RUnlock()
	return gms.first, gms.last
}

// Lookup returns the stored message with given sequence number.
func (gms *genericMsgStore) Lookup(seq uint64) *pb.MsgProto {
	gms.RLock()
	defer gms.RUnlock()
	return gms.msgs[seq]
}

// FirstMsg returns the first message stored.
func (gms *genericMsgStore) FirstMsg() *pb.MsgProto {
	gms.RLock()
	defer gms.RUnlock()
	return gms.msgs[gms.first]
}

// LastMsg returns the last message stored.
func (gms *genericMsgStore) LastMsg() *pb.MsgProto {
	gms.RLock()
	defer gms.RUnlock()
	return gms.msgs[gms.last]
}

// GetSequenceFromTimestamp returns the sequence of the first message whose
// timestamp is greater or equal to given timestamp.
func (gms *genericMsgStore) GetSequenceFromTimestamp(timestamp int64) uint64 {
	gms.RLock()
	defer gms.RUnlock()

	index := sort.Search(len(gms.msgs), func(i int) bool {
		m := gms.msgs[uint64(i)+gms.first]
		if m.Timestamp >= timestamp {
			return true
		}
		return false
	})

	return uint64(index) + gms.first
}

// Close closes this store.
func (gms *genericMsgStore) Close() error {
	return nil
}

////////////////////////////////////////////////////////////////////////////
// genericSubStore methods
////////////////////////////////////////////////////////////////////////////

// init initializes the structure of a generic sub store
func (gss *genericSubStore) init(channel string, limits ChannelLimits) {
	gss.subject = channel
	gss.limits = limits
}

// CreateSub records a new subscription represented by SubState. On success,
// it records the subscription's ID in SubState.ID. This ID is to be used
// by the other SubStore methods.
func (gss *genericSubStore) CreateSub(sub *spb.SubState) error {
	gss.Lock()
	defer gss.Unlock()

	return gss.createSub(sub)
}

// createSub is the unlocked version of CreateSub that can be used by
// non-generic implementations.
func (gss *genericSubStore) createSub(sub *spb.SubState) error {
	if gss.subsCount >= gss.limits.MaxSubs {
		return ErrTooManySubs
	}

	gss.nextSubID++
	gss.subsCount++

	sub.ID = gss.nextSubID

	return nil
}

// DeleteSub invalidates this subscription.
func (gss *genericSubStore) DeleteSub(subid uint64) {
	gss.Lock()
	defer gss.Unlock()

	gss.subsCount--
}

// AddSeqPending adds the given message seqno to the given subscription.
func (gss *genericSubStore) AddSeqPending(subid, seqno uint64) error {
	// no-op
	return nil
}

// AckSeqPending records that the given message seqno has been acknowledged
// by the given subscription.
func (gss *genericSubStore) AckSeqPending(subid, seqno uint64) error {
	// no-op
	return nil
}

// Close closes this store
func (gss *genericSubStore) Close() error {
	// no-op
	return nil
}

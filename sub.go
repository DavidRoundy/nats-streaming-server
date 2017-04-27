// Copyright 2016 Apcera Inc. All rights reserved.

// A Go client for the STAN/NATS messaging system (https://nats.io).
package stan

import (
	"errors"
	"sync"
	"time"

	"github.com/nats-io/nats"
)

const (
	DefaultAckWait     = 30 * time.Second
	DefaultMaxInflight = 1024
)

// Client defined Msg, which includes proto, then back link to subscription.
type Msg struct {
	MsgProto // MsgProto: Seq, Subject, Reply[opt], Data, Timestamp, CRC32[opt]
	Sub      Subscription
}

// Subscriptions and Options

// Subscription represents a subscription within the STAN cluster. Subscriptions
// will be rate matched and follow at-least delivery semantics.
type Subscription interface {
	Unsubscribe() error
}

// A subscription represents a subscription to a stan cluster.
type subscription struct {
	sync.RWMutex
	sc       *conn
	subject  string
	qgroup   string
	inbox    string
	ackInbox string
	inboxSub *nats.Subscription
	opts     SubscriptionOptions
	cb       MsgHandler
}

// SubscriptionOption is a function on the options for a subscription.
type SubscriptionOption func(*SubscriptionOptions) error

// MsgHandler is a callback function that processes messages delivered to
// asynchronous subscribers.
type MsgHandler func(msg *Msg)

// SubscriptionOptions are used to control the Subscription's behavior.
type SubscriptionOptions struct {
	// DurableName, if set will survive client restarts.
	DurableName string
	// Controls the number of messages the cluster will have inflight without an ACK.
	MaxInflight int
	// Controls the time the cluster will wait for an ACK for a given message.
	AckWait time.Duration
	// StartPosition enum from proto.
	StartAt StartPosition
	// Optional start sequence number.
	StartSequence uint64
	// Optional start time.
	StartTime time.Time
	// Option to do Manual Acks
	ManualAcks bool
}

var DefaultSubscriptionOptions = SubscriptionOptions{
	MaxInflight: DefaultMaxInflight,
	AckWait:     DefaultAckWait,
}

// MaxInflight is an Option to set the maximum number of messages the cluster will send
// without an ACK.
func MaxInflight(m int) SubscriptionOption {
	return func(o *SubscriptionOptions) error {
		o.MaxInflight = m
		return nil
	}
}

// AckWait is an Option to set the timeout for waiting for an ACK from the cluster's
// point of view for delivered messages.
func AckWait(t time.Duration) SubscriptionOption {
	return func(o *SubscriptionOptions) error {
		o.AckWait = t
		return nil
	}
}

// StartPosition sets the desired start position for the message stream.
func StartAt(sp StartPosition) SubscriptionOption {
	return func(o *SubscriptionOptions) error {
		o.StartAt = sp
		return nil
	}
}

// StartSequence sets the desired start sequence position and state.
func StartAtSequence(seq uint64) SubscriptionOption {
	return func(o *SubscriptionOptions) error {
		o.StartAt = StartPosition_SequenceStart
		o.StartSequence = seq
		return nil
	}
}

// StartTime sets the desired start time position and state.
func StartAtTime(start time.Time) SubscriptionOption {
	return func(o *SubscriptionOptions) error {
		o.StartAt = StartPosition_TimeStart
		o.StartTime = start
		return nil
	}
}

// StartWithLastReceived is a helper function to set start position to last received.
func StartWithLastReceived() SubscriptionOption {
	return func(o *SubscriptionOptions) error {
		o.StartAt = StartPosition_LastReceived
		return nil
	}
}

// DeliverAllAvailable will deliver all messages available.
func DeliverAllAvailable() SubscriptionOption {
	return func(o *SubscriptionOptions) error {
		o.StartAt = StartPosition_First
		return nil
	}
}

// SetManualAckMode will allow clients to control their own acks to delivered messages.
func SetManualAckMode() SubscriptionOption {
	return func(o *SubscriptionOptions) error {
		o.ManualAcks = true
		return nil
	}
}

// DurableName sets the DurableName for the subcriber.
func DurableName(name string) SubscriptionOption {
	return func(o *SubscriptionOptions) error {
		o.DurableName = name
		return nil
	}
}

// Subscribe will perform a subscription with the given options to the STAN cluster.
func (sc *conn) Subscribe(subject string, cb MsgHandler, options ...SubscriptionOption) (Subscription, error) {
	return sc.subscribe(subject, "", cb, options...)
}

// QueueSubscribe will perform a queue subscription with the given options to the STAN cluster.
func (sc *conn) QueueSubscribe(subject, qgroup string, cb MsgHandler, options ...SubscriptionOption) (Subscription, error) {
	return sc.subscribe(subject, qgroup, cb, options...)
}

// subscribe will perform a subscription with the given options to the STAN cluster.
func (sc *conn) subscribe(subject, qgroup string, cb MsgHandler, options ...SubscriptionOption) (Subscription, error) {
	sub := &subscription{subject: subject, qgroup: qgroup, inbox: newInbox(), cb: cb, sc: sc, opts: DefaultSubscriptionOptions}
	for _, opt := range options {
		if err := opt(&sub.opts); err != nil {
			return nil, err
		}
	}
	sc.Lock()
	if sc.nc == nil {
		sc.Unlock()
		return nil, ErrConnectionClosed
	}

	// Register subscription.
	sc.subMap[sub.inbox] = sub
	nc := sc.nc
	sc.Unlock()

	// Hold lock throughout.
	sub.Lock()
	defer sub.Unlock()

	// Listen for actual messages.
	if nsub, err := nc.Subscribe(sub.inbox, sc.processMsg); err != nil {
		return nil, err
	} else {
		sub.inboxSub = nsub
	}

	// Create a subscription request
	// FIXME(dlc) add others.
	sr := &SubscriptionRequest{
		ClientID:      sc.clientID,
		Subject:       subject,
		QGroup:        qgroup,
		Inbox:         sub.inbox,
		MaxInFlight:   int32(sub.opts.MaxInflight),
		AckWaitInSecs: int32(sub.opts.AckWait / time.Second),
		StartPosition: sub.opts.StartAt,
		DurableName:   sub.opts.DurableName,
	}

	// Conditionals
	switch sr.StartPosition {
	case StartPosition_TimeStart:
		sr.StartTime = sub.opts.StartTime.UnixNano()
	case StartPosition_SequenceStart:
		sr.StartSequence = sub.opts.StartSequence
	}

	b, _ := sr.Marshal()
	reply, err := sc.nc.Request(sc.subRequests, b, 2*time.Second)
	if err != nil {
		// FIXME(dlc) unwind subscription from above.
		return nil, err
	}
	r := &SubscriptionResponse{}
	if err := r.Unmarshal(reply.Data); err != nil {
		// FIXME(dlc) unwind subscription from above.
		return nil, err
	}
	if r.Error != "" {
		// FIXME(dlc) unwind subscription from above.
		return nil, errors.New(r.Error)
	}
	sub.ackInbox = r.AckInbox

	return sub, nil
}

// Unsubscribe removes interest in the subscription
func (sub *subscription) Unsubscribe() error {
	if sub == nil {
		return ErrBadSubscription
	}
	sub.Lock()
	sc := sub.sc
	if sc == nil {
		// Already closed.
		sub.Unlock()
		return ErrBadSubscription
	}
	sub.sc = nil
	sub.inboxSub.Unsubscribe()
	sub.inboxSub = nil
	inbox := sub.inbox
	sub.Unlock()

	if sc == nil {
		return ErrBadSubscription
	}

	sc.Lock()
	if sc.nc == nil {
		sc.Unlock()
		return ErrConnectionClosed
	}

	delete(sc.subMap, inbox)
	reqSubject := sc.unsubRequests
	sc.Unlock()

	// Send Unsubscribe to server.

	// FIXME(dlc) - Add in durable?
	usr := &UnsubscribeRequest{
		ClientID: sc.clientID,
		Subject:  sub.subject,
		Inbox:    sub.ackInbox,
	}
	b, _ := usr.Marshal()
	// FIXME(dlc) - make timeout configurable.
	reply, err := sc.nc.Request(reqSubject, b, 2*time.Second)
	if err != nil {
		return err
	}
	r := &SubscriptionResponse{}
	if err := r.Unmarshal(reply.Data); err != nil {
		return err
	}
	if r.Error != "" {
		return errors.New(r.Error)
	}

	return nil
}

// Manually Ack a Message.
func (msg *Msg) Ack() error {
	if msg == nil {
		return ErrNilMsg
	}
	// Look up subscription
	sub := msg.Sub.(*subscription)
	if sub == nil {
		return ErrBadSubscription
	}

	sub.RLock()
	ackSubject := sub.ackInbox
	isManualAck := sub.opts.ManualAcks
	sc := sub.sc
	sub.RUnlock()

	// Check for error conditions.
	if sc == nil {
		return ErrBadSubscription
	}
	if !isManualAck {
		return ErrManualAck
	}

	// Ack here.
	ack := &Ack{Subject: msg.Subject, Sequence: msg.Sequence}
	b, _ := ack.Marshal()
	return sc.nc.Publish(ackSubject, b)
}

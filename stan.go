// Copyright 2016 Apcera Inc. All rights reserved.

// A Go client for the STAN/NATS messaging system (https://nats.io).
package stan

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"time"

	"github.com/nats-io/nats"
)

const (
	Version                   = "0.0.1"
	DefaultNatsURL            = "nats://localhost:4222"
	DefaultConnectWait        = 2 * time.Second
	DefaultDiscoverPrefix     = "_STAN.discover"
	DefaultACKPrefix          = "_STAN.acks"
	DefaultMaxPubAcksInflight = 16384
)

// Conn represents a connection to the STAN subsystem. It can Publish and
// Subscribe to messages withing the STAN cluster.
type Conn interface {
	// Publish
	Publish(subject string, data []byte) error
	PublishAsync(subject string, data []byte, ah AckHandler) (string, error)
	// Publish with Reply
	PublishWithReply(subject, reply string, data []byte) error
	PublishAsyncWithReply(subject, reply string, data []byte, ah AckHandler) (string, error)

	// Subscribe
	Subscribe(subject string, cb MsgHandler, opts ...SubscriptionOption) (Subscription, error)

	// QueueSubscribe
	QueueSubscribe(subject, qgroup string, cb MsgHandler, opts ...SubscriptionOption) (Subscription, error)

	// Close
	Close() error
}

// Errors
var (
	ErrConnectReqTimeout = errors.New("stan: connect request timeout")
	ErrCloseReqTimeout   = errors.New("stan: close request timeout")
	ErrConnectionClosed  = errors.New("stan: connection closed")
	ErrTimeout           = errors.New("stan: publish ack timeout")
	ErrBadAck            = errors.New("stan: malformed ack")
	ErrBadSubscription   = errors.New("stan: invalid subscription")
	ErrBadConnection     = errors.New("stan: invalid connection")
	ErrManualAck         = errors.New("stan: can not manually ack in auto-ack mode")
	ErrNilMsg            = errors.New("stan: nil message")
)

// AckHandler is used for Async Publishing to provide status of the ack.
// The func will be passed teh GUID and any error state. No error means the
// message was sucessfully received by STAN.
type AckHandler func(string, error)

// Options can be used to a create a customized connection.
type Options struct {
	NatsURL            string
	NatsConn           *nats.Conn
	ConnectTimeout     time.Duration
	AckTimeout         time.Duration
	DiscoverPrefix     string
	MaxPubAcksInflight int
}

var DefaultOptions = Options{
	NatsURL:            DefaultNatsURL,
	ConnectTimeout:     DefaultConnectWait,
	AckTimeout:         DefaultAckWait,
	DiscoverPrefix:     DefaultDiscoverPrefix,
	MaxPubAcksInflight: DefaultMaxPubAcksInflight,
}

// Option is a function on the options for a connection.
type Option func(*Options) error

// ConnectWait is an Option to set the timeout for establishing a connection.
func ConnectWait(t time.Duration) Option {
	return func(o *Options) error {
		o.ConnectTimeout = t
		return nil
	}
}

// PubAckWait is an Option to set the timeout for waiting for an ACK for a
// published message.
func PubAckWait(t time.Duration) Option {
	return func(o *Options) error {
		o.AckTimeout = t
		return nil
	}
}

// A conn represents a bare connection to a stan cluster.
type conn struct {
	sync.Mutex
	clientID        string
	serverID        string
	pubPrefix       string // Publish prefix set by stan, append our subject.
	subRequests     string // Subject to send subscription requests.
	unsubRequests   string // Subject to send unsubscribe requests.
	closeRequests   string // Subject to send close requests.
	ackSubject      string // publish acks
	ackSubscription *nats.Subscription
	subMap          map[string]*subscription
	pubAckMap       map[string]*ack
	pubAckChan      chan (struct{})
	opts            Options
	nc              *nats.Conn
}

// Closure for ack contexts.
type ack struct {
	t  *time.Timer
	ah AckHandler
}

// Connect will form a connection to the STAN subsystem.
func Connect(stanClusterID, clientID string, options ...Option) (Conn, error) {
	// Process Options
	c := conn{clientID: clientID, opts: DefaultOptions}
	for _, opt := range options {
		if err := opt(&c.opts); err != nil {
			return nil, err
		}
	}
	// Create a connection if it doesn't exist.
	if c.nc == nil {
		if nc, err := nats.Connect(c.opts.NatsURL); err != nil {
			return nil, err
		} else {
			c.nc = nc
		}
	}
	// Send Request to discover the cluster
	discoverSubject := fmt.Sprintf("%s.%s", c.opts.DiscoverPrefix, stanClusterID)
	req := &ConnectRequest{ClientID: clientID}
	b, _ := req.Marshal()
	reply, err := c.nc.Request(discoverSubject, b, c.opts.ConnectTimeout)
	if err != nil {
		if err == nats.ErrTimeout {
			return nil, ErrConnectReqTimeout
		} else {
			return nil, err
		}
	}
	// Process the response, grab server pubPrefix
	cr := &ConnectResponse{}
	err = cr.Unmarshal(reply.Data)
	if err != nil {
		return nil, err
	}
	if cr.Error != "" {
		return nil, errors.New(cr.Error)
	}

	// Capture cluster configuration endpoints to publish and subscribe/unsubscribe.
	c.pubPrefix = cr.PubPrefix
	c.subRequests = cr.SubRequests
	c.unsubRequests = cr.UnsubRequests
	c.closeRequests = cr.CloseRequests

	// Setup the ACK subscription
	c.ackSubject = fmt.Sprintf("%s.%s", DefaultACKPrefix, newGUID())
	if c.ackSubscription, err = c.nc.Subscribe(c.ackSubject, c.processAck); err != nil {
		return nil, err
	}
	c.ackSubscription.SetPendingLimits(1024*1024, 32*1024*1024)
	c.pubAckMap = make(map[string]*ack)

	// Create Subscription map
	c.subMap = make(map[string]*subscription)

	c.pubAckChan = make(chan struct{}, c.opts.MaxPubAcksInflight)

	// Attach a finalizer
	runtime.SetFinalizer(&c, func(sc *conn) { sc.Close() })

	return &c, nil
}

// Close a connection to the stan system.
func (sc *conn) Close() error {
	if sc == nil {
		return ErrBadConnection
	}

	sc.Lock()
	defer sc.Unlock()

	if sc.nc == nil {
		return ErrBadConnection
	}

	// Capture for NATS calls below.
	nc := sc.nc
	defer nc.Close()

	// Signals we are closed.
	sc.nc = nil

	// Now close ourselves.
	if sc.ackSubscription != nil {
		sc.ackSubscription.Unsubscribe()
	}

	req := &CloseRequest{ClientID: sc.clientID}
	b, _ := req.Marshal()
	reply, err := nc.Request(sc.closeRequests, b, sc.opts.ConnectTimeout)
	if err != nil {
		if err == nats.ErrTimeout {
			return ErrCloseReqTimeout
		} else {
			return err
		}
	}
	cr := &CloseResponse{}
	err = cr.Unmarshal(reply.Data)
	if err != nil {
		return err
	}
	if cr.Error != "" {
		return errors.New(cr.Error)
	}
	return nil
}

// Process an ack from the STAN cluster
func (sc *conn) processAck(m *nats.Msg) {
	pa := &PubAck{}
	err := pa.Unmarshal(m.Data)
	if err != nil {
		// FIXME, make closure to have context?
		fmt.Printf("Error processing unmarshal\n")
	}

	// Remove
	a := sc.removeAck(pa.Id)

	// Perform the ackHandler callback
	if a != nil && a.ah != nil {
		a.ah(pa.Id, nil)
	}
}

// Publish will publish to the cluster and wait for an ACK.
func (sc *conn) Publish(subject string, data []byte) (e error) {
	return sc.PublishWithReply(subject, "", data)
}

// PublishAsync will publish to the cluster on pubPrefix+subject and asynchronously
// process the ACK or error state. It will return the GUID for the message being sent.
func (sc *conn) PublishAsync(subject string, data []byte, ah AckHandler) (string, error) {
	return sc.PublishAsyncWithReply(subject, "", data, ah)
}

// PublishWithReply will publish to the cluster and wait for an ACK.
func (sc *conn) PublishWithReply(subject, reply string, data []byte) error {
	// FIXME(dlc) Pool?
	ch := make(chan error)
	ah := func(guid string, err error) {
		ch <- err
	}
	if _, err := sc.PublishAsyncWithReply(subject, reply, data, ah); err != nil {
		return err
	}
	err := <-ch
	return err
}

// PublishAsyncWithReply will publish to the cluster and asynchronously
// process the ACK or error state. It will return the GUID for the message being sent.
func (sc *conn) PublishAsyncWithReply(subject, reply string, data []byte, ah AckHandler) (string, error) {
	subj := fmt.Sprintf("%s.%s", sc.pubPrefix, subject)
	pe := &PubMsg{Id: newGUID(), Subject: subject, Reply: reply, Data: data}
	b, _ := pe.Marshal()
	a := &ack{ah: ah}

	sc.Lock()
	if sc.nc == nil {
		sc.Unlock()
		return "", ErrConnectionClosed
	}
	// Map ack to guid.
	sc.pubAckMap[pe.Id] = a
	// snapshot
	ackSubject := sc.ackSubject
	ackTimeout := sc.opts.AckTimeout
	pac := sc.pubAckChan
	sc.Unlock()

	// Use the buffered channel to control the number of outstanding acks.
	pac <- struct{}{}

	err := sc.nc.PublishRequest(subj, ackSubject, b)
	if err != nil {
		sc.removeAck(pe.Id)
		return "", err
	}

	// Setup the timer for expiration.
	sc.Lock()
	a.t = time.AfterFunc(ackTimeout, func() {
		sc.removeAck(pe.Id)
		if a.ah != nil {
			ah(pe.Id, ErrTimeout)
		}
	})
	sc.Unlock()

	return pe.Id, nil
}

// removeAck removes the ack from the pubAckMap and cancels any state, e.g. timers
func (sc *conn) removeAck(guid string) *ack {
	sc.Lock()
	a := sc.pubAckMap[guid]
	delete(sc.pubAckMap, guid)
	pac := sc.pubAckChan
	sc.Unlock()

	// Cancel timer if needed.
	if a != nil && a.t != nil {
		a.t.Stop()
	}

	// Remove from channel to unblock PublishAsync
	if a != nil && len(pac) > 0 {
		<-pac
	}
	return a
}

// New style Inbox
// FIXME(dlc) remove once ported back to nats client.
func newInbox() string {
	return fmt.Sprintf("_INBOX.%s", newGUID())
}

// Helper function to produce time.Time from timestamp ns.
func (m *Msg) Time() time.Time {
	return time.Unix(0, m.Timestamp)
}

// Process an msg from the STAN cluster
func (sc *conn) processMsg(raw *nats.Msg) {
	msg := &Msg{}
	err := msg.Unmarshal(raw.Data)
	if err != nil {
		panic("Error processing unmarshal for msg")
	}
	// Lookup the subscription
	sc.Lock()
	isClosed := sc.nc == nil
	sub := sc.subMap[raw.Subject]
	sc.Unlock()

	// Check if sub is no longer valid or connection has been closed.
	if sub == nil || isClosed {
		return
	}

	// Store in msg for backlink
	msg.Sub = sub

	sub.RLock()
	cb := sub.cb
	ackSubject := sub.ackInbox
	isManualAck := sub.opts.ManualAcks
	subsc := sub.sc
	var nc *nats.Conn
	if subsc != nil {
		subsc.Lock()
		nc = subsc.nc
		subsc.Unlock()
	}
	sub.RUnlock()

	// Perform the callback
	if cb != nil && subsc != nil {
		cb(msg)
	}

	// Proces auto-ack
	if !isManualAck && nc != nil {
		ack := &Ack{Sequence: msg.Sequence}
		b, _ := ack.Marshal()
		nc.Publish(ackSubject, b)
	}
}

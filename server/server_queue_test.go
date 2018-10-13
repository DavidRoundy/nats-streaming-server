// Copyright 2016-2017 Apcera Inc. All rights reserved.
package server

import (
	"testing"
	"time"

	"github.com/nats-io/go-nats"
	"github.com/nats-io/go-nats-streaming"
	"github.com/nats-io/nats-streaming-server/spb"
	"github.com/nats-io/nats-streaming-server/stores"
)

// As of now, it is possible for members of the same group to have different
// AckWait values. This test checks that if a member with an higher AckWait
// than the other member leaves, the message with an higher expiration time
// is set to the remaining member's AckWait value.
// It also checks that on the opposite case, if a member leaves and the
// remaining member has a higher AckWait, the original expiration time is
// maintained.
func TestQueueSubsWithDifferentAckWait(t *testing.T) {
	s := runServer(t, clusterName)
	defer s.Shutdown()

	sc := NewDefaultConnection(t)
	defer sc.Close()

	var qsub1, qsub2, qsub3 stan.Subscription
	var err error

	dch := make(chan bool)
	rch2 := make(chan bool)
	rch3 := make(chan bool)

	cb := func(m *stan.Msg) {
		if !m.Redelivered {
			dch <- true
		} else {
			if m.Sub == qsub2 {
				rch2 <- true
			} else if m.Sub == qsub3 {
				rch3 <- true
				// stop further redeliveries, test is done.
				qsub3.Close()
			}
		}
	}
	// Create first queue member with high AckWait
	qsub1, err = sc.QueueSubscribe("foo", "bar", cb,
		stan.SetManualAckMode(),
		stan.AckWait(30*time.Second))
	if err != nil {
		t.Fatalf("Unexpected error on subscribe: %v", err)
	}
	// Send the single message used in this test
	if err := sc.Publish("foo", []byte("hello")); err != nil {
		t.Fatalf("Unexpected error on publish: %v", err)
	}
	// Wait for message to be received
	if err := Wait(dch); err != nil {
		t.Fatal("Did not get our message")
	}
	// Create the second member with low AckWait
	qsub2, err = sc.QueueSubscribe("foo", "bar", cb,
		stan.SetManualAckMode(),
		stan.AckWait(time.Second))
	if err != nil {
		t.Fatalf("Unexpected error on subscribe: %v", err)
	}
	// Check we have the two members
	checkQueueGroupSize(t, s, "foo", "bar", true, 2)
	// Close the first, message should be redelivered within
	// qsub2's AckWait, which is 1 second.
	qsub1.Close()
	// Check we have only 1 member
	checkQueueGroupSize(t, s, "foo", "bar", true, 1)
	// Wait for redelivery
	select {
	case <-rch2:
	// ok
	case <-time.After(1500 * time.Millisecond):
		t.Fatal("Message should have been redelivered")
	}
	// Create 3rd member with higher AckWait than the 2nd
	qsub3, err = sc.QueueSubscribe("foo", "bar", cb,
		stan.SetManualAckMode(),
		stan.AckWait(10*time.Second))
	if err != nil {
		t.Fatalf("Unexpected error on subscribe: %v", err)
	}
	// Close qsub2
	qsub2.Close()
	// Check we have only 1 member
	checkQueueGroupSize(t, s, "foo", "bar", true, 1)
	// Wait for redelivery. It should happen after the remaining
	// of the first redelivery to qsub2 and its AckWait, which
	// should be less than a second.
	select {
	case <-rch3:
	// ok
	case <-time.After(1500 * time.Millisecond):
		t.Fatal("Message should have been redelivered")
	}
}

func TestQueueMaxInFlight(t *testing.T) {
	s := runServer(t, clusterName)
	defer s.Shutdown()

	sc := NewDefaultConnection(t)
	defer sc.Close()

	total := 100
	payload := []byte("hello")
	for i := 0; i < total; i++ {
		sc.Publish("foo", payload)
	}

	ch := make(chan bool)
	received := 0
	cb := func(m *stan.Msg) {
		if !m.Redelivered {
			received++
			if received == total {
				ch <- true
			}
		}
	}
	if _, err := sc.QueueSubscribe("foo", "group", cb,
		stan.DeliverAllAvailable(),
		stan.MaxInflight(5)); err != nil {
		t.Fatalf("Unexpected error on subscribe: %v", err)
	}

	if err := Wait(ch); err != nil {
		t.Fatal("Did not get all our messages")
	}
}

func TestQueueGroupRemovedOnLastMemberLeaving(t *testing.T) {
	s := runServer(t, clusterName)
	defer s.Shutdown()

	sc := NewDefaultConnection(t)
	defer sc.Close()

	if err := sc.Publish("foo", []byte("msg1")); err != nil {
		t.Fatalf("Unexpected error on publish: %v", err)
	}
	ch := make(chan bool)
	cb := func(m *stan.Msg) {
		if m.Sequence == 1 {
			ch <- true
		}
	}
	// Create a queue subscriber
	if _, err := sc.QueueSubscribe("foo", "group", cb, stan.DeliverAllAvailable()); err != nil {
		t.Fatalf("Unexpected error on subscribe: %v", err)
	}
	// Wait to receive the message
	if err := Wait(ch); err != nil {
		t.Fatal("Did not get our message")
	}
	// Close the connection which will remove the queue subscriber
	sc.Close()

	// Create a new connection
	sc = NewDefaultConnection(t)
	defer sc.Close()
	// Send a new message
	if err := sc.Publish("foo", []byte("msg2")); err != nil {
		t.Fatalf("Unexpected error on publish: %v", err)
	}
	// Start a queue subscriber. The group should have been destroyed
	// when the last member left, so even with a new name, this should
	// be a new group and start from msg seq 1
	qsub, err := sc.QueueSubscribe("foo", "group", cb, stan.DeliverAllAvailable())
	if err != nil {
		t.Fatalf("Unexpected error on subscribe: %v", err)
	}
	// Wait to receive the message
	if err := Wait(ch); err != nil {
		t.Fatal("Did not get our message")
	}
	// Test with unsubscribe
	if err := qsub.Unsubscribe(); err != nil {
		t.Fatalf("Error during Unsubscribe: %v", err)
	}
	// Recreate a queue subscriber, it should again receive from msg1
	if _, err := sc.QueueSubscribe("foo", "group", cb, stan.DeliverAllAvailable()); err != nil {
		t.Fatalf("Unexpected error on subscribe: %v", err)
	}
	// Wait to receive the message
	if err := Wait(ch); err != nil {
		t.Fatal("Did not get our message")
	}
}

func TestQueueSubscriberTransferPendingMsgsOnClose(t *testing.T) {
	s := runServer(t, clusterName)
	defer s.Shutdown()

	sc := NewDefaultConnection(t)
	defer sc.Close()

	if err := sc.Publish("foo", []byte("msg1")); err != nil {
		t.Fatalf("Unexpected error on publish: %v", err)
	}
	var sub1 stan.Subscription
	var sub2 stan.Subscription
	var err error
	ch := make(chan bool)
	qsetup := make(chan bool)
	cb := func(m *stan.Msg) {
		<-qsetup
		if m.Sub == sub1 && m.Sequence == 1 && !m.Redelivered {
			ch <- true
		} else if m.Sub == sub2 && m.Sequence == 1 && m.Redelivered {
			ch <- true
		}
	}
	// Create a queue subscriber with MaxInflight == 1 and manual ACK
	// so that it does not ack it and see if it will be redelivered.
	sub1, err = sc.QueueSubscribe("foo", "group", cb,
		stan.DeliverAllAvailable(),
		stan.MaxInflight(1),
		stan.SetManualAckMode())
	if err != nil {
		t.Fatalf("Unexpected error on subscribe: %v", err)
	}
	qsetup <- true
	// Wait to receive the message
	if err := Wait(ch); err != nil {
		t.Fatal("Did not get our message")
	}
	// Start 2nd queue subscriber on same group
	sub2, err = sc.QueueSubscribe("foo", "group", cb, stan.AckWait(time.Second))
	if err != nil {
		t.Fatalf("Unexpected error on subscribe: %v", err)
	}
	// Unsubscribe the first member
	sub1.Unsubscribe()
	qsetup <- true
	// The second queue subscriber should receive the first message.
	if err := Wait(ch); err != nil {
		t.Fatal("Did not get our message")
	}
}

func TestPersistentStoreQueueSubLeavingUpdateQGroupLastSent(t *testing.T) {
	cleanupDatastore(t)
	defer cleanupDatastore(t)

	opts := getTestDefaultOptsForPersistentStore()
	s := runServerWithOpts(t, opts, nil)
	defer shutdownRestartedServerOnTestExit(&s)

	sc := NewDefaultConnection(t)
	defer sc.Close()

	if err := sc.Publish("foo", []byte("msg1")); err != nil {
		t.Fatalf("Unexpected error on publish: %v", err)
	}
	ch := make(chan bool)
	cb := func(m *stan.Msg) {
		ch <- true
	}
	// Create a queue subscriber with MaxInflight == 1 and manual ACK
	// so that it does not ack it and see if it will be redelivered.
	if _, err := sc.QueueSubscribe("foo", "group", cb,
		stan.DeliverAllAvailable(),
		stan.MaxInflight(1),
		stan.SetManualAckMode()); err != nil {
		t.Fatalf("Unexpected error on subscribe: %v", err)
	}
	// Wait to receive the message
	if err := Wait(ch); err != nil {
		t.Fatal("Did not get our message")
	}
	// send second message
	if err := sc.Publish("foo", []byte("msg2")); err != nil {
		t.Fatalf("Unexpected error on publish: %v", err)
	}
	// Start 2nd queue subscriber on same group
	sub2, err := sc.QueueSubscribe("foo", "group", cb, stan.DeliverAllAvailable())
	if err != nil {
		t.Fatalf("Unexpected error on subscribe: %v", err)
	}
	// The second queue subscriber should receive the second message.
	if err := Wait(ch); err != nil {
		t.Fatal("Did not get our message")
	}
	// Unsubscribe the second member
	sub2.Unsubscribe()
	// Restart server
	s.Shutdown()
	s = runServerWithOpts(t, opts, nil)
	// Send a third message
	if err := sc.Publish("foo", []byte("msg3")); err != nil {
		t.Fatalf("Unexpected error on publish: %v", err)
	}
	// Start a third queue subscriber, it should receive message 3
	msgCh := make(chan *stan.Msg)
	lastMsgCb := func(m *stan.Msg) {
		msgCh <- m
	}
	if _, err := sc.QueueSubscribe("foo", "group", lastMsgCb, stan.DeliverAllAvailable()); err != nil {
		t.Fatalf("Unexpected error on subscribe: %v", err)
	}
	// Wait for msg3 or an error to occur
	gotIt := false
	select {
	case m := <-msgCh:
		if m.Sequence != 3 {
			t.Fatalf("Unexpected message: %v", m)
		} else {
			gotIt = true
			break
		}
	case <-time.After(time.Second):
		// Wait for a bit to see if we receive extraneous messages
		break
	}
	if !gotIt {
		t.Fatal("Did not get message 3")
	}
}

func checkQueueGroupSize(t *testing.T, s *StanServer, channelName, groupName string, expectedExist bool, expectedSize int) {
	cs := channelsGet(t, s.channels, channelName)
	s.mu.RLock()
	groupSize := 0
	group, exist := cs.ss.qsubs[groupName]
	if exist {
		groupSize = len(group.subs)
	}
	s.mu.RUnlock()
	if expectedExist && !exist {
		stackFatalf(t, "Expected group to still exist, does not")
	}
	if !expectedExist && exist {
		stackFatalf(t, "Expected group to not exist, it does")
	}
	if expectedExist {
		if groupSize != expectedSize {
			stackFatalf(t, "Expected group size to be %v, got %v", expectedSize, groupSize)
		}
	}
}

func TestBasicDurableQueueSub(t *testing.T) {
	s := runServer(t, clusterName)
	defer s.Shutdown()

	sc1 := NewDefaultConnection(t)
	defer sc1.Close()
	if err := sc1.Publish("foo", []byte("msg1")); err != nil {
		t.Fatalf("Unexpected error on publish: %v", err)
	}
	ch := make(chan bool)
	count := 0
	cb := func(m *stan.Msg) {
		count++
		if count == 1 && m.Sequence == 1 {
			ch <- true
		} else if count == 2 && m.Sequence == 2 {
			ch <- true
		}
	}
	// Create a durable queue subscriber.
	sc2, err := stan.Connect(clusterName, "sc2cid")
	if err != nil {
		stackFatalf(t, "Expected to connect correctly, got err %v", err)
	}
	defer sc2.Close()
	if _, err := sc2.QueueSubscribe("foo", "group", cb,
		stan.DeliverAllAvailable(),
		stan.DurableName("qsub")); err != nil {
		t.Fatalf("Unexpected error on subscribe: %v", err)
	}
	// Check group exists
	checkQueueGroupSize(t, s, "foo", "qsub:group", true, 1)
	// Wait to receive the message
	if err := Wait(ch); err != nil {
		t.Fatal("Did not get our message")
	}
	// For this test, make sure we wait for ack to be processed.
	waitForAcks(t, s, "sc2cid", 1, 0)
	// Close the durable queue sub's connection.
	// This should not close the queue group
	sc2.Close()
	// Check queue group still exist, but size 0
	checkQueueGroupSize(t, s, "foo", "qsub:group", true, 0)
	// Send another message
	if err := sc1.Publish("foo", []byte("msg2")); err != nil {
		t.Fatalf("Unexpected error on publish: %v", err)
	}
	// Create a durable queue subscriber on the group.
	sub, err := sc1.QueueSubscribe("foo", "group", cb,
		stan.DeliverAllAvailable(),
		stan.DurableName("qsub"))
	if err != nil {
		t.Fatalf("Unexpected error on subscribe: %v", err)
	}
	// It should receive message 2 only
	if err := Wait(ch); err != nil {
		t.Fatal("Did not get our message")
	}
	// Now unsubscribe the sole member
	if err := sub.Unsubscribe(); err != nil {
		t.Fatalf("Error during Unsubscribe: %v", err)
	}
	// Group should be gone.
	checkQueueGroupSize(t, s, "foo", "qsub:group", false, 0)
}

func TestDurableAndNonDurableQueueSub(t *testing.T) {
	s := runServer(t, clusterName)
	defer s.Shutdown()

	sc := NewDefaultConnection(t)
	defer sc.Close()
	// Expect failure if durable name contains ':'
	_, err := sc.QueueSubscribe("foo", "group", func(_ *stan.Msg) {},
		stan.DurableName("qsub:"))
	if err == nil {
		t.Fatal("Expected error on subscribe")
	}
	if err.Error() != ErrInvalidDurName.Error() {
		t.Fatalf("Expected error %v, got %v", ErrInvalidDurName, err)
	}

	if err := sc.Publish("foo", []byte("msg1")); err != nil {
		t.Fatalf("Unexpected error on publish: %v", err)
	}
	ch := make(chan bool)
	cb := func(m *stan.Msg) {
		ch <- true
	}
	// Create a durable queue subscriber.
	if _, err := sc.QueueSubscribe("foo", "group", cb,
		stan.DeliverAllAvailable(),
		stan.DurableName("qsub")); err != nil {
		t.Fatalf("Unexpected error on subscribe: %v", err)
	}
	// Create a regular one
	if _, err := sc.QueueSubscribe("foo", "group", cb,
		stan.DeliverAllAvailable()); err != nil {
		t.Fatalf("Unexpected error on subscribe: %v", err)
	}
	// Check groups exists
	checkQueueGroupSize(t, s, "foo", "qsub:group", true, 1)
	checkQueueGroupSize(t, s, "foo", "group", true, 1)
	// Wait to receive the messages
	if err := Wait(ch); err != nil {
		t.Fatal("Did not get our message")
	}
	if err := Wait(ch); err != nil {
		t.Fatal("Did not get our message")
	}
	// Close connection
	sc.Close()
	// Non durable group should be gone
	checkQueueGroupSize(t, s, "foo", "group", false, 0)
	// Other should exist, but empty
	checkQueueGroupSize(t, s, "foo", "qsub:group", true, 0)
}

func TestPersistentStoreDurableQueueSub(t *testing.T) {
	cleanupDatastore(t)
	defer cleanupDatastore(t)

	opts := getTestDefaultOptsForPersistentStore()
	s := runServerWithOpts(t, opts, nil)
	defer shutdownRestartedServerOnTestExit(&s)

	sc1 := NewDefaultConnection(t)
	defer sc1.Close()
	if err := sc1.Publish("foo", []byte("msg1")); err != nil {
		t.Fatalf("Unexpected error on publish: %v", err)
	}
	ch := make(chan bool)
	count := 0
	cb := func(m *stan.Msg) {
		count++
		if count == 1 && m.Sequence == 1 {
			ch <- true
		} else if count == 2 && m.Sequence == 2 {
			ch <- true
		}
	}
	// Create a durable queue subscriber.
	sc2, err := stan.Connect(clusterName, "sc2cid")
	if err != nil {
		stackFatalf(t, "Expected to connect correctly, got err %v", err)
	}
	defer sc2.Close()
	if _, err := sc2.QueueSubscribe("foo", "group", cb,
		stan.DeliverAllAvailable(),
		stan.DurableName("qsub")); err != nil {
		t.Fatalf("Unexpected error on subscribe: %v", err)
	}
	// Wait to receive the message
	if err := Wait(ch); err != nil {
		t.Fatal("Did not get our message")
	}
	// For this test, make sure ack is processed
	waitForAcks(t, s, "sc2cid", 1, 0)
	// Close the durable queue sub's connection.
	// This should not close the queue group
	sc2.Close()
	// Check queue group still exist, but size 0
	checkQueueGroupSize(t, s, "foo", "qsub:group", true, 0)
	// Send another message
	if err := sc1.Publish("foo", []byte("msg2")); err != nil {
		t.Fatalf("Unexpected error on publish: %v", err)
	}
	// Stop and restart the server
	s.Shutdown()
	s = runServerWithOpts(t, opts, nil)
	// Create another client connection
	sc2, err = stan.Connect(clusterName, "sc2cid")
	if err != nil {
		stackFatalf(t, "Expected to connect correctly, got err %v", err)
	}
	defer sc2.Close()
	// Create a durable queue subscriber on the group.
	if _, err := sc2.QueueSubscribe("foo", "group", cb,
		stan.DeliverAllAvailable(),
		stan.DurableName("qsub")); err != nil {
		t.Fatalf("Unexpected error on subscribe: %v", err)
	}
	// It should receive message 2 only
	if err := Wait(ch); err != nil {
		t.Fatal("Did not get our message")
	}
}

func TestPersistentStoreQMemberRemovedFromStore(t *testing.T) {
	cleanupDatastore(t)
	defer cleanupDatastore(t)

	opts := getTestDefaultOptsForPersistentStore()
	s := runServerWithOpts(t, opts, nil)
	defer shutdownRestartedServerOnTestExit(&s)

	sc1 := NewDefaultConnection(t)
	defer sc1.Close()

	ch := make(chan bool)
	// Create the group (adding the first member)
	if _, err := sc1.QueueSubscribe("foo", "group",
		func(_ *stan.Msg) {
			ch <- true
		},
		stan.DurableName("dur")); err != nil {
		t.Fatalf("Unexpected error on subscribe: %v", err)
	}

	sc2, err := stan.Connect(clusterName, "othername")
	if err != nil {
		stackFatalf(t, "Expected to connect correctly, got err %v", err)
	}
	defer sc2.Close()
	// Add a second member to the group
	if _, err := sc2.QueueSubscribe("foo", "group", func(_ *stan.Msg) {},
		stan.DurableName("dur")); err != nil {
		t.Fatalf("Unexpected error on subscribe: %v", err)
	}
	// Remove it by closing the connection.
	sc2.Close()

	// Send a message and verify it is received
	if err := sc1.Publish("foo", []byte("hello")); err != nil {
		t.Fatalf("Unexpected error on publish: %v", err)
	}
	if err := Wait(ch); err != nil {
		t.Fatal("Did not get our message")
	}
	// Have the first leave the group too.
	sc1.Close()

	// Restart the server
	s.Shutdown()
	s = runServerWithOpts(t, opts, nil)

	sc := NewDefaultConnection(t)
	defer sc.Close()

	// Send a new message
	if err := sc.Publish("foo", []byte("hello")); err != nil {
		t.Fatalf("Unexpected error on publish: %v", err)
	}
	// Have a member rejoin the group
	if _, err := sc.QueueSubscribe("foo", "group",
		func(_ *stan.Msg) {
			ch <- true
		},
		stan.DurableName("dur")); err != nil {
		t.Fatalf("Unexpected error on subscribe: %v", err)
	}
	// It should receive the second message
	if err := Wait(ch); err != nil {
		t.Fatal("Did not get our message")
	}
	// Check server state
	s.mu.RLock()
	cs, _ := s.lookupOrCreateChannel("foo")
	ss := cs.ss
	s.mu.RUnlock()
	ss.RLock()
	qs := ss.qsubs["dur:group"]
	ss.RUnlock()
	if len(qs.subs) != 1 {
		t.Fatalf("Expected only 1 member, got %v", len(qs.subs))
	}
}

func TestPersistentStoreMultipleShadowQSubs(t *testing.T) {
	cleanupDatastore(t)
	defer cleanupDatastore(t)

	opts := getTestDefaultOptsForPersistentStore()
	s := runServerWithOpts(t, opts, nil)
	s.Shutdown()

	var (
		store stores.Store
		err   error
	)
	limits := stores.DefaultStoreLimits
	if persistentStoreType == stores.TypeFile {
		store, err = stores.NewFileStore(testLogger, defaultDataStore, &limits)
	}
	if err != nil {
		t.Fatalf("Error creating store: %v", err)
	}
	defer store.Close()
	cs, err := store.CreateChannel("foo")
	if err != nil {
		t.Fatalf("Error creating channel: %v", err)
	}
	sub := spb.SubState{
		ID:            1,
		AckInbox:      nats.NewInbox(),
		Inbox:         nats.NewInbox(),
		AckWaitInSecs: 30,
		MaxInFlight:   10,
		LastSent:      1,
		IsDurable:     true,
		QGroup:        "dur:queue",
	}
	cs.Subs.CreateSub(&sub)
	sub.ID = 2
	sub.LastSent = 2
	cs.Subs.CreateSub(&sub)
	store.Close()

	// Should not panic
	s = runServerWithOpts(t, opts, nil)
	defer s.Shutdown()
	scs, err := s.lookupOrCreateChannel("foo")
	if err != nil {
		t.Fatalf("Error looking up channel: %v", err)
	}
	ss := scs.ss
	ss.RLock()
	qs := ss.qsubs["dur:queue"]
	ss.RUnlock()
	if qs == nil {
		t.Fatal("Should have recovered queue group")
	}
	qs.RLock()
	shadow := qs.shadow
	lastSent := qs.lastSent
	qs.RUnlock()
	if shadow == nil {
		t.Fatal("Should have recovered a shadow queue sub")
	}
	if shadow.ID != 2 || lastSent != 2 {
		t.Fatalf("Recovered shadow queue sub should be ID 2, lastSent 2, got %v, %v", shadow.ID, lastSent)
	}
}

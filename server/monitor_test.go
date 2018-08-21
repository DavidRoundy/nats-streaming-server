// Copyright 2017 Apcera Inc. All rights reserved.

package server

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"math/rand"
	"net/http"
	"reflect"
	"runtime"
	"testing"
	"time"

	natsd "github.com/nats-io/gnatsd/server"
	natsdTest "github.com/nats-io/gnatsd/test"
	"github.com/nats-io/go-nats"
	"github.com/nats-io/go-nats-streaming"
	"github.com/nats-io/nats-streaming-server/stores"
)

const (
	monitorHost  = "127.0.0.1"
	monitorPort  = 8222
	expectedJSON = "application/json"
	expectedText = "text/html; charset=utf-8"
	expectedCb   = "application/javascript"
)

var defaultMonitorOptions = natsd.Options{
	Host:     "localhost",
	Port:     4222,
	HTTPHost: monitorHost,
	HTTPPort: monitorPort,
	Cluster: natsd.ClusterOpts{
		Host: "localhost",
		Port: 6222,
	},
	NoLog:  true,
	NoSigs: true,
}

func resetPreviousHTTPConnections() {
	http.DefaultTransport = &http.Transport{}
}

func runMonitorServer(t *testing.T, sOpts *Options) *StanServer {
	nOpts := defaultMonitorOptions
	return runServerWithOpts(t, sOpts, &nOpts)
}

func getBody(t *testing.T, endpoint, expectedContentType string) (*http.Response, []byte) {
	url := fmt.Sprintf("http://%s:%d%s", monitorHost, monitorPort, endpoint)
	resp, err := http.Get(url)
	if err != nil {
		stackFatalf(t, "Expected no error: Got %v\n", err)
	}
	if resp.StatusCode != 200 {
		stackFatalf(t, "Expected a 200 response, got %d\n", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if ct != expectedContentType {
		stackFatalf(t, "Expected %s content-type, got %s\n", expectedContentType, ct)
	}
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		resp.Body.Close()
		stackFatalf(t, "Got an error reading the body: %v\n", err)
	}
	return resp, body
}

func TestMonitorUseEmbeddedNATSServer(t *testing.T) {
	resetPreviousHTTPConnections()
	s := runMonitorServer(t, GetDefaultOptions())
	defer s.Shutdown()

	resp, _ := getBody(t, RootPath, expectedText)
	defer resp.Body.Close()
}

func TestMonitorStartOwnHTTPServer(t *testing.T) {
	resetPreviousHTTPConnections()
	ns := natsdTest.RunDefaultServer()
	defer ns.Shutdown()

	nOpts := natsdTest.DefaultTestOptions
	nOpts.HTTPHost = monitorHost
	nOpts.HTTPPort = monitorPort
	sOpts := GetDefaultOptions()
	sOpts.NATSServerURL = "nats://localhost:4222"
	s := runServerWithOpts(t, sOpts, &nOpts)
	defer s.Shutdown()

	resp, _ := getBody(t, RootPath, expectedText)
	resp.Body.Close()
}

func TestMonitorStartOwnHTTPSServer(t *testing.T) {
	resetPreviousHTTPConnections()
	ns := natsdTest.RunDefaultServer()
	defer ns.Shutdown()

	nOpts := natsdTest.DefaultTestOptions
	nOpts.HTTPHost = monitorHost
	nOpts.HTTPSPort = monitorPort
	nOpts.TLSConfig = &tls.Config{InsecureSkipVerify: true}
	cert, err := tls.LoadX509KeyPair("../test/certs/server-cert.pem", "../test/certs/server-key.pem")
	if err != nil {
		t.Fatalf("Got error reading certificates: %s", err)
	}
	nOpts.TLSConfig.Certificates = []tls.Certificate{cert}
	sOpts := GetDefaultOptions()
	sOpts.NATSServerURL = "nats://localhost:4222"
	s := runServerWithOpts(t, sOpts, &nOpts)
	defer s.Shutdown()

	tlsConfig := &tls.Config{}
	caCert, err := ioutil.ReadFile("../test/certs/ca.pem")
	if err != nil {
		t.Fatalf("Got error reading RootCA file: %s", err)
	}
	caCertPool := x509.NewCertPool()
	caCertPool.AppendCertsFromPEM(caCert)
	tlsConfig.RootCAs = caCertPool
	transport := &http.Transport{TLSClientConfig: tlsConfig}
	httpClient := &http.Client{Transport: transport}

	url := fmt.Sprintf("https://%s:%d%s", monitorHost, monitorPort, RootPath)
	resp, err := httpClient.Get(url)
	if err != nil {
		stackFatalf(t, "Expected no error: Got %v\n", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		stackFatalf(t, "Expected a 200 response, got %d\n", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if ct != expectedText {
		stackFatalf(t, "Expected %s content-type, got %s\n", expectedText, ct)
	}
}

func TestMonitorServerz(t *testing.T) {
	resetPreviousHTTPConnections()
	s := runMonitorServer(t, GetDefaultOptions())
	defer s.Shutdown()

	sc := NewDefaultConnection(t)
	defer sc.Close()
	sub, err := sc.Subscribe("foo", func(_ *stan.Msg) {})
	if err != nil {
		t.Fatalf("Unexpected error on subscribe: %v", err)
	}
	defer sub.Unsubscribe()
	totalMsgs := 10
	msg := []byte("hello")
	for i := 0; i < totalMsgs; i++ {
		if err := sc.Publish("foo", msg); err != nil {
			t.Fatalf("Unexpected error on publish: %v", err)
		}
	}
	cs := s.store.LookupChannel("foo")
	_, totalBytes, _ := cs.Msgs.State()

	resp, body := getBody(t, ServerPath, expectedJSON)
	defer resp.Body.Close()

	sz := Serverz{}
	if err := json.Unmarshal(body, &sz); err != nil {
		t.Fatalf("Got an error unmarshalling the body: %v", err)
	}
	if sz.ClusterID != s.ClusterID() {
		t.Fatalf("Expected ClusterID to be %v, got %v", s.ClusterID(), sz.ClusterID)
	}
	if sz.ServerID != s.serverID {
		t.Fatalf("Expected ServerID to be %v, got %v", s.serverID, sz.ServerID)
	}
	if sz.State != Standalone.String() {
		t.Fatalf("Expected State to be %v, got %v", Standalone.String(), sz.State)
	}
	if sz.Now.IsZero() {
		t.Fatalf("Expected Now to be set, was not")
	}
	if sz.Start.IsZero() {
		t.Fatalf("Expected Start to be set, was not")
	}
	if sz.Uptime == "" {
		t.Fatalf("Expected Uptime to be set, was not")
	}
	if sz.Version != VERSION {
		t.Fatalf("Expected version to be %v, got %v", VERSION, sz.Version)
	}
	if sz.GoVersion != runtime.Version() {
		t.Fatalf("Expected GoVersion to be %v, got %v", runtime.Version(), sz.Version)
	}
	if sz.Clients != 1 {
		t.Fatalf("Expected 1 client, got %v", sz.Clients)
	}
	if sz.Channels != 1 {
		t.Fatalf("Expected 1 channel, got %v", sz.Channels)
	}
	if sz.Subscriptions != 1 {
		t.Fatalf("Expected 1 subscription, got %v", sz.Subscriptions)
	}
	if sz.TotalMsgs != totalMsgs {
		t.Fatalf("Expected %d messages, got %v", totalMsgs, sz.TotalMsgs)
	}
	if sz.TotalBytes != totalBytes {
		t.Fatalf("Expected %v bytes, got %v", totalBytes, sz.TotalBytes)
	}
	resp.Body.Close()

	sub.Unsubscribe()
	waitForNumSubs(t, s, clientName, 0)

	resp, body = getBody(t, ServerPath, expectedJSON)
	defer resp.Body.Close()

	sz = Serverz{}
	if err := json.Unmarshal(body, &sz); err != nil {
		t.Fatalf("Got an error unmarshalling the body: %v", err)
	}
	if sz.Clients != 1 {
		t.Fatalf("Expected 1 client, got %v", sz.Clients)
	}
	if sz.Channels != 1 {
		t.Fatalf("Expected 1 channel, got %v", sz.Channels)
	}
	if sz.Subscriptions != 0 {
		t.Fatalf("Expected 0 subscription, got %v", sz.Subscriptions)
	}
	if sz.TotalMsgs != totalMsgs {
		t.Fatalf("Expected %d messages, got %v", totalMsgs, sz.TotalMsgs)
	}
	if sz.TotalBytes != totalBytes {
		t.Fatalf("Expected %v bytes, got %v", totalBytes, sz.TotalBytes)
	}
	resp.Body.Close()

	sc.Close()
	waitForNumClients(t, s, 0)

	resp, body = getBody(t, ServerPath, expectedJSON)
	defer resp.Body.Close()

	sz = Serverz{}
	if err := json.Unmarshal(body, &sz); err != nil {
		t.Fatalf("Got an error unmarshalling the body: %v", err)
	}
	if sz.Clients != 0 {
		t.Fatalf("Expected 0 client, got %v", sz.Clients)
	}
	if sz.Channels != 1 {
		t.Fatalf("Expected 1 channel, got %v", sz.Channels)
	}
	if sz.Subscriptions != 0 {
		t.Fatalf("Expected 0 subscription, got %v", sz.Subscriptions)
	}
	if sz.TotalMsgs != totalMsgs {
		t.Fatalf("Expected %d messages, got %v", totalMsgs, sz.TotalMsgs)
	}
	if sz.TotalBytes != totalBytes {
		t.Fatalf("Expected %v bytes, got %v", totalBytes, sz.TotalBytes)
	}
	resp.Body.Close()

	// Test JSONP
	resp, _ = getBody(t, ServerPath+"?callback=callback", expectedCb)
	resp.Body.Close()

	// Restart server, for memory based server, things should have been reset
	s.Shutdown()
	resetPreviousHTTPConnections()
	s = runMonitorServer(t, GetDefaultOptions())
	defer s.Shutdown()

	resp, body = getBody(t, ServerPath, expectedJSON)
	defer resp.Body.Close()

	sz = Serverz{}
	if err := json.Unmarshal(body, &sz); err != nil {
		t.Fatalf("Got an error unmarshalling the body: %v", err)
	}
	if sz.ClusterID != s.ClusterID() {
		t.Fatalf("Expected ClusterID to be %v, got %v", s.ClusterID(), sz.ClusterID)
	}
	if sz.ServerID != s.serverID {
		t.Fatalf("Expected ServerID to be %v, got %v", s.serverID, sz.ServerID)
	}
	if sz.State != Standalone.String() {
		t.Fatalf("Expected State to be %v, got %v", Standalone.String(), sz.State)
	}
	if sz.Now.IsZero() {
		t.Fatalf("Expected Now to be set, was not")
	}
	if sz.Start.IsZero() {
		t.Fatalf("Expected Start to be set, was not")
	}
	if sz.Uptime == "" {
		t.Fatalf("Expected Uptime to be set, was not")
	}
	if sz.Version != VERSION {
		t.Fatalf("Expected version to be %v, got %v", VERSION, sz.Version)
	}
	if sz.GoVersion != runtime.Version() {
		t.Fatalf("Expected GoVersion to be %v, got %v", runtime.Version(), sz.Version)
	}
	if sz.Clients != 0 {
		t.Fatalf("Expected 0 client, got %v", sz.Clients)
	}
	if sz.Channels != 0 {
		t.Fatalf("Expected 0 channel, got %v", sz.Channels)
	}
	if sz.Subscriptions != 0 {
		t.Fatalf("Expected 0 subscription, got %v", sz.Subscriptions)
	}
	if sz.TotalMsgs != 0 {
		t.Fatalf("Expected 0 message, got %v", sz.TotalMsgs)
	}
	if sz.TotalBytes > 0 {
		t.Fatalf("Expected 0 bytes, got %v", sz.TotalBytes)
	}
}

func TestMonitorUptime(t *testing.T) {
	expected := []string{"1y2d3h4m5s", "1d2h3m4s", "1h2m3s", "1m2s", "1s"}
	durations := []time.Duration{
		365*24*time.Hour + 2*24*time.Hour + 3*time.Hour + 4*time.Minute + 5*time.Second,
		24*time.Hour + 2*time.Hour + 3*time.Minute + 4*time.Second,
		time.Hour + 2*time.Minute + 3*time.Second,
		time.Minute + 2*time.Second,
		time.Second,
	}
	for i, d := range durations {
		got := myUptime(d)
		if got != expected[i] {
			t.Fatalf("Expected %v, got %v", expected[i], got)
		}
	}
}

func TestMonitorServerzAfterRestart(t *testing.T) {
	resetPreviousHTTPConnections()
	cleanupDatastore(t, defaultDataStore)
	defer cleanupDatastore(t, defaultDataStore)
	opts := getTestDefaultOptsForFileStore()

	s := runMonitorServer(t, opts)
	defer s.Shutdown()

	nc, err := nats.Connect(nats.DefaultURL, nats.ReconnectWait(100*time.Millisecond))
	if err != nil {
		t.Fatalf("Error on connect: %v", err)
	}
	defer nc.Close()
	sc, err := stan.Connect(clusterName, clientName, stan.NatsConn(nc))
	if err != nil {
		t.Fatalf("Error on connect: %v", err)
	}
	defer sc.Close()
	sub, err := sc.Subscribe("foo", func(_ *stan.Msg) {})
	if err != nil {
		t.Fatalf("Unexpected error on subscribe: %v", err)
	}
	defer sub.Unsubscribe()
	totalMsgs := 10
	msg := []byte("hello")
	for i := 0; i < totalMsgs; i++ {
		if err := sc.Publish("foo", msg); err != nil {
			t.Fatalf("Unexpected error on publish: %v", err)
		}
	}
	cs := s.store.LookupChannel("foo")
	_, totalBytes, _ := cs.Msgs.State()

	for i := 0; i < 2; i++ {
		resp, body := getBody(t, ServerPath, expectedJSON)
		defer resp.Body.Close()

		sz := Serverz{}
		if err := json.Unmarshal(body, &sz); err != nil {
			t.Fatalf("Got an error unmarshalling the body: %v", err)
		}
		if sz.ClusterID != s.ClusterID() {
			t.Fatalf("Expected ClusterID to be %v, got %v", s.ClusterID(), sz.ClusterID)
		}
		if sz.ServerID != s.serverID {
			t.Fatalf("Expected ServerID to be %v, got %v", s.serverID, sz.ServerID)
		}
		if sz.Now.IsZero() {
			t.Fatalf("Expected Now to be set, was not")
		}
		if sz.Start.IsZero() {
			t.Fatalf("Expected Start to be set, was not")
		}
		if sz.Uptime == "" {
			t.Fatalf("Expected Uptime to be set, was not")
		}
		if sz.Version != VERSION {
			t.Fatalf("Expected version to be %v, got %v", VERSION, sz.Version)
		}
		if sz.GoVersion != runtime.Version() {
			t.Fatalf("Expected GoVersion to be %v, got %v", runtime.Version(), sz.Version)
		}
		if sz.Clients != 1 {
			t.Fatalf("Expected 1 client, got %v", sz.Clients)
		}
		if sz.Channels != 1 {
			t.Fatalf("Expected 1 channel, got %v", sz.Channels)
		}
		if sz.Subscriptions != 1 {
			t.Fatalf("Expected 1 subscription, got %v", sz.Subscriptions)
		}
		if sz.TotalMsgs != totalMsgs {
			t.Fatalf("Expected %d messages, got %v", totalMsgs, sz.TotalMsgs)
		}
		if sz.TotalBytes != totalBytes {
			t.Fatalf("Expected %v bytes, got %v", totalBytes, sz.TotalBytes)
		}
		resp.Body.Close()

		// Restart server
		s.Shutdown()
		resetPreviousHTTPConnections()
		s = runMonitorServer(t, opts)
		defer s.Shutdown()
	}
	sc.Close()
	nc.Close()
	s.Shutdown()
}

func TestMonitorStorez(t *testing.T) {
	msg := []byte("hello")
	total := 1000

	testStore := func(s *StanServer, expectedType string) {
		defer s.Shutdown()

		resetPreviousHTTPConnections()

		sc := NewDefaultConnection(t)
		defer sc.Close()

		expectedTotalMsgs := 0
		expectedTotalBytes := uint64(0)

		for i := 0; i < 2; i++ {
			resp, body := getBody(t, StorePath, expectedJSON)
			defer resp.Body.Close()

			sz := Storez{}
			if err := json.Unmarshal(body, &sz); err != nil {
				t.Fatalf("Got an error unmarshalling the body: %v", err)
			}
			if sz.ClusterID != s.ClusterID() {
				t.Fatalf("Expected ClusterID to be %v, got %v", s.ClusterID(), sz.ClusterID)
			}
			if sz.ServerID != s.serverID {
				t.Fatalf("Expected ServerID to be %v, got %v", s.serverID, sz.ServerID)
			}
			if sz.Now.IsZero() {
				t.Fatalf("Expected Now to be set, was not")
			}
			if sz.Type != expectedType {
				t.Fatalf("Expected Type to be %v, got %v", expectedType, sz.Type)
			}
			if !reflect.DeepEqual(sz.Limits, s.opts.StoreLimits) {
				t.Fatalf("Expected Limits to be %v, got %v", s.opts.StoreLimits, sz.Limits)
			}
			if sz.TotalMsgs != expectedTotalMsgs {
				t.Fatalf("Expected TotalMsgs to be %v, got %v", expectedTotalMsgs, sz.TotalMsgs)
			}
			if sz.TotalBytes != expectedTotalBytes {
				t.Fatalf("Expected TotalMsgs to be %v, got %v", expectedTotalBytes, sz.TotalBytes)
			}
			resp.Body.Close()

			if i == 0 {
				for j := 0; j < total; j++ {
					if err := sc.Publish("foo", msg); err != nil {
						t.Fatalf("Unexpected error on publish: %v", err)
					}
				}
				cs := s.store.LookupChannel("foo")
				expectedTotalMsgs, expectedTotalBytes, _ = cs.Msgs.State()
			}
		}
	}

	s := runMonitorServer(t, GetDefaultOptions())
	testStore(s, stores.TypeMemory)

	cleanupDatastore(t, defaultDataStore)
	defer cleanupDatastore(t, defaultDataStore)
	opts := getTestDefaultOptsForFileStore()
	s = runMonitorServer(t, opts)
	testStore(s, stores.TypeFile)
}

func TestMonitorClientsz(t *testing.T) {
	resetPreviousHTTPConnections()
	s := runMonitorServer(t, GetDefaultOptions())
	defer s.Shutdown()

	cids := []string{"me1", "me2", "me3", "me4", "me5"}
	totalClients := len(cids)
	scs := make([]stan.Conn, 0, totalClients)
	for _, cid := range cids {
		sc, err := stan.Connect(clusterName, cid)
		if err != nil {
			t.Fatalf("Error on connect: %v", err)
		}
		defer sc.Close()
		if _, err := sc.Subscribe("bar", func(_ *stan.Msg) {}); err != nil {
			t.Fatalf("Unexpected error on subscribe: %v", err)
		}
		scs = append(scs, sc)
	}

	generateExpectedCZ := func(offset, limit, count, total int, cids []string, expectSubs bool) *Clientsz {
		clientsz := &Clientsz{
			ClusterID: s.info.ClusterID,
			ServerID:  s.serverID,
			Offset:    offset,
			Limit:     limit,
			Count:     count,
			Total:     total,
		}
		clientsz.Clients = make([]*Clientz, 0, len(cids))
		for _, cid := range cids {
			cli := s.store.GetClient(cid)
			cz := &Clientz{
				ID:      cid,
				HBInbox: cli.HbInbox,
			}
			if expectSubs {
				srvCli := cli.UserData.(*client)
				srvCli.RLock()
				cz.Subscriptions = getCliSubs(srvCli.subs)
				srvCli.RUnlock()
			}
			clientsz.Clients = append(clientsz.Clients, cz)
		}
		return clientsz
	}

	paths := []string{"", "?offset=-1", "?offset=1", "?offset=10", "?limit=-1", "?limit=1", "?offset=1&limit=2", "?subs=1"}
	expected := []*Clientsz{
		generateExpectedCZ(0, defaultMonitorListLimit, totalClients, totalClients, cids, false),
		generateExpectedCZ(0, defaultMonitorListLimit, totalClients, totalClients, cids, false),
		generateExpectedCZ(1, defaultMonitorListLimit, totalClients-1, totalClients, cids[1:], false),
		generateExpectedCZ(10, defaultMonitorListLimit, 0, totalClients, cids[totalClients:], false),
		generateExpectedCZ(0, defaultMonitorListLimit, totalClients, totalClients, cids, false),
		generateExpectedCZ(0, 1, 1, totalClients, cids[:1], false),
		generateExpectedCZ(1, 2, 2, totalClients, cids[1:1+2], false),
		generateExpectedCZ(0, defaultMonitorListLimit, totalClients-2, totalClients-2, cids[2:], true), // We have closed the 2 first clients
	}

	for i := 0; i < len(paths); i++ {
		resp, body := getBody(t, ClientsPath+paths[i], expectedJSON)
		defer resp.Body.Close()

		cz := Clientsz{}
		if err := json.Unmarshal(body, &cz); err != nil {
			t.Fatalf("Got an error unmarshalling the body: %v", err)
		}
		resp.Body.Close()
		goal := *expected[i]
		// We cannot assume Now, so remove it for comparison
		cz.Now = time.Time{}
		// We have only 1 sub per client, so DeepEqual will be ok.
		if !reflect.DeepEqual(cz, goal) {
			t.Fatalf("Iter=%v - Path=%q - Expected to get %v, got %v", i, ClientsPath+paths[i], goal, cz)
		}
		if i == len(paths)-2 {
			// Close the 2 first clients
			scs[0].Close()
			scs[1].Close()
		}
	}
	for _, sc := range scs {
		sc.Close()
	}
}

func getCliSubs(subs []*subState) map[string][]*Subscriptionz {
	if len(subs) == 0 {
		return nil
	}
	subsz := make(map[string][]*Subscriptionz)
	for _, sub := range subs {
		subz := createSubz(sub)
		sarr := subsz[sub.subject]
		newSarr := append(sarr, subz)
		if &newSarr != &sarr {
			subsz[sub.subject] = newSarr
		}
	}
	return subsz
}

func getChannelSubs(subs []*subState) []*Subscriptionz {
	if len(subs) == 0 {
		return nil
	}
	subsz := make([]*Subscriptionz, 0, len(subs))
	for _, sub := range subs {
		subsz = append(subsz, createSubz(sub))
	}
	return subsz
}

func createSubz(sub *subState) *Subscriptionz {
	sub.RLock()
	subz := &Subscriptionz{
		Inbox:        sub.Inbox,
		AckInbox:     sub.AckInbox,
		DurableName:  sub.DurableName,
		QueueName:    sub.QGroup,
		IsDurable:    sub.IsDurable,
		MaxInflight:  int(sub.MaxInFlight),
		AckWait:      int(sub.AckWaitInSecs),
		LastSent:     sub.LastSent,
		PendingCount: len(sub.acksPending),
		IsStalled:    sub.stalled,
	}
	sub.RUnlock()
	return subz
}

func TestMonitorClientz(t *testing.T) {
	resetPreviousHTTPConnections()
	s := runMonitorServer(t, GetDefaultOptions())
	defer s.Shutdown()

	cids := []string{"me1", "me2"}
	numClients := len(cids)
	numSubs := 10
	scs := make([]stan.Conn, 0, numClients)
	for _, cid := range cids {
		sc, err := stan.Connect(clusterName, cid)
		if err != nil {
			t.Fatalf("Error on connect: %v", err)
		}
		defer sc.Close()
		for i := 0; i < numSubs; i++ {
			if _, err := sc.Subscribe("bar", func(_ *stan.Msg) {}); err != nil {
				t.Fatalf("Unexpected error on subscribe: %v", err)
			}
		}
		scs = append(scs, sc)
	}

	generateExpectedCZ := func(cid string, expectSubs bool) *Clientz {
		cli := s.store.GetClient(cid)
		if cli == nil {
			return nil
		}
		cz := &Clientz{
			ID:      cid,
			HBInbox: cli.HbInbox,
		}
		if expectSubs {
			srvCli := cli.UserData.(*client)
			srvCli.RLock()
			cz.Subscriptions = getCliSubs(srvCli.subs)
			srvCli.RUnlock()
		}
		return cz
	}

	paths := []string{"?client=me1", "?client=me2&subs=1"}
	expected := []*Clientz{
		generateExpectedCZ("me1", false),
		generateExpectedCZ("me2", true),
	}

	for i := 0; i < len(paths); i++ {
		resp, body := getBody(t, ClientsPath+paths[i], expectedJSON)
		defer resp.Body.Close()

		cz := Clientz{}
		if err := json.Unmarshal(body, &cz); err != nil {
			t.Fatalf("Got an error unmarshalling the body: %v", err)
		}
		resp.Body.Close()
		goal := *expected[i]
		if goal.Subscriptions != nil {
			if err := compareCliSubs(goal.Subscriptions, cz.Subscriptions); err != nil {
				t.Fatalf("Iter=%v - Path=%q - %v", i, ClientsPath+paths[i], err)
			}
			// Now nilify the Subscriptions for the DeepEqual call
			goal.Subscriptions = nil
			cz.Subscriptions = nil
		} else if cz.Subscriptions != nil {
			t.Fatalf("Iter=%v - Path=%q - Did not expect to get subscriptions, got %v", i, ClientsPath+paths[i], cz.Subscriptions)
		}
		if !reflect.DeepEqual(cz, goal) {
			t.Fatalf("Iter=%v - Path=%q - Expected to get %v, got %v", i, ClientsPath+paths[i], goal, cz)
		}
	}

	// Check one that does not exist, expect 404
	url := fmt.Sprintf("http://%s:%d%s", monitorHost, monitorPort, ClientsPath+"?client=donotexist")
	resp, err := http.Get(url)
	if err != nil {
		stackFatalf(t, "Expected no error: Got %v\n", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		stackFatalf(t, "Expected a 404 response, got %d\n", resp.StatusCode)
	}

	for _, sc := range scs {
		sc.Close()
	}
}

func compareCliSubs(expected, got map[string][]*Subscriptionz) error {
	if len(expected) != len(got) {
		return fmt.Errorf("expected %d channels, got %v", len(expected), len(got))
	}
	for cn, sarr := range got {
		expectedSarr := expected[cn]
		if len(sarr) != len(expectedSarr) {
			return fmt.Errorf("channel %v, expected %d subscriptions, got %v", cn, len(expectedSarr), len(sarr))
		}
		if err := compareChannelSubs(cn, expectedSarr, sarr); err != nil {
			return err
		}
	}
	return nil
}

func compareChannelSubs(cn string, expected, got []*Subscriptionz) error {
	if len(expected) != len(got) {
		return fmt.Errorf("expected %d subscriptions, got %v", len(expected), len(got))
	}
	ok := false
	for _, sub := range got {
		for _, expectedSub := range expected {
			if reflect.DeepEqual(sub, expectedSub) {
				ok = true
				break
			}
		}
		if ok {
			break
		}
	}
	if !ok {
		return fmt.Errorf("channel %v, expected subscriptions %v, got %v", cn, expected, got)
	}
	return nil
}

func TestMonitorChannelsz(t *testing.T) {
	resetPreviousHTTPConnections()
	s := runMonitorServer(t, GetDefaultOptions())
	defer s.Shutdown()

	channels := []string{"bar", "baz", "foo", "foo.bar"}
	totalChannels := len(channels)
	for _, c := range channels {
		if _, err := s.lookupOrCreateChannel(c); err != nil {
			t.Fatalf("Error creating channel: %v", err)
		}
	}

	generateExpectedCZ := func(offset, limit, count int, channels []string) *Channelsz {
		channelsz := &Channelsz{
			ClusterID: s.info.ClusterID,
			ServerID:  s.serverID,
			Offset:    offset,
			Limit:     limit,
			Count:     count,
			Total:     totalChannels,
		}
		if channels != nil {
			channelsz.Names = make([]string, 0, len(channels))
			channelsz.Names = append(channelsz.Names, channels...)
		}
		return channelsz
	}

	paths := []string{"", "?offset=-1", "?offset=1", "?offset=10", "?limit=-1", "?limit=1", "?offset=1&limit=2"}
	expected := []*Channelsz{
		generateExpectedCZ(0, defaultMonitorListLimit, totalChannels, channels),
		generateExpectedCZ(0, defaultMonitorListLimit, totalChannels, channels),
		generateExpectedCZ(1, defaultMonitorListLimit, totalChannels-1, channels[1:]),
		generateExpectedCZ(10, defaultMonitorListLimit, 0, nil),
		generateExpectedCZ(0, defaultMonitorListLimit, totalChannels, channels),
		generateExpectedCZ(0, 1, 1, channels[:1]),
		generateExpectedCZ(1, 2, 2, channels[1:1+2]),
	}

	for i := 0; i < len(paths); i++ {
		resp, body := getBody(t, ChannelsPath+paths[i], expectedJSON)
		defer resp.Body.Close()

		cz := Channelsz{}
		if err := json.Unmarshal(body, &cz); err != nil {
			t.Fatalf("Got an error unmarshalling the body: %v", err)
		}
		resp.Body.Close()
		goal := *expected[i]
		// We cannot assume Now, so remove it for comparison
		cz.Now = time.Time{}
		if !reflect.DeepEqual(cz, goal) {
			t.Fatalf("Iter=%v - Path=%q - Expected to get %v, got %v", i, ChannelsPath+paths[i], goal, cz)
		}
	}
}

func TestMonitorChannelsWithSubsz(t *testing.T) {
	resetPreviousHTTPConnections()
	s := runMonitorServer(t, GetDefaultOptions())
	defer s.Shutdown()

	sc := NewDefaultConnection(t)
	defer sc.Close()

	channels := []string{"bar", "baz", "foo", "foo.bar"}
	totalChannels := len(channels)

	totalSubs := 0
	for _, c := range channels {
		cs, err := s.lookupOrCreateChannel(c)
		if err != nil {
			t.Fatalf("Error creating channel: %v", err)
		}
		for i := 0; i < rand.Intn(10)+1; i++ {
			cs.Msgs.Store([]byte("hello"))
		}
		numSubs := rand.Intn(4) + 1
		totalSubs += numSubs
		for i := 0; i < numSubs; i++ {
			if _, err := sc.Subscribe(c, func(_ *stan.Msg) {}); err != nil {
				t.Fatalf("Error on subscribe: %v", err)
			}
		}
		if _, err := sc.Subscribe(c, func(_ *stan.Msg) {},
			stan.DurableName(fmt.Sprintf("%s_dur", c))); err != nil {
			t.Fatalf("Error on subscribe: %v", err)
		}
		totalSubs++
		if _, err := sc.QueueSubscribe(c, "queue", func(_ *stan.Msg) {}); err != nil {
			t.Fatalf("Error on subscribe: %v", err)
		}
		totalSubs++
	}
	waitForNumSubs(t, s, clientName, totalSubs)

	generateExpectedCZ := func(offset, limit, count int, channels []string) *Channelsz {
		channelsz := &Channelsz{
			ClusterID: s.info.ClusterID,
			ServerID:  s.serverID,
			Offset:    offset,
			Limit:     limit,
			Count:     count,
			Total:     totalChannels,
		}
		if channels != nil {
			channelsz.Channels = make([]*Channelz, 0, len(channels))
			for _, c := range channels {
				cs := s.store.LookupChannel(c)
				if cs == nil {
					continue
				}
				msgs, bytes, _ := cs.Msgs.State()
				firstSeq, lastSeq := cs.Msgs.FirstAndLastSequence()
				channelz := &Channelz{
					Name:     c,
					FirstSeq: firstSeq,
					LastSeq:  lastSeq,
					Msgs:     msgs,
					Bytes:    bytes,
				}
				ss := cs.UserData.(*subStore)
				ss.RLock()
				subscriptions := getChannelSubs(ss.psubs)
				for _, dur := range ss.durables {
					subscriptions = append(subscriptions, createSubz(dur))
				}
				for _, qsub := range ss.qsubs {
					qsub.RLock()
					subscriptions = append(subscriptions, getChannelSubs(qsub.subs)...)
					qsub.RUnlock()
				}
				ss.RUnlock()
				channelz.Subscriptions = subscriptions
				channelsz.Channels = append(channelsz.Channels, channelz)
			}
		}
		return channelsz
	}

	paths := []string{"?subs=1", "?offset=-1&subs=1", "?offset=1&subs=1", "?offset=10&subs=1", "?limit=-1&subs=1", "?limit=1&subs=1", "?offset=1&limit=2&subs=1"}
	expected := []*Channelsz{
		generateExpectedCZ(0, defaultMonitorListLimit, totalChannels, channels),
		generateExpectedCZ(0, defaultMonitorListLimit, totalChannels, channels),
		generateExpectedCZ(1, defaultMonitorListLimit, totalChannels-1, channels[1:]),
		generateExpectedCZ(10, defaultMonitorListLimit, 0, nil),
		generateExpectedCZ(0, defaultMonitorListLimit, totalChannels, channels),
		generateExpectedCZ(0, 1, 1, channels[:1]),
		generateExpectedCZ(1, 2, 2, channels[1:1+2]),
	}

	for i := 0; i < len(paths); i++ {
		resp, body := getBody(t, ChannelsPath+paths[i], expectedJSON)
		defer resp.Body.Close()

		cz := Channelsz{}
		if err := json.Unmarshal(body, &cz); err != nil {
			t.Fatalf("Got an error unmarshalling the body: %v", err)
		}
		resp.Body.Close()
		goal := *expected[i]
		// We cannot assume Now, so remove it for comparison
		cz.Now = time.Time{}
		for i, channelz := range goal.Channels {
			if channelz.Subscriptions != nil {
				if err := compareChannelSubs(channelz.Name, channelz.Subscriptions, cz.Channels[i].Subscriptions); err != nil {
					t.Fatalf("Iter=%v - Path=%q - %v", i, ClientsPath+paths[i], err)
				}
				// Now nilify the Subscriptions for the DeepEqual call
				channelz.Subscriptions = nil
				cz.Channels[i].Subscriptions = nil
			} else if cz.Channels[i].Subscriptions != nil {
				t.Fatalf("Iter=%v - Path=%q - Was not expecting subscriptions, got %v", i, ClientsPath+paths[i], cz.Channels[i].Subscriptions)
			}
		}
		if !reflect.DeepEqual(cz, goal) {
			t.Fatalf("Iter=%v - Path=%q - Expected to get %v, got %v", i, ChannelsPath+paths[i], goal, cz)
		}
	}
}

func TestMonitorChannelz(t *testing.T) {
	resetPreviousHTTPConnections()
	s := runMonitorServer(t, GetDefaultOptions())
	defer s.Shutdown()

	sc := NewDefaultConnection(t)
	defer sc.Close()

	channels := []string{"bar", "baz", "foo", "foo.bar"}
	for _, c := range channels {
		cs, err := s.lookupOrCreateChannel(c)
		if err != nil {
			t.Fatalf("Error creating channel: %v", err)
		}
		for i := 0; i < rand.Intn(10)+1; i++ {
			cs.Msgs.Store([]byte("hello"))
		}
		if _, err := sc.Subscribe(c, func(_ *stan.Msg) {}); err != nil {
			t.Fatalf("Error on subscribe: %v", err)
		}
	}

	generateExpectedCZ := func(name string, expectedSubs bool) *Channelz {
		cs := s.store.LookupChannel(name)
		if cs == nil {
			return nil
		}
		msgs, bytes, _ := cs.Msgs.State()
		firstSeq, lastSeq := cs.Msgs.FirstAndLastSequence()
		channelz := &Channelz{
			Name:     name,
			FirstSeq: firstSeq,
			LastSeq:  lastSeq,
			Msgs:     msgs,
			Bytes:    bytes,
		}
		if expectedSubs {
			ss := cs.UserData.(*subStore)
			channelz.Subscriptions = getChannelSubs(ss.psubs)
		}
		return channelz
	}

	paths := []string{"?channel=bar", "?channel=foo", "?channel=foo.bar&subs=1"}
	expected := []*Channelz{
		generateExpectedCZ("bar", false),
		generateExpectedCZ("foo", false),
		generateExpectedCZ("foo.bar", true),
	}

	for i := 0; i < len(paths); i++ {
		resp, body := getBody(t, ChannelsPath+paths[i], expectedJSON)
		defer resp.Body.Close()

		cz := Channelz{}
		if err := json.Unmarshal(body, &cz); err != nil {
			t.Fatalf("Got an error unmarshalling the body: %v", err)
		}
		resp.Body.Close()
		goal := *expected[i]
		// We have only 1 subscription per channel, so DeepEqual will be ok.
		if !reflect.DeepEqual(cz, goal) {
			t.Fatalf("Iter=%v - Path=%q - Expected to get %v, got %v", i, ChannelsPath+paths[i], goal, cz)
		}
	}

	// Ask for a channel that does not exist
	url := fmt.Sprintf("http://%s:%d%s", monitorHost, monitorPort, ChannelsPath+"?channel=donotexist")
	resp, err := http.Get(url)
	if err != nil {
		stackFatalf(t, "Expected no error: Got %v\n", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		stackFatalf(t, "Expected a 404 response, got %d\n", resp.StatusCode)
	}
}

package zka

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"testing"
	"time"
)

func acquireTestCardLease(t *testing.T, socket, operation string) (net.Conn, <-chan cardLeaseResponse) {
	t.Helper()
	conn, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(conn).Encode(cardLeaseRequest{Version: cardLeaseProtocolVersion, Operation: operation}); err != nil {
		conn.Close()
		t.Fatal(err)
	}
	response := make(chan cardLeaseResponse, 1)
	go func() {
		var reply cardLeaseResponse
		_ = json.NewDecoder(conn).Decode(&reply)
		response <- reply
	}()
	return conn, response
}

func TestCardLeaseSocketSerializesClientsUntilHolderCloses(t *testing.T) {
	d, err := newTestDaemon(t, testRoot(t), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	serveTestDaemon(t, d)
	info, err := os.Stat(d.paths.CardLeaseSocket)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("card lease socket mode = %v, %v", info, err)
	}

	first, firstReply := acquireTestCardLease(t, d.paths.CardLeaseSocket, "pivb-mint")
	if reply := <-firstReply; !reply.OK || reply.Version != cardLeaseProtocolVersion {
		t.Fatalf("first lease response = %#v", reply)
	}
	second, secondReply := acquireTestCardLease(t, d.paths.CardLeaseSocket, "pivb-describe")
	defer second.Close()
	select {
	case reply := <-secondReply:
		t.Fatalf("second client acquired while first held lease: %#v", reply)
	case <-time.After(20 * time.Millisecond):
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case reply := <-secondReply:
		if !reply.OK || reply.Version != cardLeaseProtocolVersion {
			t.Fatalf("second lease response = %#v", reply)
		}
	case <-time.After(time.Second):
		t.Fatal("second client did not acquire after first disconnected")
	}
}

func TestSmartCardLeaseSerializesAndCancels(t *testing.T) {
	lease := newSmartCardLease()
	release, err := lease.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := lease.acquire(ctx); err == nil {
		t.Fatal("second lease acquired concurrently")
	}
	release()
	release()
	releaseAgain, err := lease.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	releaseAgain()
}

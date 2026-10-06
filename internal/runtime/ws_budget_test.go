package runtime

import (
	"bufio"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

func TestWebSocketByteBudget(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	ws := newWSConn(a, bufio.NewReader(a), false)
	shared := &wsByteBudget{}
	shared.lower(1024)
	ws.sharedBudget = shared
	ws.budget.lower(512)
	go ws.writer()
	// The first write blocks on net.Pipe; in-flight bytes must remain charged.
	if err := ws.enqueue(strings.Repeat("x", 400)); err != nil {
		t.Fatal(err)
	}
	if got := ws.budget.used.Load(); got != 400 {
		t.Fatal("in-flight bytes", got)
	}
	if err := ws.enqueue(strings.Repeat("x", 113)); err == nil {
		t.Fatal("connection byte limit not enforced")
	}
	if ws.budget.used.Load() > 512 || shared.used.Load() > 1024 {
		t.Fatal("budget exceeded")
	}
	b.Close()
	deadline := time.Now().Add(time.Second)
	for ws.budget.used.Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if ws.budget.used.Load() != 0 || shared.used.Load() != 0 {
		t.Fatal("close leaked byte reservations", ws.budget.used.Load(), shared.used.Load())
	}
}
func TestWebSocketSharedByteBudget(t *testing.T) {
	shared := &wsByteBudget{}
	shared.lower(600)
	conns := make([]*wsConn, 2)
	peers := make([]net.Conn, 2)
	for i := range conns {
		a, b := net.Pipe()
		defer a.Close()
		defer b.Close()
		conns[i] = newWSConn(a, bufio.NewReader(a), false)
		conns[i].sharedBudget = shared
		conns[i].budget.lower(512)
		peers[i] = b
		go conns[i].writer()
	}
	if err := conns[0].enqueue(strings.Repeat("x", 400)); err != nil {
		t.Fatal(err)
	}
	if err := conns[1].enqueue(strings.Repeat("x", 201)); err == nil {
		t.Fatal("flat byte limit not enforced")
	}
	if got := shared.used.Load(); got != 400 {
		t.Fatal(got)
	}
	shared.lower(1000)
	if shared.limit.Load() != 600 {
		t.Fatal("budget can be raised")
	}
	for _, c := range conns {
		c.end(1000, "")
	}
	for _, p := range peers {
		p.Close()
	}
	deadline := time.Now().Add(time.Second)
	for shared.used.Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if shared.used.Load() != 0 {
		t.Fatal("shared budget leaked")
	}
}

// Queue the rejection before starting the writer so close cannot overtake it.
func TestWebSocketByteBudgetClosePreservesQueuedMessages(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	ws := newWSConn(a, bufio.NewReader(a), false)
	peer := newWSConn(b, bufio.NewReader(b), true)
	ws.budget.lower(1024)
	for _, message := range []string{"rejected", "reason"} {
		if err := ws.enqueue(message); err != nil {
			t.Fatal(err)
		}
	}
	ws.end(1008, "readonly")
	go ws.writer()
	for _, want := range []string{"rejected", "reason"} {
		got, err := peer.readMessage()
		if err != nil || got != want {
			t.Fatalf("message %q, want %q: %v", got, want, err)
		}
	}
	_, err := peer.readMessage()
	var closed *wsCloseError
	if !errors.As(err, &closed) || closed.Code != 1008 || closed.Reason != "readonly" {
		t.Fatalf("close: %v", err)
	}
	<-ws.gone
	if ws.budget.used.Load() != 0 {
		t.Fatal("close leaked reservations")
	}
}

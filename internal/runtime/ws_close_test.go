package runtime

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/wsconn"
)

func TestWsCloseCodeNormalization(t *testing.T) {
	cases := []struct {
		in   int
		want int
	}{
		{0, 1000},
		{999, 1000},
		{1000, 1000},
		{1001, 1001},
		{1002, 1002},
		{1003, 1003},
		{1004, 1000},
		{1005, 1000},
		{1006, 1000},
		{1007, 1007},
		{1008, 1008},
		{1009, 1009},
		{1010, 1010},
		{1011, 1011},
		{1012, 1012},
		{1013, 1013},
		{1014, 1014},
		{1015, 1000},
		{1016, 1000},
		{1999, 1000},
		{2000, 1000},
		{2999, 1000},
		{3000, 3000},
		{3999, 3999},
		{4000, 4000},
		{4999, 4999},
		{5000, 1000},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("code_%d", tc.in), func(t *testing.T) {
			if got := closeCode(tc.in); got != tc.want {
				t.Fatalf("closeCode(%d) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

func TestWsCloseEchoSanitization(t *testing.T) {
	cases := []struct {
		name     string
		sendCode int
		wantEcho int
	}{
		{"valid_1000", 1000, 1000},
		{"valid_1001", 1001, 1001},
		{"valid_4000", 4000, 4000},
		{"no_status_1005", 1005, 1000},
		{"reserved_1006", 1006, 1000},
		{"reserved_1015", 1015, 1000},
		{"reserved_2000", 2000, 1000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &wsHub{
				conns:  make(map[int64]*wsconn.Conn),
				events: make(chan wsEvent, 8),
				stop:   make(chan struct{}),
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				h.serve(w, r, r.URL.String(), nil)
			}))
			defer server.Close()
			address := strings.TrimPrefix(server.URL, "http://")
			client, err := wsconn.Dial(t.Context(), func(ctx context.Context) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "tcp", address)
			}, address, "/ws")
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close(1006, "")

			select {
			case ev := <-h.events:
				if ev.Type != "open" {
					t.Fatalf("expected open event, got %v", ev.Type)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("timeout waiting for open event")
			}

			if tc.sendCode == 1005 {
				if err := client.WriteFrame(wsconn.OpClose, nil); err != nil {
					t.Fatal(err)
				}
			} else {
				p := make([]byte, 2)
				binary.BigEndian.PutUint16(p, uint16(tc.sendCode))
				if err := client.WriteFrame(wsconn.OpClose, p); err != nil {
					t.Fatal(err)
				}
			}

			_, err = client.ReadMessage(MaxWSMessage)
			ce, ok := errors.AsType[*wsconn.CloseError](err)
			if !ok {
				t.Fatalf("expected CloseError, got %v", err)
			}
			if ce.Code != tc.wantEcho {
				t.Fatalf("echoed code = %d, want %d", ce.Code, tc.wantEcho)
			}

			select {
			case ev := <-h.events:
				if ev.Type != "close" {
					t.Fatalf("expected close event, got %v", ev.Type)
				}
				if ev.Code != tc.sendCode {
					t.Fatalf("event code = %d, want %d", ev.Code, tc.sendCode)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("timeout waiting for close event")
			}
		})
	}
}

func TestWsHubCloseSanitization(t *testing.T) {
	cases := []struct {
		name      string
		closeCode int
		wantCode  int
	}{
		{"valid_1000", 1000, 1000},
		{"valid_4000", 4000, 4000},
		{"reserved_1015", 1015, 1000},
		{"reserved_2000", 2000, 1000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &wsHub{
				conns:  make(map[int64]*wsconn.Conn),
				events: make(chan wsEvent, 8),
				stop:   make(chan struct{}),
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				h.serve(w, r, r.URL.String(), nil)
			}))
			defer server.Close()
			address := strings.TrimPrefix(server.URL, "http://")
			client, err := wsconn.Dial(t.Context(), func(ctx context.Context) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "tcp", address)
			}, address, "/ws")
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close(1006, "")

			var connID int64
			select {
			case ev := <-h.events:
				if ev.Type != "open" {
					t.Fatalf("expected open event, got %v", ev.Type)
				}
				connID = ev.ID
			case <-time.After(3 * time.Second):
				t.Fatal("timeout waiting for open event")
			}

			if err := h.close(connID, tc.closeCode, "bye"); err != nil {
				t.Fatal(err)
			}

			_, err = client.ReadMessage(MaxWSMessage)
			ce, ok := errors.AsType[*wsconn.CloseError](err)
			if !ok {
				t.Fatalf("expected CloseError, got %v", err)
			}
			if ce.Code != tc.wantCode {
				t.Fatalf("received code = %d, want %d", ce.Code, tc.wantCode)
			}
		})
	}
}

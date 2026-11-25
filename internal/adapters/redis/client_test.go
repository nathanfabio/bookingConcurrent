package redis

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/nathanfabio/bookingConcurrent/internal/platform/config"
)

func TestClientHonorsDeadlineDuringStalledRedisIO(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	connected := make(chan struct{})
	connections := make(chan net.Conn, 1)
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		connections <- conn
		close(connected)
		// Consume Redis's handshake/commands without ever replying. A
		// background-context read would stall for the default socket timeout
		// instead of respecting the much shorter caller deadline.
		_, _ = io.Copy(io.Discard, conn)
	}()
	client := NewClient(config.Redis{Addr: listener.Addr().String()})
	defer func() {
		_ = client.Close()
		_ = listener.Close()
		select {
		case conn := <-connections:
			_ = conn.Close()
		case <-done:
		}
		<-done
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	err = client.Ping(ctx).Err()
	if err == nil || time.Since(started) > time.Second {
		t.Fatalf("stalled Redis I/O must honor caller deadline: elapsed=%s err=%v", time.Since(started), err)
	}
	select {
	case <-connected:
	default:
		t.Fatal("test did not reach the stalled Redis socket")
	}
}

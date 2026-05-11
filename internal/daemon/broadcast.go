package daemon

import (
	"log/slog"
	"net"
	"os"
	"sync"
	"time"
)

const (
	clientQueueSize    = 128
	clientWriteTimeout = 5 * time.Second
)

type broadcaster struct {
	mu      sync.Mutex
	clients map[*bcastClient]struct{}
	sseSubs map[chan []byte]struct{}
}

type bcastClient struct {
	conn net.Conn
	ch   chan []byte
	once sync.Once
}

func (c *bcastClient) close() {
	c.once.Do(func() {
		close(c.ch)
		c.conn.Close()
	})
}

func newBroadcaster() *broadcaster {
	return &broadcaster{
		clients: make(map[*bcastClient]struct{}),
		sseSubs: make(map[chan []byte]struct{}),
	}
}

// add registers a new client and starts its write goroutine.
func (b *broadcaster) add(conn net.Conn) {
	c := &bcastClient{conn: conn, ch: make(chan []byte, clientQueueSize)}
	b.mu.Lock()
	b.clients[c] = struct{}{}
	b.mu.Unlock()
	go b.serve(c)
}

// serve writes queued events to a client connection; unregisters on exit.
func (b *broadcaster) serve(c *bcastClient) {
	defer func() {
		b.mu.Lock()
		delete(b.clients, c)
		b.mu.Unlock()
		c.close()
	}()
	for data := range c.ch {
		c.conn.SetWriteDeadline(time.Now().Add(clientWriteTimeout))
		if _, err := c.conn.Write(data); err != nil {
			return
		}
	}
}

// dispatch fans out a JSON event line to all connected clients and SSE subscribers.
func (b *broadcaster) dispatch(data []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for c := range b.clients {
		select {
		case c.ch <- data:
		default:
			// slow client: evict
			slog.Warn("broadcast.slow_client, evicting", "remote", c.conn.RemoteAddr())
			go c.close()
			delete(b.clients, c)
		}
	}
	for ch := range b.sseSubs {
		select {
		case ch <- data:
		default:
			// drop event for slow SSE client
		}
	}
}

// subscribeSSE registers a new channel for SSE events.
func (b *broadcaster) subscribeSSE() chan []byte {
	ch := make(chan []byte, clientQueueSize)
	b.mu.Lock()
	b.sseSubs[ch] = struct{}{}
	b.mu.Unlock()
	return ch
}

// unsubscribeSSE removes the channel from SSE subscriptions.
func (b *broadcaster) unsubscribeSSE(ch chan []byte) {
	b.mu.Lock()
	delete(b.sseSubs, ch)
	b.mu.Unlock()
	close(ch)
}

// closeAll disconnects every client.
func (b *broadcaster) closeAll() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for c := range b.clients {
		c.close()
	}
	b.clients = make(map[*bcastClient]struct{})
}

// runEventSocket listens for APP connections on the Unix event socket.
func (d *Daemon) runEventSocket() {
	defer d.wg.Done()
	path := d.cfg().Transport.EventSocket
	os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		slog.Error("event socket listen failed", "path", path, "err", err)
		return
	}
	defer ln.Close()
	if err := os.Chmod(path, 0666); err != nil {
		slog.Warn("event socket chmod failed", "path", path, "err", err)
	}
	slog.Info("event socket listening", "path", path)

	go func() {
		<-d.ctx.Done()
		ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-d.ctx.Done():
				return
			default:
				slog.Warn("event socket accept error", "err", err)
				time.Sleep(100 * time.Millisecond)
			}
			continue
		}
		slog.Info("socket.client.connected", "remote", conn.RemoteAddr())
		d.bcast.add(conn)
	}
}

// runBroadcaster reads from the event channel and fans out to all clients.
func (d *Daemon) runBroadcaster() {
	defer d.wg.Done()
	for {
		select {
		case data := <-d.eventCh:
			d.bcast.dispatch(data)
		case <-d.ctx.Done():
			d.bcast.closeAll()
			return
		}
	}
}

package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type observedListener struct {
	net.Listener
	entered chan struct{}
	once    sync.Once
}

func (l *observedListener) Accept() (net.Conn, error) {
	l.once.Do(func() { close(l.entered) })
	return l.Listener.Accept()
}

// handoffListener returns an already accepted connection only after shutdown.
// This deterministically covers cancellation between Accept and registration.
type handoffListener struct {
	conn     net.Conn
	accepted chan struct{}
	release  chan struct{}
	once     sync.Once
}

func (l *handoffListener) Accept() (net.Conn, error) {
	close(l.accepted)
	<-l.release
	return l.conn, nil
}
func (l *handoffListener) Close() error   { l.once.Do(func() { close(l.release) }); return nil }
func (l *handoffListener) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)} }

func waitServer(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("server did not stop and join handlers")
	}
}

func TestServerShutdownAfterAcceptBeforeRegistration(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()
	listener := &handoffListener{conn: serverConn, accepted: make(chan struct{}), release: make(chan struct{})}
	server, _ := NewServer(0, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.serve(ctx, listener) }()
	<-listener.accepted
	cancel()
	waitServer(t, done)
	clientConn.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := clientConn.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("unregistered connection was not closed: %v", err)
	}
}

func TestServerShutdownActiveConnections(t *testing.T) {
	for _, mode := range []string{"idle", "passive-wait", "upload", "download"} {
		t.Run(mode, func(t *testing.T) {
			home := t.TempDir()
			file, err := os.Create(filepath.Join(home, "large"))
			if err != nil {
				t.Fatal(err)
			}
			if err := file.Truncate(64 << 20); err != nil {
				file.Close()
				t.Fatal(err)
			}
			file.Close()
			listener, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			server, _ := NewServer(0, Config{"anonymous": {HomeDir: home}})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- server.serve(ctx, listener) }()
			control, err := net.Dial("tcp4", listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer control.Close()
			control.SetDeadline(time.Now().Add(5 * time.Second))
			reader := bufio.NewReader(control)
			expect := func(code string) string {
				t.Helper()
				line, err := reader.ReadString('\n')
				if err != nil || !strings.HasPrefix(line, code+" ") {
					t.Fatalf("expected %s, got %q, %v", code, line, err)
				}
				return line
			}
			expect("220")
			fmt.Fprint(control, "USER anonymous\r\n")
			expect("230")
			var data net.Conn
			if mode != "idle" {
				fmt.Fprint(control, "PASV\r\n")
				reply := expect("227")
				var h1, h2, h3, h4, p1, p2 int
				if _, err := fmt.Sscanf(reply, "227 Entering Passive Mode (%d,%d,%d,%d,%d,%d)", &h1, &h2, &h3, &h4, &p1, &p2); err != nil {
					t.Fatal(err)
				}
				if mode == "passive-wait" {
					server.mu.Lock()
					var c *FTPConn
					for conn := range server.conns {
						c = conn
					}
					server.mu.Unlock()
					if c == nil {
						t.Fatal("connection not registered")
					}
					c.mu.Lock()
					observed := &observedListener{Listener: c.listener, entered: make(chan struct{})}
					c.listener = observed
					c.mu.Unlock()
					fmt.Fprint(control, "STOR pending\r\n")
					select {
					case <-observed.entered:
					case <-time.After(time.Second):
						t.Fatal("passive Accept not reached")
					}
				} else {
					data, err = net.Dial("tcp4", fmt.Sprintf("%d.%d.%d.%d:%d", h1, h2, h3, h4, p1*256+p2))
					if err != nil {
						t.Fatal(err)
					}
					defer data.Close()
					if mode == "download" {
						fmt.Fprint(control, "RETR large\r\n")
					} else {
						fmt.Fprint(control, "STOR partial\r\n")
					}
					expect("125")
				}
			}
			cancel()
			waitServer(t, done)
			server.mu.Lock()
			count := len(server.conns)
			server.mu.Unlock()
			if count != 0 {
				t.Fatalf("%d connection handlers remain registered", count)
			}
			// Drain buffered bytes, then require EOF, rather than treating a timeout as closure.
			if _, err := io.Copy(io.Discard, reader); err != nil {
				t.Fatalf("control connection not closed: %v", err)
			}
			if data != nil {
				data.SetReadDeadline(time.Now().Add(time.Second))
				if _, err := io.Copy(io.Discard, data); err != nil {
					t.Fatalf("data connection not closed: %v", err)
				}
			}
		})
	}
}

func TestServerStartAlreadyCanceled(t *testing.T) {
	server, _ := NewServer(0, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan error, 1)
	go func() { done <- server.Start(ctx) }()
	waitServer(t, done)
}

func TestConcurrentCloseDuringDataEstablishment(t *testing.T) {
	for i := 0; i < 30; i++ {
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		c, _ := securitySession(t, t.TempDir())
		observed := &observedListener{Listener: listener, entered: make(chan struct{})}
		c.pasv, c.listener = true, observed
		done := make(chan error, 1)
		go func() { _, err := c.establishDataConn(); done <- err }()
		<-observed.entered
		var closers sync.WaitGroup
		for j := 0; j < 4; j++ {
			closers.Add(1)
			go func() { defer closers.Done(); c.Close() }()
		}
		closers.Wait()
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("closed passive Accept succeeded")
			}
		case <-time.After(time.Second):
			t.Fatal("passive Accept leaked")
		}
	}
}

type failedListener struct{ net.Listener }

func (l failedListener) Accept() (net.Conn, error) { return nil, errors.New("test listener failure") }
func TestServerAcceptFailureReturns(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	server, _ := NewServer(0, nil)
	if err := server.serve(context.Background(), failedListener{listener}); err == nil {
		t.Fatal("accept failure lost")
	}
}

func TestConcurrentCloseDuringActiveDial(t *testing.T) {
	for i := 0; i < 30; i++ {
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		c, _ := securitySession(t, t.TempDir())
		c.dataHost, c.dataPort = "127.0.0.1", listener.Addr().(*net.TCPAddr).Port
		done := make(chan error, 1)
		go func() { _, err := c.establishDataConn(); done <- err }()
		c.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			listener.Close()
			t.Fatal("active dial did not finish after Close")
		}
		listener.(*net.TCPListener).SetDeadline(time.Now().Add(time.Millisecond))
		peer, err := listener.Accept()
		listener.Close()
		if err == nil {
			peer.SetReadDeadline(time.Now().Add(time.Second))
			_, readErr := peer.Read(make([]byte, 1))
			peer.Close()
			if readErr != io.EOF {
				t.Fatalf("late active data connection leaked: %v", readErr)
			}
		}
	}
}

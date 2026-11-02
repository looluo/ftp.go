package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"sync"
)

// Server represents an FTP server.
type Server struct {
	port     int
	listener net.Listener
	config   Config
	mu       sync.Mutex
	conns    map[*FTPConn]struct{}
}

// NewServer creates a new FTP server.
func NewServer(port int, config Config) (*Server, error) {
	return &Server{port: port, config: config, conns: make(map[*FTPConn]struct{})}, nil
}

// Start serves until cancellation, then waits for every connection handler.
func (s *Server) Start(ctx context.Context) error {
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", s.port))
	if err != nil {
		return fmt.Errorf("failed to listen: %w", err)
	}
	return s.serve(ctx, listener)
}

func (s *Server) serve(parent context.Context, listener net.Listener) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	s.mu.Lock()
	s.listener = listener
	s.mu.Unlock()
	log.Printf("FTP server listening on %s", listener.Addr())

	// Closing the listener unblocks Accept; closing connections unblocks commands,
	// passive Accept, active DialContext, and in-progress transfers.
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		listener.Close()
		s.closeAllConnections()
	}()

	var handlers sync.WaitGroup
	var acceptErr error
	for {
		conn, err := listener.Accept()
		if err != nil {
			acceptErr = err
			break
		}
		s.mu.Lock()
		if ctx.Err() != nil {
			s.mu.Unlock()
			conn.Close()
			break
		}
		ftpConn := NewFTPConn(conn, s.config)
		s.conns[ftpConn] = struct{}{}
		handlers.Add(1)
		s.mu.Unlock()
		go func() {
			defer handlers.Done()
			defer s.untrackConnection(ftpConn)
			ftpConn.Handle()
		}()
	}
	cancel()
	<-shutdownDone
	handlers.Wait()
	if parent.Err() != nil {
		return nil
	}
	return fmt.Errorf("accept failed: %w", acceptErr)
}

func (s *Server) untrackConnection(conn *FTPConn) {
	s.mu.Lock()
	delete(s.conns, conn)
	s.mu.Unlock()
}

func (s *Server) closeAllConnections() {
	s.mu.Lock()
	conns := make([]*FTPConn, 0, len(s.conns))
	for conn := range s.conns {
		conns = append(conns, conn)
	}
	s.mu.Unlock()
	for _, conn := range conns {
		conn.Close()
	}
}

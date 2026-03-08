package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"sync"
)

// Server represents an FTP server
type Server struct {
	port     int
	listener net.Listener
	config   Config
	mu       sync.Mutex
	conns    map[*FTPConn]struct{}
}

// NewServer creates a new FTP server
func NewServer(port int, config Config) (*Server, error) {
	return &Server{
		port:   port,
		config: config,
		conns:  make(map[*FTPConn]struct{}),
	}, nil
}

// Start starts the FTP server
func (s *Server) Start(ctx context.Context) error {
	laddr := fmt.Sprintf(":%d", s.port)
	listener, err := net.Listen("tcp", laddr)
	if err != nil {
		return fmt.Errorf("failed to listen: %w", err)
	}
	s.listener = listener
	defer listener.Close()

	log.Printf("FTP server listening on %s", laddr)

	// Accept connections in a goroutine
	connChan := make(chan net.Conn)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				select {
				case <-ctx.Done():
					return
				default:
					log.Printf("Accept error: %v", err)
					continue
				}
			}
			connChan <- conn
		}
	}()

	// Handle connections
	for {
		select {
		case <-ctx.Done():
			log.Println("Shutting down server...")
			s.closeAllConnections()
			return nil
		case conn := <-connChan:
			log.Printf("New connection from %s", conn.RemoteAddr())
			ftpConn := NewFTPConn(conn, s.config)
			s.trackConnection(ftpConn)
			go func() {
				defer s.untrackConnection(ftpConn)
				ftpConn.Handle()
			}()
		}
	}
}

func (s *Server) trackConnection(conn *FTPConn) {
	s.mu.Lock()
	s.conns[conn] = struct{}{}
	s.mu.Unlock()
}

func (s *Server) untrackConnection(conn *FTPConn) {
	s.mu.Lock()
	delete(s.conns, conn)
	s.mu.Unlock()
}

func (s *Server) closeAllConnections() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for conn := range s.conns {
		conn.Close()
	}
}

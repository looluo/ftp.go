package main

import (
	"net"
	"path/filepath"
	"testing"
)

func TestParsePath(t *testing.T) {
	tmpDir := t.TempDir()

	conn := &FTPConn{
		homeDir: tmpDir,
		currDir: "/",
	}

	tests := []struct {
		name        string
		input       string
		wantRemote  string
		shouldError bool
	}{
		{
			name:        "simple path",
			input:       "test.txt",
			wantRemote:  "/test.txt",
			shouldError: false,
		},
		{
			name:        "absolute path",
			input:       "/dir/file.txt",
			wantRemote:  "/dir/file.txt",
			shouldError: false,
		},
		{
			name:        "current directory",
			input:       ".",
			wantRemote:  "/",
			shouldError: false,
		},
		{
			name:        "parent directory",
			input:       "..",
			wantRemote:  "/",
			shouldError: false,
		},
		{
			name:        "path traversal attempt",
			input:       "../../../etc/passwd",
			wantRemote:  "/etc/passwd", // path.Clean will normalize it
			shouldError: false,         // It's handled by security check, not error
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			remote, local, err := conn.parsePath(tt.input)

			if tt.shouldError {
				if err == nil {
					t.Errorf("Expected error, got nil")
				}
				return
			}

			if err != nil {
				t.Errorf("Unexpected error: %v", err)
				return
			}

			if remote != tt.wantRemote {
				t.Errorf("Expected remote '%s', got '%s'", tt.wantRemote, remote)
			}

			// Verify local path is within homeDir
			absHome, _ := filepath.Abs(tmpDir)
			absLocal, _ := filepath.Abs(local)
			if absLocal[:len(absHome)] != absHome {
				t.Errorf("Local path '%s' escapes home directory '%s'", absLocal, absHome)
			}
		})
	}
}

func TestParsePathWithCurrentDir(t *testing.T) {
	tmpDir := t.TempDir()

	conn := &FTPConn{
		homeDir: tmpDir,
		currDir: "/subdir",
	}

	remote, _, err := conn.parsePath("file.txt")
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
		return
	}

	expected := "/subdir/file.txt"
	if remote != expected {
		t.Errorf("Expected remote '%s', got '%s'", expected, remote)
	}
}

func TestPathTraversalSecurity(t *testing.T) {
	tmpDir := t.TempDir()

	conn := &FTPConn{
		homeDir: tmpDir,
		currDir: "/",
	}

	// Test various path traversal attempts
	traversalAttempts := []string{
		"../",
		"..",
		"../..",
		"../../..",
		"../../../etc/passwd",
		"..\\..\\..\\windows\\system32",
		"/../../etc/passwd",
	}

	for _, attempt := range traversalAttempts {
		t.Run(attempt, func(t *testing.T) {
			_, local, err := conn.parsePath(attempt)

			// Should either error or stay within home directory
			if err == nil {
				absHome, _ := filepath.Abs(tmpDir)
				absLocal, _ := filepath.Abs(local)

				// Path must start with home directory
				if len(absLocal) < len(absHome) || absLocal[:len(absHome)] != absHome {
					t.Errorf("Path traversal succeeded! Path '%s' escapes home '%s'", absLocal, absHome)
				}
			}
		})
	}
}

func TestParseCommand(t *testing.T) {
	conn := &FTPConn{}

	tests := []struct {
		input    string
		wantCmd  string
		wantArg  string
	}{
		{"USER test", "USER", "test"},
		{"PASS", "PASS", ""},
		{"CWD /home", "CWD", "/home"},
		{"list", "LIST", ""},
		{"QUIT ", "QUIT", ""},
	}

	for _, tt := range tests {
		cmd, arg := conn.parseCommand(tt.input)
		if cmd != tt.wantCmd {
			t.Errorf("Expected cmd '%s', got '%s'", tt.wantCmd, cmd)
		}
		if arg != tt.wantArg {
			t.Errorf("Expected arg '%s', got '%s'", tt.wantArg, arg)
		}
	}
}

// Mock net.Conn for testing
type mockConn struct {
	net.Conn
}

func (m *mockConn) Write(b []byte) (n int, err error) {
	return len(b), nil
}

func (m *mockConn) Close() error {
	return nil
}

func TestFTPConnSendMsg(t *testing.T) {
	// This is a basic test to ensure sendMsg doesn't panic
	conn := &FTPConn{
		conn: &mockConn{},
	}

	conn.sendMsg(220, "Test message")
}

func TestGetExternalIP(t *testing.T) {
	// Just verify it doesn't error on a system with network
	ip, err := getExternalIP()
	// May fail in isolated environments, so we just log
	if err != nil {
		t.Logf("getExternalIP returned error (expected in some environments): %v", err)
	} else {
		t.Logf("getExternalIP returned: %s", ip)
	}
}

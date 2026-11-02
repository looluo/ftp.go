package main

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type securityControl struct {
	mockConn
	replies bytes.Buffer
	peer    net.IP
}

func (c *securityControl) Write(p []byte) (int, error) { return c.replies.Write(p) }
func (c *securityControl) RemoteAddr() net.Addr        { return &net.TCPAddr{IP: c.peer, Port: 40000} }

func securitySession(t *testing.T, home string) (*FTPConn, *securityControl) {
	t.Helper()
	control := &securityControl{peer: net.ParseIP("127.0.0.1")}
	c := NewFTPConn(control, Config{"anonymous": {HomeDir: home}})
	c.handleUSER("anonymous")
	t.Cleanup(c.Close)
	control.replies.Reset()
	return c, control
}

func TestSecurityDELEMustNotEscapeHomeViaSymlink(t *testing.T) {
	base := t.TempDir()
	home, outside := filepath.Join(base, "home"), filepath.Join(base, "home-backup")
	for _, dir := range []string{home, outside} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	victim := filepath.Join(outside, "victim.txt")
	if err := os.WriteFile(victim, []byte("must survive"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(home, "link")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	c, control := securitySession(t, home)
	c.handleDELE("link/victim.txt")
	got, err := os.ReadFile(victim)
	if err != nil || string(got) != "must survive" {
		t.Fatalf("root-external victim changed/deleted: %q, %v; reply: %s", got, err, control.replies.String())
	}
	if !strings.HasPrefix(control.replies.String(), "550 ") {
		t.Fatalf("expected rejection, got %s", control.replies.String())
	}
}

func TestSecurityPORTValidation(t *testing.T) {
	valid := []string{"127", "0", "0", "1", "200", "1"}
	invalid := []string{"127,0,0,1,0", "127,0,0,1,200,1,2", "127,0,0,1,0,0", "127,0,0,1,0,21", "127,0,0,2,200,1"}
	for i := range valid {
		for _, bad := range []string{"-1", "256", "x", "", "+1", " 1"} {
			fields := append([]string(nil), valid...)
			fields[i] = bad
			invalid = append(invalid, strings.Join(fields, ","))
		}
	}
	for _, arg := range invalid {
		t.Run(arg, func(t *testing.T) {
			c, control := securitySession(t, t.TempDir())
			c.dataHost, c.dataPort, c.pasv = "127.0.0.1", 51201, true
			c.handlePORT(arg)
			if !strings.HasPrefix(control.replies.String(), "501 ") {
				t.Errorf("invalid PORT accepted: %s", control.replies.String())
			}
			if c.dataHost != "127.0.0.1" || c.dataPort != 51201 || !c.pasv {
				t.Errorf("invalid request changed state: %s:%d, pasv=%v", c.dataHost, c.dataPort, c.pasv)
			}
		})
	}
	t.Run("valid", func(t *testing.T) {
		c, control := securitySession(t, t.TempDir())
		c.handlePORT(strings.Join(valid, ","))
		if !strings.HasPrefix(control.replies.String(), "200 ") || c.dataHost != "127.0.0.1" || c.dataPort != 51201 || c.pasv {
			t.Fatalf("valid PORT failed: %s (%s)", control.replies.String(), fmt.Sprint(c.dataPort))
		}
	})
}

// transferCommand drives real active TCP data connections, including rejected files.
func transferCommand(t *testing.T, c *FTPConn, control *securityControl, cmd, arg, upload string) string {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	listener.(*net.TCPListener).SetDeadline(time.Now().Add(3 * time.Second))
	port := listener.Addr().(*net.TCPAddr).Port
	c.handlePORT(fmt.Sprintf("127,0,0,1,%d,%d", port/256, port%256))
	if !strings.Contains(control.replies.String(), "200 PORT") {
		t.Fatalf("PORT failed: %s", control.replies.String())
	}
	control.replies.Reset()
	received := make(chan string, 1)
	go func() {
		data, err := listener.Accept()
		if err != nil {
			received <- ""
			return
		}
		defer data.Close()
		data.SetDeadline(time.Now().Add(3 * time.Second))
		if cmd == "STOR" {
			io.WriteString(data, upload)
			received <- ""
			return
		}
		payload, _ := io.ReadAll(data)
		received <- string(payload)
	}()
	commands[cmd](c, arg)
	select {
	case payload := <-received:
		return payload
	case <-time.After(4 * time.Second):
		t.Fatal("data connection did not finish")
		return ""
	}
}

func TestSecurityFilesystemCommandsRejectExternalSymlinks(t *testing.T) {
	for _, targetKind := range []string{"absolute", "relative"} {
		for _, command := range []string{"CWD", "LIST", "RETR", "STOR", "STOR-new", "RETR-file-link", "STOR-file-link", "STOR-dangling-link", "MKD", "RMD", "DELE", "RNFR", "RNTO"} {
			t.Run(targetKind+"/"+command, func(t *testing.T) {
				base := t.TempDir()
				home, outside := filepath.Join(base, "home"), filepath.Join(base, "home-backup")
				for _, dir := range []string{home, outside, filepath.Join(outside, "empty")} {
					if err := os.Mkdir(dir, 0700); err != nil {
						t.Fatal(err)
					}
				}
				victim := filepath.Join(outside, "victim.txt")
				if err := os.WriteFile(victim, []byte("outside secret"), 0600); err != nil {
					t.Fatal(err)
				}
				target := outside
				if targetKind == "relative" {
					target = "../home-backup"
				}
				if err := os.Symlink(target, filepath.Join(home, "link")); err != nil {
					t.Skipf("symlink unavailable: %v", err)
				}
				c, control := securitySession(t, home)
				arg := "link/victim.txt"
				var payload string
				switch command {
				case "CWD":
					c.handleCWD("link")
				case "LIST":
					c.currDir = "/link"
					payload = transferCommand(t, c, control, "LIST", "", "")
				case "RETR", "STOR":
					payload = transferCommand(t, c, control, command, arg, "overwrite")
				case "STOR-new":
					payload = transferCommand(t, c, control, "STOR", "link/new.txt", "new")
				case "RETR-file-link", "STOR-file-link", "STOR-dangling-link":
					name := "victim.txt"
					if command == "STOR-dangling-link" {
						name = "new.txt"
					}
					if err := os.Symlink(filepath.Join(target, name), filepath.Join(home, "file-link")); err != nil {
						t.Fatal(err)
					}
					cmd := "STOR"
					if command == "RETR-file-link" {
						cmd = "RETR"
					}
					payload = transferCommand(t, c, control, cmd, "file-link", "overwrite")
				case "MKD":
					c.handleMKD("link/new/subdir")
				case "RMD":
					c.handleRMD("link/empty")
				case "DELE":
					c.handleDELE(arg)
				case "RNFR":
					c.handleRNFR(arg)
				case "RNTO":
					if err := os.WriteFile(filepath.Join(home, "source"), []byte("inside"), 0600); err != nil {
						t.Fatal(err)
					}
					c.handleRNFR("source")
					control.replies.Reset()
					c.handleRNTO(arg)
				}
				if !strings.HasPrefix(control.replies.String(), "550 ") {
					t.Errorf("expected rejection, got %s", control.replies.String())
				}
				if strings.Contains(payload, "outside secret") || strings.Contains(payload, "victim.txt") {
					t.Errorf("external content leaked: %q", payload)
				}
				got, err := os.ReadFile(victim)
				if err != nil || string(got) != "outside secret" {
					t.Fatalf("external victim changed: %q, %v", got, err)
				}
				if _, err := os.Stat(filepath.Join(outside, "empty")); err != nil {
					t.Errorf("external directory removed: %v", err)
				}
				for _, name := range []string{"new", "new.txt"} {
					if _, err := os.Stat(filepath.Join(outside, name)); !os.IsNotExist(err) {
						t.Errorf("external path created: %s, %v", name, err)
					}
				}
			})
		}
	}
}

func TestSecurityRenameSourceReplacedAfterRNFR(t *testing.T) {
	base := t.TempDir()
	home, outside := filepath.Join(base, "home"), filepath.Join(base, "home-backup")
	for _, dir := range []string{home, outside, filepath.Join(home, "dir")} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, dir := range []string{outside, filepath.Join(home, "dir")} {
		if err := os.WriteFile(filepath.Join(dir, "victim"), []byte("survive"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	c, control := securitySession(t, home)
	c.handleRNFR("dir/victim")
	if !strings.HasPrefix(control.replies.String(), "350 ") {
		t.Fatal(control.replies.String())
	}
	if err := os.Rename(filepath.Join(home, "dir"), filepath.Join(home, "original")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(home, "dir")); err != nil {
		t.Skip(err)
	}
	control.replies.Reset()
	c.handleRNTO("stolen")
	if !strings.HasPrefix(control.replies.String(), "550 ") {
		t.Fatal(control.replies.String())
	}
	got, err := os.ReadFile(filepath.Join(outside, "victim"))
	if err != nil || string(got) != "survive" {
		t.Fatalf("external source moved: %q, %v", got, err)
	}
}

func TestSecurityNormalFileOperations(t *testing.T) {
	home := t.TempDir()
	c, control := securitySession(t, home)
	c.handleMKD("dir/nested")
	control.replies.Reset()
	transferCommand(t, c, control, "STOR", "dir/nested/file", "inside payload")
	if !strings.Contains(control.replies.String(), "226 ") {
		t.Fatal(control.replies.String())
	}
	control.replies.Reset()
	got := transferCommand(t, c, control, "RETR", "../../../dir/nested/file", "")
	if got != "inside payload" {
		t.Fatalf("normalized virtual-root RETR: %q", got)
	}
	if err := os.Symlink("dir", filepath.Join(home, "alias")); err != nil {
		t.Skip(err)
	}
	control.replies.Reset()
	got = transferCommand(t, c, control, "RETR", "alias/nested/file", "")
	if got != "inside payload" {
		t.Fatalf("safe internal symlink RETR: %q", got)
	}
	c.handleRNFR("dir/nested/file")
	c.handleRNTO("dir/renamed")
	control.replies.Reset()
	payload := transferCommand(t, c, control, "LIST", "", "")
	if !strings.Contains(payload, "dir") {
		t.Fatalf("LIST failed: %q", payload)
	}
	c.handleDELE("dir/renamed")
	c.handleRMD("dir/nested")
	if _, err := os.Stat(filepath.Join(home, "dir/nested")); !os.IsNotExist(err) {
		t.Fatalf("RMD failed: %v", err)
	}
}

func TestSecurityNativePathsRejected(t *testing.T) {
	c, control := securitySession(t, t.TempDir())
	for _, input := range []string{`..\home-backup\victim.txt`, `C:\home-backup\victim.txt`, `//host/share\victim`, "file:stream", "file\x00name"} {
		control.replies.Reset()
		c.handleMKD(input)
		if !strings.HasPrefix(control.replies.String(), "550 ") {
			t.Errorf("native path accepted %q: %s", input, control.replies.String())
		}
	}
}

func TestSecurityUserChangeClearsRootAndRename(t *testing.T) {
	c, control := securitySession(t, t.TempDir())
	c.config["private"] = UserConfig{HomeDir: t.TempDir(), Password: "secret"}
	c.currDir, c.renameTempPath = "/old", "source"
	c.handleUSER("private")
	if c.identified || c.renameTempPath != "" || c.currDir != "/" {
		t.Fatal("USER retained previous login state")
	}
	control.replies.Reset()
	c.handleMKD("forbidden")
	if !strings.HasPrefix(control.replies.String(), "530 ") {
		t.Fatal(control.replies.String())
	}
	c.handlePASS("secret")
	if !c.identified {
		t.Fatal("new user login failed")
	}
}

func TestSecurityActiveTargetRecheckedBeforeDial(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	c, control := securitySession(t, t.TempDir())
	control.peer = net.ParseIP("192.0.2.1")
	port := listener.Addr().(*net.TCPAddr).Port
	c.handlePORT(fmt.Sprintf("127,0,0,1,%d,%d", port/256, port%256))
	if !strings.HasPrefix(control.replies.String(), "501 ") {
		t.Fatal(control.replies.String())
	}
	// Even stale or directly corrupted state cannot bypass the dial-time policy.
	c.dataHost, c.dataPort = "127.0.0.1", port
	if data, err := c.establishDataConn(); err == nil {
		data.Close()
		t.Fatal("third-party target accepted")
	}
	listener.(*net.TCPListener).SetDeadline(time.Now().Add(30 * time.Millisecond))
	if data, err := listener.Accept(); err == nil {
		data.Close()
		t.Fatal("server connected to forbidden target")
	}
}

func TestSecurityLoginRootRemainsPinnedAcrossRename(t *testing.T) {
	base := t.TempDir()
	home, original := filepath.Join(base, "home"), filepath.Join(base, "original")
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	c, control := securitySession(t, home)
	if err := os.Rename(home, original); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "victim"), []byte("replacement directory"), 0600); err != nil {
		t.Fatal(err)
	}
	transferCommand(t, c, control, "STOR", "victim", "pinned root")
	got, err := os.ReadFile(filepath.Join(original, "victim"))
	if err != nil || string(got) != "pinned root" {
		t.Fatalf("login root changed after rename: %q, %v", got, err)
	}
	got, err = os.ReadFile(filepath.Join(home, "victim"))
	if err != nil || string(got) != "replacement directory" {
		t.Fatalf("replacement directory was modified: %q, %v", got, err)
	}
}

func TestSecuritySymlinkReplacementDuringMKD(t *testing.T) {
	base := t.TempDir()
	home, outside := filepath.Join(base, "home"), filepath.Join(base, "home-backup")
	for _, dir := range []string{home, outside, filepath.Join(home, "inside")} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("inside", filepath.Join(home, "swap")); err != nil {
		t.Skip(err)
	}
	c, control := securitySession(t, home)
	stop, done := make(chan struct{}), make(chan error, 1)
	swapped := make(chan struct{})
	go func() {
		first := true
		for {
			for _, target := range []string{outside, "inside"} {
				select {
				case <-stop:
					done <- nil
					return
				default:
				}
				if err := os.Symlink(target, filepath.Join(home, "next")); err != nil {
					done <- err
					return
				}
				if err := os.Rename(filepath.Join(home, "next"), filepath.Join(home, "swap")); err != nil {
					done <- err
					return
				}
				if first {
					close(swapped)
					first = false
				}
			}
		}
	}()
	select {
	case <-swapped:
	case err := <-done:
		t.Fatal(err)
	}
	for i := 0; i < 200; i++ {
		control.replies.Reset()
		c.handleMKD("swap/new/subdir")
	}
	close(stop)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(outside, "new")); !os.IsNotExist(err) {
		t.Fatalf("external directory created during link replacement: %v", err)
	}
}

func TestSecurityInvalidPORTPreservesPassiveListener(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	c, control := securitySession(t, t.TempDir())
	c.pasv, c.listener = true, listener
	c.handlePORT("127,0,0,1,200,256")
	if !strings.HasPrefix(control.replies.String(), "501 ") || c.listener != listener || !c.pasv {
		t.Fatal("invalid PORT replaced passive state")
	}
	listener.(*net.TCPListener).SetDeadline(time.Now().Add(time.Millisecond))
	_, err = listener.Accept()
	if timeout, ok := err.(net.Error); !ok || !timeout.Timeout() {
		t.Fatalf("invalid PORT closed passive listener: %v", err)
	}
}

func TestSecurityPORTBoundaryPorts(t *testing.T) {
	for _, port := range []int{1024, 65535} {
		c, control := securitySession(t, t.TempDir())
		c.handlePORT(fmt.Sprintf("127,0,0,1,%d,%d", port/256, port%256))
		if !strings.HasPrefix(control.replies.String(), "200 ") || c.dataPort != port {
			t.Fatalf("valid boundary port %d rejected: %s", port, control.replies.String())
		}
	}
}

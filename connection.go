package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// FTPConn represents an FTP client connection
type FTPConn struct {
	// mu protects resources shared by the command loop and server shutdown.
	mu             sync.Mutex
	closed         bool
	ctx            context.Context
	cancel         context.CancelFunc
	root           *os.Root
	conn           net.Conn
	username       string
	running        bool
	homeDir        string
	currDir        string
	identified     bool
	pasv           bool
	dataConn       net.Conn
	dataHost       string
	dataPort       int
	listener       net.Listener
	config         Config
	renameTempPath string
}

// NewFTPConn creates a new FTP connection handler
func NewFTPConn(conn net.Conn, config Config) *FTPConn {
	ctx, cancel := context.WithCancel(context.Background())
	return &FTPConn{
		ctx:        ctx,
		cancel:     cancel,
		conn:       conn,
		config:     config,
		running:    true,
		identified: false,
		currDir:    "/",
	}
}

// Close closes the FTP connection
func (c *FTPConn) Close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	if c.cancel != nil {
		c.cancel()
	}
	data, listener, root := c.dataConn, c.listener, c.root
	c.dataConn, c.listener, c.root = nil, nil, nil
	c.mu.Unlock()
	if c.conn != nil {
		c.conn.Close()
	}
	if listener != nil {
		listener.Close()
	}
	if data != nil {
		data.Close()
	}
	if root != nil {
		root.Close()
	}
}

// filesystem returns the login's pinned root, never an absolute client path.
func (c *FTPConn) filesystem() (*os.Root, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.root == nil {
		return nil, errors.New("filesystem unavailable")
	}
	return c.root, nil
}

func (c *FTPConn) resetLogin() {
	c.identified = false
	c.username, c.homeDir, c.currDir, c.renameTempPath = "", "", "/", ""
	c.mu.Lock()
	root, listener, data := c.root, c.listener, c.dataConn
	c.root, c.listener, c.dataConn = nil, nil, nil
	c.pasv, c.dataHost, c.dataPort = false, "", 0
	c.mu.Unlock()
	if root != nil {
		root.Close()
	}
	if listener != nil {
		listener.Close()
	}
	if data != nil {
		data.Close()
	}
}

func (c *FTPConn) login(home string) error {
	root, err := os.OpenRoot(home)
	if err != nil {
		return err
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		root.Close()
		return net.ErrClosed
	}
	old := c.root
	c.root = root
	c.mu.Unlock()
	if old != nil {
		old.Close()
	}
	c.homeDir, c.currDir, c.renameTempPath = home, "/", ""
	c.identified = true
	return nil
}

// Handle handles the FTP connection
func (c *FTPConn) Handle() {
	defer c.Close()
	c.sendWelcome()

	scanner := bufio.NewScanner(c.conn)
	for c.running && scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		cmd, arg := c.parseCommand(line)
		if handler, ok := commands[cmd]; ok {
			handler(c, arg)
		} else {
			c.sendMsg(500, "Command not found")
		}
	}

	if err := scanner.Err(); err != nil {
		log.Printf("Scanner error: %v", err)
	}

	c.sendMsg(221, "Goodbye")
	log.Printf("Connection closed for user %s", c.username)
}

func (c *FTPConn) parseCommand(line string) (string, string) {
	parts := strings.SplitN(line, " ", 2)
	cmd := strings.ToUpper(parts[0])
	var arg string
	if len(parts) > 1 {
		arg = parts[1]
	}
	return cmd, arg
}

// parsePath maps slash-separated FTP names to paths relative to os.Root.
// Virtual /.. stays at /; native separators, drive/stream syntax and NUL are rejected.
// This normalization is not the security boundary: every file operation uses Root.
func (c *FTPConn) parsePath(remotePath string) (remote, local string, err error) {
	if strings.ContainsAny(remotePath, "\\:\x00") {
		return "", "", errors.New("invalid FTP path")
	}
	if !path.IsAbs(remotePath) {
		remotePath = path.Join("/", c.currDir, remotePath)
	}
	remote = path.Clean(remotePath)
	if remote == "/" {
		return remote, ".", nil
	}
	local, err = filepath.Localize(strings.TrimPrefix(remote, "/"))
	if err != nil {
		return "", "", fmt.Errorf("invalid FTP path: %w", err)
	}
	return remote, local, nil
}

func (c *FTPConn) sendWelcome() {
	c.sendMsg(220, "Welcome to FTP.go Server")
}

func (c *FTPConn) sendMsg(code int, msg string) {
	fullMsg := fmt.Sprintf("%d %s\r\n", code, msg)
	_, err := c.conn.Write([]byte(fullMsg))
	if err != nil {
		log.Printf("Error sending message: %v", err)
	}
}

// validateActiveTarget is checked both at PORT and immediately before dialing.
func (c *FTPConn) validateActiveTarget(host string, port int) error {
	peer, ok := c.conn.RemoteAddr().(*net.TCPAddr)
	ip := net.ParseIP(host)
	if !ok || ip == nil || !ip.Equal(peer.IP) {
		return errors.New("data target must match control peer")
	}
	if port < 1024 || port > 65535 {
		return errors.New("data port must be between 1024 and 65535")
	}
	return nil
}

func (c *FTPConn) establishDataConn() (net.Conn, error) {
	c.closeDataConn()
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, net.ErrClosed
	}
	passive, listener, host, port, ctx := c.pasv, c.listener, c.dataHost, c.dataPort, c.ctx
	c.mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	var conn net.Conn
	var err error
	if passive {
		if listener == nil {
			return nil, errors.New("PASV required first")
		}
		for {
			conn, err = listener.Accept()
			if err != nil {
				return nil, fmt.Errorf("passive mode accept failed: %w", err)
			}
			peer, peerOK := c.conn.RemoteAddr().(*net.TCPAddr)
			dataPeer, dataOK := conn.RemoteAddr().(*net.TCPAddr)
			if peerOK && dataOK && peer.IP.Equal(dataPeer.IP) {
				break
			}
			conn.Close()
		}
	} else {
		if err := c.validateActiveTarget(host, port); err != nil {
			return nil, err
		}
		dialer := net.Dialer{Timeout: 10 * time.Second}
		conn, err = dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
		if err != nil {
			return nil, fmt.Errorf("active mode dial failed: %w", err)
		}
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		conn.Close()
		return nil, net.ErrClosed
	}
	c.dataConn = conn
	c.mu.Unlock()
	return conn, nil
}

func (c *FTPConn) closeDataConn() {
	c.mu.Lock()
	conn := c.dataConn
	c.dataConn = nil
	c.mu.Unlock()
	if conn != nil {
		conn.Close()
	}
}

// Command handlers

func (c *FTPConn) handleUSER(arg string) {
	c.resetLogin()
	userConfig, ok := c.config[arg]
	if !ok {
		c.sendMsg(550, "Invalid user")
		c.running = false
		return
	}

	c.username = arg
	if arg == "anonymous" {
		if err := c.login(userConfig.HomeDir); err != nil {
			c.sendMsg(530, "Home directory unavailable")
			return
		}
		c.sendMsg(230, "Login successful")
	} else {
		c.sendMsg(331, "Password required")
	}
}

func (c *FTPConn) handlePASS(arg string) {
	if c.username == "" {
		c.sendMsg(503, "Login with USER first")
		return
	}

	userConfig, ok := c.config[c.username]
	if !ok {
		c.sendMsg(530, "Login incorrect")
		c.running = false
		return
	}

	if arg != userConfig.Password {
		c.sendMsg(530, "Login incorrect")
		c.running = false
		return
	}

	if err := c.login(userConfig.HomeDir); err != nil {
		c.sendMsg(530, "Home directory unavailable")
		return
	}
	c.sendMsg(230, "Login successful")
}

func (c *FTPConn) handleCWD(arg string) {
	if !c.identified {
		c.sendMsg(530, "Not logged in")
		return
	}

	root, err := c.filesystem()
	if err != nil {
		c.sendMsg(550, "Filesystem unavailable")
		return
	}

	remote, local, err := c.parsePath(arg)
	if err != nil {
		c.sendMsg(550, fmt.Sprintf("Failed to change directory: %v", err))
		return
	}

	info, err := root.Stat(local)
	if err != nil {
		c.sendMsg(550, fmt.Sprintf("Directory not found: %v", err))
		return
	}

	if !info.IsDir() {
		c.sendMsg(550, "Not a directory")
		return
	}

	c.currDir = remote
	c.sendMsg(250, "Directory changed")
}

func (c *FTPConn) handleCDUP(arg string) {
	c.handleCWD("..")
}

func (c *FTPConn) handlePWD(arg string) {
	if !c.identified {
		c.sendMsg(530, "Not logged in")
		return
	}
	c.sendMsg(257, fmt.Sprintf("\"%s\"", c.currDir))
}

func (c *FTPConn) handleLIST(arg string) {
	if !c.identified {
		c.sendMsg(530, "Not logged in")
		return
	}

	root, err := c.filesystem()
	if err != nil {
		c.sendMsg(550, "Filesystem unavailable")
		return
	}
	data, err := c.establishDataConn()
	if err != nil {
		c.sendMsg(425, fmt.Sprintf("Cannot open data connection: %v", err))
		return
	}
	defer c.closeDataConn()

	_, local, err := c.parsePath(c.currDir)
	if err != nil {
		c.sendMsg(550, fmt.Sprintf("Failed to list directory: %v", err))
		return
	}

	dir, err := root.Open(local)
	if err != nil {
		c.sendMsg(550, "Cannot open directory")
		return
	}
	defer dir.Close()
	entries, err := dir.ReadDir(-1)
	if err != nil {
		c.sendMsg(550, fmt.Sprintf("Failed to read directory: %v", err))
		return
	}

	c.sendMsg(125, "Data connection already open; transfer starting")

	for _, entry := range entries {
		info, err := root.Lstat(filepath.Join(local, entry.Name()))
		if err != nil {
			continue
		}
		line := c.formatListLine(entry.Name(), info)
		if _, err := io.WriteString(data, line); err != nil {
			c.sendMsg(451, "Transfer failed")
			return
		}
	}

	c.sendMsg(226, "Transfer complete")
}

func (c *FTPConn) formatListLine(name string, info os.FileInfo) string {
	var typeChar string
	if info.IsDir() {
		typeChar = "d"
	} else {
		typeChar = "-"
	}

	var timeStr string
	if time.Since(info.ModTime()).Hours() > 180*24 { // 6 months
		timeStr = info.ModTime().Format("Jan 02  2006")
	} else {
		timeStr = info.ModTime().Format("Jan 02 15:04")
	}

	return fmt.Sprintf("%srw-r--r--   1 user  group  %12d %s %s\r\n",
		typeChar, info.Size(), timeStr, name)
}

func (c *FTPConn) handleRETR(arg string) {
	if !c.identified {
		c.sendMsg(530, "Not logged in")
		return
	}

	root, err := c.filesystem()
	if err != nil {
		c.sendMsg(550, "Filesystem unavailable")
		return
	}

	_, local, err := c.parsePath(arg)
	if err != nil {
		c.sendMsg(550, fmt.Sprintf("Access denied: %v", err))
		return
	}

	data, err := c.establishDataConn()
	if err != nil {
		c.sendMsg(425, fmt.Sprintf("Cannot open data connection: %v", err))
		return
	}
	defer c.closeDataConn()

	file, err := root.Open(local)
	if err != nil {
		c.sendMsg(550, fmt.Sprintf("Cannot open file: %v", err))
		return
	}
	defer file.Close()

	c.sendMsg(125, "Data connection already open; transfer starting")

	_, err = io.Copy(data, file)
	if err != nil {
		c.sendMsg(451, "Transfer failed")
		return
	}

	c.sendMsg(226, "Transfer complete")
}

func (c *FTPConn) handleSTOR(arg string) {
	if !c.identified {
		c.sendMsg(530, "Not logged in")
		return
	}

	root, err := c.filesystem()
	if err != nil {
		c.sendMsg(550, "Filesystem unavailable")
		return
	}

	_, local, err := c.parsePath(arg)
	if err != nil {
		c.sendMsg(550, fmt.Sprintf("Access denied: %v", err))
		return
	}

	data, err := c.establishDataConn()
	if err != nil {
		c.sendMsg(425, fmt.Sprintf("Cannot open data connection: %v", err))
		return
	}
	defer c.closeDataConn()

	file, err := root.Create(local)
	if err != nil {
		c.sendMsg(550, fmt.Sprintf("Cannot create file: %v", err))
		return
	}
	defer file.Close()

	c.sendMsg(125, "Data connection already open; transfer starting")

	_, err = io.Copy(file, data)
	if err != nil {
		c.sendMsg(451, "Transfer failed")
		return
	}

	c.sendMsg(226, "Transfer complete")
}

func (c *FTPConn) handleMKD(arg string) {
	if !c.identified {
		c.sendMsg(530, "Not logged in")
		return
	}

	root, err := c.filesystem()
	if err != nil {
		c.sendMsg(550, "Filesystem unavailable")
		return
	}

	remote, local, err := c.parsePath(arg)
	if err != nil {
		c.sendMsg(550, fmt.Sprintf("Access denied: %v", err))
		return
	}

	if err := root.MkdirAll(local, 0755); err != nil {
		c.sendMsg(550, fmt.Sprintf("Cannot create directory: %v", err))
		return
	}

	c.sendMsg(257, fmt.Sprintf("\"%s\" directory created", remote))
}

func (c *FTPConn) handleRMD(arg string) {
	if !c.identified {
		c.sendMsg(530, "Not logged in")
		return
	}

	root, err := c.filesystem()
	if err != nil {
		c.sendMsg(550, "Filesystem unavailable")
		return
	}

	_, local, err := c.parsePath(arg)
	if err != nil {
		c.sendMsg(550, fmt.Sprintf("Access denied: %v", err))
		return
	}

	if err := root.Remove(local); err != nil {
		c.sendMsg(550, fmt.Sprintf("Cannot remove directory: %v", err))
		return
	}

	c.sendMsg(250, "Directory removed")
}

func (c *FTPConn) handleDELE(arg string) {
	if !c.identified {
		c.sendMsg(530, "Not logged in")
		return
	}

	root, err := c.filesystem()
	if err != nil {
		c.sendMsg(550, "Filesystem unavailable")
		return
	}

	_, local, err := c.parsePath(arg)
	if err != nil {
		c.sendMsg(550, fmt.Sprintf("Access denied: %v", err))
		return
	}

	if err := root.Remove(local); err != nil {
		c.sendMsg(550, fmt.Sprintf("Cannot delete file: %v", err))
		return
	}

	c.sendMsg(250, "File deleted")
}

func (c *FTPConn) handleRNFR(arg string) {
	if !c.identified {
		c.sendMsg(530, "Not logged in")
		return
	}

	root, err := c.filesystem()
	if err != nil {
		c.sendMsg(550, "Filesystem unavailable")
		return
	}

	remote, local, err := c.parsePath(arg)
	if err != nil {
		c.sendMsg(550, fmt.Sprintf("Access denied: %v", err))
		return
	}

	if _, err := root.Lstat(local); err != nil {
		c.sendMsg(550, "Rename source unavailable")
		return
	}

	c.renameTempPath = local
	c.sendMsg(350, fmt.Sprintf("Ready for destination name for %s", remote))
}

func (c *FTPConn) handleRNTO(arg string) {
	if !c.identified {
		c.sendMsg(530, "Not logged in")
		return
	}

	if c.renameTempPath == "" {
		c.sendMsg(503, "RNFR command required first")
		return
	}

	root, err := c.filesystem()
	if err != nil {
		c.sendMsg(550, "Filesystem unavailable")
		return
	}

	remote, local, err := c.parsePath(arg)
	if err != nil {
		c.sendMsg(550, fmt.Sprintf("Access denied: %v", err))
		return
	}

	source := c.renameTempPath
	c.renameTempPath = ""
	if err := root.Rename(source, local); err != nil {
		c.sendMsg(550, fmt.Sprintf("Cannot rename: %v", err))
		return
	}

	c.renameTempPath = ""
	c.sendMsg(250, fmt.Sprintf("Renamed to %s", remote))
}

func (c *FTPConn) handlePORT(arg string) {
	if !c.identified {
		c.sendMsg(530, "Not logged in")
		return
	}

	params := strings.Split(arg, ",")
	if len(params) != 6 {
		c.sendMsg(501, "Invalid PORT parameter")
		return
	}
	var octets [6]byte
	for i, field := range params {
		if field == "" {
			c.sendMsg(501, "Invalid PORT parameter")
			return
		}
		for _, ch := range field {
			if ch < '0' || ch > '9' {
				c.sendMsg(501, "Invalid PORT parameter")
				return
			}
		}
		value, err := strconv.ParseUint(field, 10, 8)
		if err != nil {
			c.sendMsg(501, "Invalid PORT parameter")
			return
		}
		octets[i] = byte(value)
	}
	host := net.IPv4(octets[0], octets[1], octets[2], octets[3]).String()
	port := int(octets[4])*256 + int(octets[5])
	if err := c.validateActiveTarget(host, port); err != nil {
		c.sendMsg(501, err.Error())
		return
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	oldListener := c.listener
	c.listener = nil
	c.dataHost, c.dataPort, c.pasv = host, port, false
	c.mu.Unlock()
	if oldListener != nil {
		oldListener.Close()
	}
	c.sendMsg(200, "PORT command successful")
}

func (c *FTPConn) handlePASV(arg string) {
	if !c.identified {
		c.sendMsg(530, "Not logged in")
		return
	}

	// Advertise the control connection's local IPv4 address when available.
	var ip string
	if addr, ok := c.conn.LocalAddr().(*net.TCPAddr); ok && addr.IP.To4() != nil {
		ip = addr.IP.To4().String()
	} else {
		var err error
		ip, err = getExternalIP()
		if err != nil {
			c.sendMsg(421, "Cannot get local IPv4 address")
			return
		}
	}
	listener, err := net.Listen("tcp4", net.JoinHostPort(ip, "0"))
	if err != nil {
		c.sendMsg(421, fmt.Sprintf("Cannot create passive listener: %v", err))
		return
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		listener.Close()
		return
	}
	oldListener := c.listener
	c.listener, c.pasv = listener, true
	c.mu.Unlock()
	if oldListener != nil {
		oldListener.Close()
	}
	port := listener.Addr().(*net.TCPAddr).Port
	ipStr := strings.ReplaceAll(ip, ".", ",")
	c.sendMsg(227, fmt.Sprintf("Entering Passive Mode (%s,%d,%d)", ipStr, port>>8&0xFF, port&0xFF))
}

func (c *FTPConn) handleTYPE(arg string) {
	if !c.identified {
		c.sendMsg(530, "Not logged in")
		return
	}
	c.sendMsg(200, "Type set to ASCII")
}

func (c *FTPConn) handleSYST(arg string) {
	c.sendMsg(215, "UNIX Type: L8")
}

func (c *FTPConn) handleQUIT(arg string) {
	c.sendMsg(221, "Goodbye")
	c.running = false
}

// Command map
var commands = map[string]func(*FTPConn, string){
	"USER": (*FTPConn).handleUSER,
	"PASS": (*FTPConn).handlePASS,
	"CWD":  (*FTPConn).handleCWD,
	"XCWD": (*FTPConn).handleCWD,
	"CDUP": (*FTPConn).handleCDUP,
	"PWD":  (*FTPConn).handlePWD,
	"XPWD": (*FTPConn).handlePWD,
	"LIST": (*FTPConn).handleLIST,
	"NLST": (*FTPConn).handleLIST,
	"RETR": (*FTPConn).handleRETR,
	"STOR": (*FTPConn).handleSTOR,
	"MKD":  (*FTPConn).handleMKD,
	"XMKD": (*FTPConn).handleMKD,
	"RMD":  (*FTPConn).handleRMD,
	"XRMD": (*FTPConn).handleRMD,
	"DELE": (*FTPConn).handleDELE,
	"RNFR": (*FTPConn).handleRNFR,
	"RNTO": (*FTPConn).handleRNTO,
	"PORT": (*FTPConn).handlePORT,
	"PASV": (*FTPConn).handlePASV,
	"TYPE": (*FTPConn).handleTYPE,
	"SYST": (*FTPConn).handleSYST,
	"QUIT": (*FTPConn).handleQUIT,
}

// getExternalIP returns the first non-loopback IPv4 address
func getExternalIP() (string, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return "", err
	}

	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}

		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}

		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}

			if ip == nil || ip.IsLoopback() {
				continue
			}

			ip = ip.To4()
			if ip == nil {
				continue
			}

			return ip.String(), nil
		}
	}

	return "", errors.New("no network connection found")
}

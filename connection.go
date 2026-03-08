package main

import (
	"bufio"
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
	"time"
)

// FTPConn represents an FTP client connection
type FTPConn struct {
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
	return &FTPConn{
		conn:       conn,
		config:     config,
		running:    true,
		identified: false,
		currDir:    "/",
	}
}

// Close closes the FTP connection
func (c *FTPConn) Close() {
	if c.conn != nil {
		c.conn.Close()
	}
	c.closeDataConn()
	if c.listener != nil {
		c.listener.Close()
	}
}

// Handle handles the FTP connection
func (c *FTPConn) Handle() {
	c.sendWelcome()

	scanner := bufio.NewScanner(c.conn)
	for scanner.Scan() && c.running {
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
	c.Close()
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

// parsePath converts a remote path to a local path with security checks
// Returns error if path escapes the home directory
func (c *FTPConn) parsePath(remotePath string) (remote, local string, err error) {
	if remotePath == "" {
		remotePath = "."
	}

	// Handle relative vs absolute paths
	if !strings.HasPrefix(remotePath, "/") {
		remotePath = path.Join(c.currDir, remotePath)
	}

	// Clean the path
	remotePath = path.Clean(remotePath)

	// Construct local path
	localPath := filepath.Join(c.homeDir, remotePath)

	// Security check: ensure path is within homeDir
	absHome, err := filepath.Abs(c.homeDir)
	if err != nil {
		return "", "", fmt.Errorf("failed to resolve home directory: %w", err)
	}

	absLocal, err := filepath.Abs(localPath)
	if err != nil {
		return "", "", fmt.Errorf("failed to resolve path: %w", err)
	}

	// Check for path traversal attempt
	if !strings.HasPrefix(absLocal, absHome) {
		return "", "", errors.New("access denied: path traversal attempt")
	}

	if remotePath == "." {
		remotePath = "/"
	}

	return remotePath, absLocal, nil
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

func (c *FTPConn) establishDataConn() error {
	c.closeDataConn()

	var conn net.Conn
	var err error

	if c.pasv {
		conn, err = c.listener.Accept()
		if err != nil {
			return fmt.Errorf("passive mode accept failed: %w", err)
		}
	} else {
		raddr := fmt.Sprintf("%s:%d", c.dataHost, c.dataPort)
		conn, err = net.Dial("tcp", raddr)
		if err != nil {
			return fmt.Errorf("active mode dial failed: %w", err)
		}
	}

	c.dataConn = conn
	return nil
}

func (c *FTPConn) closeDataConn() {
	if c.dataConn != nil {
		c.dataConn.Close()
		c.dataConn = nil
	}
}

// Command handlers

func (c *FTPConn) handleUSER(arg string) {
	userConfig, ok := c.config[arg]
	if !ok {
		c.sendMsg(550, "Invalid user")
		c.running = false
		return
	}

	c.username = arg
	if arg == "anonymous" {
		c.identified = true
		c.homeDir = userConfig.HomeDir
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

	c.homeDir = userConfig.HomeDir
	c.identified = true
	c.sendMsg(230, "Login successful")
}

func (c *FTPConn) handleCWD(arg string) {
	if !c.identified {
		c.sendMsg(530, "Not logged in")
		return
	}

	remote, local, err := c.parsePath(arg)
	if err != nil {
		c.sendMsg(550, fmt.Sprintf("Failed to change directory: %v", err))
		return
	}

	info, err := os.Stat(local)
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

	if err := c.establishDataConn(); err != nil {
		c.sendMsg(425, fmt.Sprintf("Cannot open data connection: %v", err))
		return
	}
	defer c.closeDataConn()

	_, local, err := c.parsePath(c.currDir)
	if err != nil {
		c.sendMsg(550, fmt.Sprintf("Failed to list directory: %v", err))
		return
	}

	// Use os.ReadDir (replaces deprecated ioutil.ReadDir)
	entries, err := os.ReadDir(local)
	if err != nil {
		c.sendMsg(550, fmt.Sprintf("Failed to read directory: %v", err))
		return
	}

	c.sendMsg(125, "Data connection already open; transfer starting")

	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			continue
		}
		line := c.formatListLine(entry.Name(), info)
		io.WriteString(c.dataConn, line)
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

	_, local, err := c.parsePath(arg)
	if err != nil {
		c.sendMsg(550, fmt.Sprintf("Access denied: %v", err))
		return
	}

	if err := c.establishDataConn(); err != nil {
		c.sendMsg(425, fmt.Sprintf("Cannot open data connection: %v", err))
		return
	}
	defer c.closeDataConn()

	file, err := os.Open(local)
	if err != nil {
		c.sendMsg(550, fmt.Sprintf("Cannot open file: %v", err))
		return
	}
	defer file.Close()

	c.sendMsg(125, "Data connection already open; transfer starting")

	_, err = io.Copy(c.dataConn, file)
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

	_, local, err := c.parsePath(arg)
	if err != nil {
		c.sendMsg(550, fmt.Sprintf("Access denied: %v", err))
		return
	}

	if err := c.establishDataConn(); err != nil {
		c.sendMsg(425, fmt.Sprintf("Cannot open data connection: %v", err))
		return
	}
	defer c.closeDataConn()

	file, err := os.Create(local)
	if err != nil {
		c.sendMsg(550, fmt.Sprintf("Cannot create file: %v", err))
		return
	}
	defer file.Close()

	c.sendMsg(125, "Data connection already open; transfer starting")

	_, err = io.Copy(file, c.dataConn)
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

	remote, local, err := c.parsePath(arg)
	if err != nil {
		c.sendMsg(550, fmt.Sprintf("Access denied: %v", err))
		return
	}

	if err := os.MkdirAll(local, 0755); err != nil {
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

	_, local, err := c.parsePath(arg)
	if err != nil {
		c.sendMsg(550, fmt.Sprintf("Access denied: %v", err))
		return
	}

	if err := os.Remove(local); err != nil {
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

	_, local, err := c.parsePath(arg)
	if err != nil {
		c.sendMsg(550, fmt.Sprintf("Access denied: %v", err))
		return
	}

	if err := os.Remove(local); err != nil {
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

	remote, local, err := c.parsePath(arg)
	if err != nil {
		c.sendMsg(550, fmt.Sprintf("Access denied: %v", err))
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

	remote, local, err := c.parsePath(arg)
	if err != nil {
		c.sendMsg(550, fmt.Sprintf("Access denied: %v", err))
		return
	}

	if err := os.Rename(c.renameTempPath, local); err != nil {
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

	c.dataHost = strings.Join(params[0:4], ".")

	p1, err := strconv.Atoi(params[4])
	if err != nil {
		c.sendMsg(501, "Invalid port parameter")
		return
	}

	p2, err := strconv.Atoi(params[5])
	if err != nil {
		c.sendMsg(501, "Invalid port parameter")
		return
	}

	c.dataPort = p1*256 + p2
	c.pasv = false
	c.sendMsg(200, "PORT command successful")
}

func (c *FTPConn) handlePASV(arg string) {
	if !c.identified {
		c.sendMsg(530, "Not logged in")
		return
	}

	if c.listener != nil {
		c.listener.Close()
	}

	ip, err := getExternalIP()
	if err != nil {
		c.sendMsg(421, fmt.Sprintf("Cannot get external IP: %v", err))
		return
	}

	laddr := fmt.Sprintf("%s:0", ip)
	listener, err := net.Listen("tcp", laddr)
	if err != nil {
		c.sendMsg(421, fmt.Sprintf("Cannot create passive listener: %v", err))
		return
	}

	c.listener = listener
	port := listener.Addr().(*net.TCPAddr).Port
	ipStr := strings.ReplaceAll(ip, ".", ",")

	c.sendMsg(227, fmt.Sprintf("Entering Passive Mode (%s,%d,%d)", ipStr, port>>8&0xFF, port&0xFF))
	c.pasv = true
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

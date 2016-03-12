package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type userConfig struct {
	Password string `json:"password"`
	HomeDir  string `json:"homeDir"`
}

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
	listener       net.Listener // For Pasv mode
	commands       map[string]func(string)
	config         map[string]userConfig
	renameTempPath string
}

var p = flag.Int("p", 9021, "listen port")

func main() {
	flag.Parse()
	config, err := parseConfig()
	if err != nil {
		log.Println(err)
		os.Exit(1)
	}
	laddr := fmt.Sprintf(":%d", *p)
	listener, err := net.Listen("tcp", laddr)
	if err != nil {
		log.Fatal(err)
	}
	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Print(err)
			continue
		}
		log.Printf("New Connection from %s\n", conn.RemoteAddr())
		go handleConn(conn, config)
	}
	listener.Close()
}

func handleConn(conn net.Conn, config map[string]userConfig) {
	handler := NewFTPConn(conn, config)
	handler.start()
}

func parseConfig() (map[string]userConfig, error) {
	file, err := os.Open("config.json")
	if err != nil {
		log.Println(err)
		return nil, err
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	config := make(map[string]userConfig)
	decoder.Decode(&config)
	return config, nil
}

func NewFTPConn(conn net.Conn, config map[string]userConfig) *FTPConn {
	ftpConn := &FTPConn{conn: conn, config: config, running: true, identified: false}
	ftpConn.buildCommandMap()
	dir, err := os.Getwd()
	if err != nil {
		dir = "."
	}
	ftpConn.homeDir = dir
	ftpConn.currDir = "/"
	return ftpConn
}

func (ftpConn *FTPConn) start() error {
	ftpConn.buildCommandMap()
	ftpConn.sayWelcome()
	input := bufio.NewScanner(ftpConn.conn)
	for input.Scan() {
		command, arg := "", ""
		data := strings.Split(input.Text(), " ")
		length := len(data)
		if len(data) == 0 {
			ftpConn.running = false
			break
		} else if length == 1 {
			command = data[0]
		} else {
			command = data[0]
			arg = data[1]
		}
		methond, ok := ftpConn.commands[strings.ToUpper(command)]
		if !ok {
			ftpConn.sendMsg(500, "Command Not Found")
			continue
		}
		methond(arg)
		if !ftpConn.running {
			break
		}
	}
	ftpConn.sayBye()
	ftpConn.closeDataConn()
	ftpConn.conn.Close()
	log.Println("FTP connnection closed.")
	return nil
}

func (ftpConn *FTPConn) sayWelcome() {
	ftpConn.sendMsg(220, "Welcome to Ftp.go FTP")
}

func (ftpConn *FTPConn) sayBye() {
	ftpConn.sendMsg(200, "OK")
}

func (ftpConn *FTPConn) sendMsg(code int, msg string) error {
	_, err := fmt.Fprintf(ftpConn.conn, "%d %s\r\n", code, msg)
	if err != nil {
		log.Println(err)
	}
	return err
}

func (ftpConn *FTPConn) parsePath(arg string) (remote, local string) {
	if arg == "" {
		arg = "."
	}
	if arg[0] != '/' {
		arg = path.Join(ftpConn.currDir, arg)
	}
	arg = filepath.Clean(arg)
	local = path.Join(ftpConn.homeDir, arg)
	remote = arg
	if remote == "." {
		remote = "/"
	}
	return
}

func (ftpConn *FTPConn) dataConnConn() (err error) {
	ftpConn.closeDataConn()
	var conn net.Conn
	if ftpConn.pasv {
		conn, err = ftpConn.listener.Accept()
		if err != nil {
			return
		}
	} else {
		raddr := fmt.Sprintf("%s:%d", ftpConn.dataHost, ftpConn.dataPort)
		conn, err = net.Dial("tcp", raddr)
		if err != nil {
			return
		}
	}
	ftpConn.dataConn = conn
	return
}

func (ftpConn *FTPConn) closeDataConn() {
	if ftpConn.dataConn != nil {
		ftpConn.dataConn.Close()
		ftpConn.dataConn = nil
	}
}

func (ftpConn *FTPConn) handleSecurity(f func(string)) func(string) {
	return func(arg string) {
		if ftpConn.identified {
			f(arg)
		} else {
			ftpConn.sendMsg(530, "Not logged in")
		}
	}

}

func (ftpConn *FTPConn) handleUSER(arg string) {
	if userConfig, ok := ftpConn.config[arg]; ok {
		ftpConn.username = arg
		if arg == "anonymous" {
			ftpConn.identified = true
			ftpConn.homeDir = userConfig.HomeDir
			ftpConn.sendMsg(230, "OK")
		} else {
			ftpConn.sendMsg(331, "Need password")
		}
	} else {
		ftpConn.sendMsg(550, "Invalid User")
		ftpConn.running = false
	}
}

func (ftpConn *FTPConn) handlePASS(arg string) {
	if arg == ftpConn.config[ftpConn.username].Password {
		ftpConn.homeDir = ftpConn.config[ftpConn.username].HomeDir
		ftpConn.identified = true
		ftpConn.sendMsg(230, "OK")
	} else {
		ftpConn.sendMsg(530, "Password is not corrected")
		ftpConn.running = false
	}
}

func (ftpConn *FTPConn) handleCWD(arg string) {
	remote, local := ftpConn.parsePath(arg)
	info, err := os.Stat(local)
	if err != nil {
		log.Println(err)
		ftpConn.sendMsg(500, "Change directory failed!")
		return
	}
	if info.IsDir() {
		ftpConn.currDir = remote
		ftpConn.sendMsg(250, "Working directory changed")
	} else {
		ftpConn.sendMsg(500, "Change directory failed!")
	}
}

func (ftpConn *FTPConn) handleXCWD(arg string) {
	ftpConn.handleCWD(arg)
}

func (ftpConn *FTPConn) handleCDUP(arg string) {
	ftpConn.handleCWD("..")
}

func (ftpConn *FTPConn) handleRNFR(arg string) {
	remote, local := ftpConn.parsePath(arg)
	ftpConn.renameTempPath = local
	ftpConn.sendMsg(350, "rename from "+remote)
}

func (ftpConn *FTPConn) handleRNTO(arg string) {
	remote, local := ftpConn.parsePath(arg)
	os.Rename(ftpConn.renameTempPath, local)
	ftpConn.sendMsg(250, "rename to "+remote)
}

func (ftpConn *FTPConn) handleDELE(arg string) {
	_, local := ftpConn.parsePath(arg)
	if _, err := os.Stat(local); os.IsNotExist(err) {
		ftpConn.sendMsg(450, "File does not exist")
	} else {
		os.Remove(local)
		ftpConn.sendMsg(250, "File deleted")
	}
}

func (ftpConn *FTPConn) handleRMD(arg string) {
	_, local := ftpConn.parsePath(arg)
	if _, err := os.Stat(local); os.IsNotExist(err) {
		ftpConn.sendMsg(500, "Directory does not exist")
	} else {
		os.Remove(local)
		ftpConn.sendMsg(250, "OK")
	}
}

func (ftpConn *FTPConn) handleMKD(arg string) {
	_, local := ftpConn.parsePath(arg)
	if _, err := os.Stat(local); os.IsNotExist(err) {
		os.MkdirAll(local, os.ModePerm)
		ftpConn.sendMsg(257, fmt.Sprintf("\"%s\" directory created", arg))
	} else {
		ftpConn.sendMsg(500, "Folder is already existed")
	}
}

func (ftpConn *FTPConn) handleXMKD(arg string) {
	ftpConn.handleMKD(arg)
}

func (ftpConn *FTPConn) handlePWD(arg string) {
	remote, _ := ftpConn.parsePath(arg)
	ftpConn.sendMsg(257, fmt.Sprintf("\"%s\"", remote))
}

func (ftpConn *FTPConn) handleLIST(arg string) {
	if err := ftpConn.dataConnConn(); err != nil {
		ftpConn.handleErr(err, 500, "List directory failed")
		return
	}
	defer ftpConn.closeDataConn()

	ftpConn.sendMsg(125, "OK")
	format := "%s%s%s------  %d %s  %s  %d  %s %s\r\n"
	_, local := ftpConn.parsePath(ftpConn.currDir)
	infos, err := ioutil.ReadDir(local)
	if err != nil {
		ftpConn.handleErr(err, 500, "List directory failed")
		return
	}
	for _, info := range infos {
		var f string = "-"
		var modTime string
		if time.Now().Sub(info.ModTime()).Hours() > 6*30*24 {
			modTime = info.ModTime().Format("JAN 02 2006")
		} else {
			modTime = info.ModTime().Format("01 02 15:04")
		}

		if info.IsDir() {
			f = "d"
		}
		msg := fmt.Sprintf(format,
			f, "r", "w", 1, "0", "0",
			info.Size(), modTime, info.Name())
		io.WriteString(ftpConn.dataConn, msg)
	}
	ftpConn.sendMsg(226, "Limit")
}

func (ftpConn *FTPConn) handleQUIT(arg string) {
	ftpConn.closeDataConn()
	ftpConn.running = false
	ftpConn.username = ""
	ftpConn.identified = false
	ftpConn.sendMsg(221, "OK")
}

func (ftpConn *FTPConn) handlePORT(arg string) {
	ftpConn.closeDataConn()
	params := strings.Split(arg, ",")
	if len(params) != 6 {
		ftpConn.sendMsg(501, "Parameter error")
	} else {
		ftpConn.dataHost = strings.Join(params[:4], ".")
		p1, err := strconv.Atoi(params[4])
		if err != nil {
			ftpConn.handleErr(err, 501, "Parameter error")
			return
		}
		p2, err := strconv.Atoi(params[5])
		if err != nil {
			ftpConn.sendMsg(501, "Parameter error")
			return
		}
		ftpConn.dataPort = p1*256 + p2
		ftpConn.sendMsg(200, "OK")
		ftpConn.pasv = false
	}
}

func (ftpConn *FTPConn) handlePASV(arg string) {
	if ftpConn.listener != nil {
		ftpConn.listener.Close()
		ftpConn.listener = nil
	}
	ip, err := externalIP()
	if err != nil {
		ftpConn.handleErr(err, 500, "passive mode error")
		return
	}
	laddr := fmt.Sprintf("%s:0", ip)
	l, err := net.Listen("tcp", laddr)
	if err != nil {
		ftpConn.handleErr(err, 500, "passive mode error")
		return
	}
	ftpConn.listener = l
	h := strings.Replace(ip, ".", ",", -1)
	port := l.Addr().(*net.TCPAddr).Port
	ftpConn.sendMsg(227, fmt.Sprintf("Entering Passive Mode (%s,%d,%d)", h, port>>8&0xFF, port&0xFF))
	ftpConn.pasv = true
}

func (ftpConn *FTPConn) handleTYPE(arg string) {
	ftpConn.sendMsg(220, "OK")
}

func (ftpConn *FTPConn) handleRETR(arg string) {
	if err := ftpConn.dataConnConn(); err != nil {
		ftpConn.handleErr(err, 425, "Can't open data connection")
		return
	}
	defer ftpConn.closeDataConn()
	ftpConn.sendMsg(125, "OK")
	_, local := ftpConn.parsePath(arg)

	f, err := os.Open(local)
	if err != nil {
		ftpConn.sendMsg(450, "Can't open file")
		return
	}
	defer f.Close()
	_, err = io.Copy(ftpConn.dataConn, f)
	if err != nil {
		ftpConn.sendMsg(451, "Transfer file failed")
		return
	}

	ftpConn.sendMsg(226, "OK")
}

func (ftpConn *FTPConn) handleSTOR(arg string) {
	if err := ftpConn.dataConnConn(); err != nil {
		ftpConn.handleErr(err, 425, "Can't open data connection")
		return
	}
	defer ftpConn.closeDataConn()
	ftpConn.sendMsg(125, "OK")

	_, local := ftpConn.parsePath(arg)

	f, err := os.Create(local)
	if err != nil {
		ftpConn.sendMsg(450, "Can't create file")
		return
	}
	defer f.Close()
	_, err = io.Copy(f, ftpConn.dataConn)
	if err != nil {
		ftpConn.sendMsg(451, "Store file failed")
		return
	}
	ftpConn.sendMsg(226, "OK")
}

func (ftpConn *FTPConn) handleSYST(arg string) {
	ftpConn.sendMsg(215, "UNIX")
}

func (ftpConn *FTPConn) handleErr(err error, code int, msg string) {
	log.Println(err)
	ftpConn.sendMsg(code, msg)
}

func (ftpConn *FTPConn) buildCommandMap() {
	ftpConn.commands = make(map[string]func(string))
	commands := ftpConn.commands
	commands["USER"] = ftpConn.handleUSER
	commands["PASS"] = ftpConn.handlePASS
	commands["CWD"] = ftpConn.handleSecurity(ftpConn.handleCWD)
	commands["XCWD"] = ftpConn.handleSecurity(ftpConn.handleXCWD)
	commands["CDUP"] = ftpConn.handleSecurity(ftpConn.handleCDUP)
	commands["RNFR"] = ftpConn.handleSecurity(ftpConn.handleRNFR)
	commands["RNTO"] = ftpConn.handleSecurity(ftpConn.handleRNTO)
	commands["DELE"] = ftpConn.handleSecurity(ftpConn.handleDELE)
	commands["RMD"] = ftpConn.handleSecurity(ftpConn.handleRMD)
	commands["MKD"] = ftpConn.handleSecurity(ftpConn.handleMKD)
	commands["XMKD"] = ftpConn.handleSecurity(ftpConn.handleXMKD)
	commands["PWD"] = ftpConn.handleSecurity(ftpConn.handlePWD)
	commands["QUIT"] = ftpConn.handleSecurity(ftpConn.handleQUIT)
	commands["PORT"] = ftpConn.handleSecurity(ftpConn.handlePORT)
	commands["PASV"] = ftpConn.handleSecurity(ftpConn.handlePASV)
	commands["TYPE"] = ftpConn.handleSecurity(ftpConn.handleTYPE)
	commands["RETR"] = ftpConn.handleSecurity(ftpConn.handleRETR)
	commands["STOR"] = ftpConn.handleSecurity(ftpConn.handleSTOR)
	commands["SYST"] = ftpConn.handleSecurity(ftpConn.handleSYST)
	commands["LIST"] = ftpConn.handleSecurity(ftpConn.handleLIST)
}

// http://play.golang.org/p/BDt3qEQ_2H
func externalIP() (string, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return "", err
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 {
			continue // interface down
		}
		if iface.Flags&net.FlagLoopback != 0 {
			continue // loopback interface
		}
		addrs, err := iface.Addrs()
		if err != nil {
			return "", err
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
				continue // not an ipv4 address
			}
			return ip.String(), nil
		}
	}
	return "", errors.New("are you connected to the network?")
}

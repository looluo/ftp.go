# ftp.go

A simple FTP server implementation in Go.

## Features

- **User Authentication**: Support for multiple users with configurable credentials
- **Anonymous Login**: Built-in support for anonymous FTP access
- **Passive & Active Mode**: Supports both PASV and PORT data connection modes
- **Security**: Path traversal protection to prevent unauthorized file access
- **Graceful Shutdown**: Proper signal handling for clean server shutdown
- **Core FTP Commands**:
  - `USER` / `PASS` - Authentication
  - `CWD` / `XCWD` / `CDUP` - Directory navigation
  - `PWD` / `XPWD` - Print working directory
  - `LIST` / `NLST` - List directory contents
  - `RETR` - Download files
  - `STOR` - Upload files
  - `MKD` / `XMKD` - Create directories
  - `RMD` / `XRMD` - Remove directories
  - `DELE` - Delete files
  - `RNFR` / `RNTO` - Rename files/directories
  - `QUIT` - Disconnect
  - `TYPE` - Set transfer type
  - `SYST` - System information

## Requirements

- Go 1.25 or later (uses `os.Root`, including `MkdirAll` and `Rename`)

## Configuration

Create a `config.json` file in the working directory:

```json
{
    "username": {
        "password": "your_password",
        "homeDir": "/path/to/home"
    },
    "anonymous": {
        "password": "",
        "homeDir": "/path/to/anonymous/root"
    }
}
```

## Usage

```bash
# Build
go build -o ftp-server .

# Run with default port (9021)
./ftp-server

# Run with custom port
./ftp-server -p 2121

# Run with custom config file
./ftp-server -c /path/to/config.json
```

### Command Line Options

| Flag | Default | Description |
|------|---------|-------------|
| `-p` | 9021 | FTP server listen port |
| `-c` | config.json | Path to configuration file |

## Testing

### Unit Tests

```bash
go test ./... -v
go test -race ./...
```

### Integration Testing

Connect using any FTP client:

```bash
ftp localhost 9021
```

Or use `lftp`, `FileZilla`, etc.

## Architecture

The server is organized into the following components:

- `main.go` - Entry point and CLI handling
- `server.go` - Server lifecycle and connection management
- `connection.go` - FTP protocol implementation
- `config.go` - Configuration loading and validation

## Security Notes

- Passwords are transmitted in plaintext (FTP protocol limitation). Use FTPS/FTPS for secure transmission.
- Each successful login opens an `os.Root` handle. All FTP filesystem commands operate relative to that handle, including directory listing metadata and both rename paths. This prevents traversal through external symlinks, including symlink replacement races, on supported native platforms.
- FTP paths use `/`. Parent components are normalized within the virtual root; backslashes, colons (including Windows drive/alternate-stream syntax), and NUL are rejected. Relative symlinks that stay inside the root work; absolute symlinks are rejected, including absolute links back into the root.
- `PORT` requires six decimal octets in 0–255, the control peer's IP, and a port in 1024–65535. Invalid requests preserve the previous data endpoint. The target is checked again before dialing, and active dials are canceled during shutdown. Active FTP clients behind NAT must advertise the IP visible to the server or use passive mode.
- Passive data connections must also originate from the control peer's IP. Shutdown closes control/data sockets and passive listeners and waits for all connection handlers.
- The configured home directory is trusted when opened at login. `os.Root` is not an OS sandbox: it does not exclude existing hard links, mount points, or device files inside the tree. `GOOS=js` deployments do not provide the same symlink-race guarantee. See [Go's `os.Root` documentation](https://pkg.go.dev/os#Root).
- Configuration validation ensures home directories exist before starting.

## License

Apache License 2.0

## Author

[looluo](https://github.com/looluo)

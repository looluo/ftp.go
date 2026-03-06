# ftp.go

A simple FTP server implementation in Go.

## Features

- **User Authentication**: Support for multiple users with configurable credentials
- **Anonymous Login**: Built-in support for anonymous FTP access
- **Passive & Active Mode**: Supports both PASV and PORT data connection modes
- **Core FTP Commands**:
  - `USER` / `PASS` - Authentication
  - `CWD` / `XCWD` / `CDUP` - Directory navigation
  - `PWD` - Print working directory
  - `LIST` - List directory contents
  - `RETR` - Download files
  - `STOR` - Upload files
  - `MKD` / `XMKD` - Create directories
  - `RMD` - Remove directories
  - `DELE` - Delete files
  - `RNFR` / `RNTO` - Rename files/directories
  - `QUIT` - Disconnect
  - `TYPE` - Set transfer type
  - `SYST` - System information

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
go build -o ftp-server main.go

# Run with default port (9021)
./ftp-server

# Run with custom port
./ftp-server -p 2121
```

## Testing

Connect using any FTP client:

```bash
ftp localhost 9021
```

Or use `lftp`, `FileZilla`, etc.

## License

Apache License 2.0

## Author

[looluo](https://github.com/looluo)

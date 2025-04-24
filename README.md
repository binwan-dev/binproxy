# binproxy

[English] | [中文](docs/README_zh.md)

## Introduction

`binproxy` is a simple client-server TCP proxy tool written in Go. It allows you to expose a TCP service running on a local machine (potentially behind NAT) to the internet via a publicly accessible server.

The client connects to the server's control port, authenticates, and then the server manages forwarding traffic between external connections on configured ports and the client, which in turn proxies the traffic to the specified local targets.

## Features

*   Simple configuration via YAML files.
*   Client authentication using name/key pairs.
*   Multiplexing multiple proxied connections over a single control channel.
*   Cross-platform compilation support (Linux, Windows, macOS).

## Prerequisites

*   Go 1.18 or later installed.

## Building

1.  **Clone the repository (if applicable)**
2.  **Initialize Go Modules (if not already done):**
    ```bash
    go mod init binproxy
    go mod tidy
    ```
3.  **Build Server:**
    *   For Linux (amd64):
        ```bash
        GOOS=linux GOARCH=amd64 go build -o dist/server/binproxy-server ./server
        ```
    *   For Windows (amd64):
        ```bash
        GOOS=windows GOARCH=amd64 go build -o dist/client/binproxy-server.exe ./server
        ```
    *   For macOS (amd64):
        ```bash
        GOOS=darwin GOARCH=amd64 go build -o dist/server/binproxy-server ./server
        ```
4.  **Build Client:**
    *   For Linux (amd64):
        ```bash
        GOOS=linux GOARCH=amd64 go build -o dist/client/binproxy-client ./client
        ```
    *   For Windows (amd64):
        ```bash
        GOOS=windows GOARCH=amd64 go build -o dist/client/binproxy-client.exe ./client
        ```
    *   For macOS (amd64):
        ```bash
        GOOS=darwin GOARCH=amd64 go build -o dist/client/binproxy-client ./client
        ```
5.  **Copy Configuration Files:** Manually copy the respective `config.yaml` files into the `dist/server` and `dist/client` directories after building.
    ```bash
    cp server/config.yaml dist/server/
    cp client/config.yaml dist/client/
    ```

The `dist` directory will contain the ready-to-deploy binaries and their configuration files.

## Configuration

Configuration is done using YAML files.

### Server (`dist/server/config.yaml`)

```yaml
control_port: 8001        # Port the server listens on for client control connections
client_configs:
  - client_name: "client_A" # Unique name for the client
    client_key: "secret_key_A" # Secret key for authentication
    proxies:                 # List of proxy rules for this client
      - server_port: 8000    # External port the server will listen on
        client_ip: "192.168.1.100" # Local IP the client should forward traffic to
        client_port: 9000    # Local port the client should forward traffic to
      - server_port: 8002
        client_ip: "127.0.0.1"
        client_port: 9001
  - client_name: "client_B"
    client_key: "secret_key_B"
    proxies:
      - server_port: 8080
        client_ip: "10.0.0.50"
        client_port: 9090
```

*   `control_port`: The TCP port the server uses to listen for incoming client connections for authentication and control messages.
*   `client_configs`: An array defining allowed clients.
    *   `client_name`: A unique identifier for the client.
    *   `client_key`: A pre-shared secret key used for authenticating the client.
    *   `proxies`: An array of proxy rules associated with this client.
        *   `server_port`: The public-facing TCP port the server will listen on for external connections related to this rule. Must be unique across *all* clients.
        *   `client_ip`: The target IP address on the client's local network where the traffic should be forwarded.
        *   `client_port`: The target TCP port on the client's local network.

### Client (`dist/client/config.yaml`)

```yaml
server_addr: "your_server_ip_or_domain" # IP address or hostname of the server
control_port: 8001                      # Must match the server's control_port
client_name: "client_A"                 # Must match a client_name in the server config
client_key: "secret_key_A"              # Must match the client_key for the client_name
```

*   `server_addr`: The address where the `binproxy-server` is running.
*   `control_port`: The control port the server is listening on (must match server config).
*   `client_name`: The name this client will use to authenticate with the server.
*   `client_key`: The secret key for authentication.

## Running

### Server

1.  Place the compiled `binproxy-server` binary and the `config.yaml` file in the same directory on your server.
2.  Navigate to that directory in your terminal.
3.  Run the server: `./binproxy-server` (Linux/macOS) or `. inproxy-server.exe` (Windows).
4.  The server will log that it's listening on the control port and the configured data ports.

### Client

1.  Place the compiled `binproxy-client` binary and the `config.yaml` file in the same directory on the machine whose local services you want to expose.
2.  Edit `config.yaml` with the correct server address and client credentials.
3.  Navigate to that directory in your terminal.
4.  Run the client: `./binproxy-client` (Linux/macOS) or `. inproxy-client.exe` (Windows).
5.  The client will attempt to connect to the server, authenticate, and then wait for instructions. If the connection drops, it will attempt to reconnect.

## How it Works (Briefly)

1.  The **Client** connects to the **Server**'s control port.
2.  The **Client** sends an `AuthRequest` with its name and key.
3.  The **Server** validates the credentials against its `config.yaml`. If valid, it sends back an `AuthResponse` containing the list of proxy rules assigned to that client.
4.  An **External User** connects to one of the `server_port`s defined in the server's config.
5.  The **Server** accepts this connection, generates a unique Connection ID (`ConnID`), and sends a `MsgNewConnection` (containing `ConnID` and the `server_port`) to the corresponding authenticated **Client** via the control channel.
6.  The **Client** receives `MsgNewConnection`. It looks up the local target IP/Port based on the `server_port` from its stored rules. It dials the local target.
7.  If the local connection is successful, the **Client** sends a `MsgConnectionReady` (containing `ConnID`) back to the **Server**. It then starts a goroutine to read from the local connection and forward data back to the server (as `MsgData`).
8.  If the local connection fails, the **Client** sends `MsgCloseConnection` to the **Server**.
9.  Upon receiving `MsgConnectionReady`, the **Server** starts a goroutine to read from the external connection and forwards data to the **Client** (as `MsgData` containing `ConnID` and the data chunk) via the control channel.
10. When the **Client** receives `MsgData`, it finds the corresponding local connection using `ConnID` and writes the data to it.
11. When the **Server** receives `MsgData` from the client, it finds the corresponding external connection using `ConnID` and writes the data to it.
12. If either the external connection or the local connection is closed (or encounters an error), the corresponding side sends `MsgCloseConnection` (with `ConnID`) to the other side via the control channel, allowing both sides to clean up the specific proxied connection resources.

## License

MIT License 
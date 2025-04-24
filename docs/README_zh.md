[English](../README.md) | [中文]

## 简介

`binproxy` 是一个使用 Go 编写的简单的客户端-服务器 TCP 代理工具。它允许您将运行在本地机器（可能位于 NAT 之后）上的 TCP 服务通过一个具有公网 IP 的服务器暴露给互联网。

客户端连接到服务器的控制端口，进行身份验证，然后服务器管理在配置的端口上的外部连接与客户端之间的流量转发，客户端再将流量代理到指定的本地目标。

## 特性

*   通过 YAML 文件进行简单配置。
*   使用名称/密钥对进行客户端身份验证。
*   通过单个控制通道复用多个代理连接。
*   支持跨平台编译（Linux、Windows、macOS）。

## 先决条件

*   已安装 Go 1.18 或更高版本。

## 构建

1.  **克隆仓库（如果适用）**
2.  **初始化 Go 模块（如果尚未完成）：**
    ```bash
    go mod init binproxy
    go mod tidy
    ```
3.  **构建服务端：**
    *   适用于 Linux (amd64):
        ```bash
        GOOS=linux GOARCH=amd64 go build -o dist/server/binproxy-server ./server
        ```
    *   适用于 Windows (amd64):
        ```bash
        GOOS=windows GOARCH=amd64 go build -o dist/client/binproxy-server.exe ./server
        ```
    *   适用于 macOS (amd64):
        ```bash
        GOOS=darwin GOARCH=amd64 go build -o dist/server/binproxy-server ./server
        ```
4.  **构建客户端：**
    *   适用于 Linux (amd64):
        ```bash
        GOOS=linux GOARCH=amd64 go build -o dist/client/binproxy-client ./client
        ```
    *   适用于 Windows (amd64):
        ```bash
        GOOS=windows GOARCH=amd64 go build -o dist/client/binproxy-client.exe ./client
        ```
    *   适用于 macOS (amd64):
        ```bash
        GOOS=darwin GOARCH=amd64 go build -o dist/client/binproxy-client ./client
        ```
5.  **复制配置文件：** 构建后，手动将相应的 `config.yaml` 文件复制到 `dist/server` 和 `dist/client` 目录中。
    ```bash
    cp server/config.yaml dist/server/
    cp client/config.yaml dist/client/
    ```

`dist` 目录将包含准备部署的二进制文件及其配置文件。

## 配置

使用 YAML 文件进行配置。

### 服务端 (`dist/server/config.yaml`)

```yaml
control_port: 8001        # 服务器监听客户端控制连接的端口
client_configs:
  - client_name: "client_A" # 客户端的唯一名称
    client_key: "secret_key_A" # 用于身份验证的密钥
    proxies:                 # 与此客户端关联的代理规则列表
      - server_port: 8000    # 服务器将监听的外部端口
        client_ip: "192.168.1.100" # 客户端应将流量转发到的本地 IP
        client_port: 9000    # 客户端应将流量转发到的本地端口
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

*   `control_port`: 服务器用于监听客户端连接以进行身份验证和控制消息的 TCP 端口。
*   `client_configs`: 定义允许的客户端的数组。
    *   `client_name`: 客户端的唯一标识符。
    *   `client_key`: 用于验证客户端身份的预共享密钥。
    *   `proxies`: 与此客户端关联的代理规则数组。
        *   `server_port`: 服务器将为此规则监听的面向公众的 TCP 端口。在*所有*客户端中必须唯一。
        *   `client_ip`: 流量应转发到的客户端本地网络上的目标 IP 地址。
        *   `client_port`: 客户端本地网络上的目标 TCP 端口。

### 客户端 (`dist/client/config.yaml`)

```yaml
server_addr: "your_server_ip_or_domain" # 服务器的 IP 地址或主机名
control_port: 8001                      # 必须与服务器的 control_port 匹配
client_name: "client_A"                 # 必须与服务器配置中的 client_name 匹配
client_key: "secret_key_A"              # 必须与 client_name 对应的 client_key 匹配
```

*   `server_addr`: `binproxy-server` 运行的地址。
*   `control_port`: 服务器正在监听的控制端口（必须与服务器配置匹配）。
*   `client_name`: 此客户端将用于向服务器进行身份验证的名称。
*   `client_key`: 用于身份验证的密钥。

## 运行

### 服务端

1.  将编译好的 `binproxy-server` 二进制文件和 `config.yaml` 文件放在服务器上的同一目录中。
2.  在终端中导航到该目录。
3.  运行服务器：`./binproxy-server` (Linux/macOS) 或 `. inproxy-server.exe` (Windows)。
4.  服务器将记录它正在监听控制端口和配置的数据端口。

### 客户端

1.  将编译好的 `binproxy-client` 二进制文件和 `config.yaml` 文件放在您想要暴露其本地服务的机器上的同一目录中。
2.  编辑 `config.yaml`，填入正确的服务器地址和客户端凭据。
3.  在终端中导航到该目录。
4.  运行客户端：`./binproxy-client` (Linux/macOS) 或 `. inproxy-client.exe` (Windows)。
5.  客户端将尝试连接到服务器，进行身份验证，然后等待指令。如果连接断开，它将尝试重新连接。

## 工作原理 (简述)

1.  **客户端** 连接到 **服务器** 的控制端口。
2.  **客户端** 发送包含其名称和密钥的 `AuthRequest`。
3.  **服务器** 根据其 `config.yaml` 验证凭据。如果有效，则发送回一个 `AuthResponse`，其中包含分配给该客户端的代理规则列表。
4.  **外部用户** 连接到服务器配置中定义的 `server_port` 之一。
5.  **服务器** 接受此连接，生成唯一的连接 ID (`ConnID`)，并通过控制通道向相应的已认证 **客户端** 发送 `MsgNewConnection`（包含 `ConnID` 和 `server_port`）。
6.  **客户端** 收到 `MsgNewConnection`。它根据 `server_port` 从其存储的规则中查找本地目标 IP/端口。它连接（Dial）本地目标。
7.  如果本地连接成功，**客户端** 向 **服务器** 发送回 `MsgConnectionReady`（包含 `ConnID`）。然后它启动一个 goroutine 从本地连接读取数据并将数据（作为 `MsgData`）转发回服务器。
8.  如果本地连接失败，**客户端** 向 **服务器** 发送 `MsgCloseConnection`。
9.  收到 `MsgConnectionReady` 后，**服务器** 启动一个 goroutine 从外部连接读取数据，并通过控制通道将数据（作为包含 `ConnID` 和数据块的 `MsgData`）转发给 **客户端**。
10. 当 **客户端** 收到 `MsgData` 时，它使用 `ConnID` 查找相应的本地连接，并将数据写入该连接。
11. 当 **服务器** 从客户端收到 `MsgData` 时，它使用 `ConnID` 查找相应的外部连接，并将数据写入该连接。
12. 如果外部连接或本地连接任一端关闭（或遇到错误），相应的一方会通过控制通道向另一方发送 `MsgCloseConnection`（带有 `ConnID`），允许双方清理特定的代理连接资源。

## 许可证

MIT 许可证

## 声明

该项目仅为技术研究，请遵守相关法律法规，请勿用于非法用途。 
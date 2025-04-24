package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"sync"
	"time"

	"binproxy/protocol"

	"gopkg.in/yaml.v3"
)

// ClientConfig holds the client configuration
type ClientConfig struct {
	ServerAddr  string `yaml:"server_addr"`
	ControlPort int    `yaml:"control_port"`
	ClientName  string `yaml:"client_name"`
	ClientKey   string `yaml:"client_key"`
}

// Global map to store active local connections managed by this client
var localConnections = make(map[uint64]net.Conn)
var localConnectionsMutex sync.Mutex

// Global map to store proxy rules received from the server (serverPort -> ClientProxy)
var proxyRules = make(map[int]protocol.ClientProxy)
var proxyRulesMutex sync.RWMutex

func main() {
	cfg, err := loadConfig("config.yaml")
	if err != nil {
		log.Fatalf("Failed to load client config: %v", err)
	}

	serverControlAddr := fmt.Sprintf("%s:%d", cfg.ServerAddr, cfg.ControlPort)

	for {
		log.Printf("Attempting to connect to server control port: %s", serverControlAddr)
		controlConn, err := net.Dial("tcp", serverControlAddr)
		if err != nil {
			log.Printf("Failed to connect to server: %v. Retrying in 5 seconds...", err)
			time.Sleep(5 * time.Second)
			continue
		}

		log.Printf("Connected to server control port.")
		err = handleControlConnection(controlConn, cfg)
		if err != nil {
			log.Printf("Connection handler error: %v", err)
		}
		// Ensure all resources are cleaned up before retrying
		cleanupLocalConnections()
		controlConn.Close()
		log.Printf("Disconnected from server. Retrying in 5 seconds...")
		time.Sleep(5 * time.Second)
	}
}

func loadConfig(path string) (*ClientConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading client config file %s: %w", path, err)
	}

	var cfg ClientConfig
	err = yaml.Unmarshal(data, &cfg)
	if err != nil {
		return nil, fmt.Errorf("unmarshalling client config: %w", err)
	}

	if cfg.ServerAddr == "" || cfg.ControlPort == 0 || cfg.ClientName == "" || cfg.ClientKey == "" {
		return nil, fmt.Errorf("invalid client configuration: server_addr, control_port, client_name, and client_key must be set")
	}

	return &cfg, nil
}

// handleControlConnection manages the authenticated control connection.
func handleControlConnection(controlConn net.Conn, cfg *ClientConfig) error {
	// 1. Send Authentication Request
	authReq := protocol.AuthRequest{
		ClientName: cfg.ClientName,
		ClientKey:  cfg.ClientKey,
	}
	authBytes, err := json.Marshal(authReq)
	if err != nil {
		return fmt.Errorf("failed to marshal auth request: %w", err)
	}

	_, err = controlConn.Write(append(authBytes, '\n'))
	if err != nil {
		return fmt.Errorf("failed to send auth request: %w", err)
	}
	log.Printf("Sent authentication request for client '%s'", cfg.ClientName)

	// 2. Wait for Authentication Response
	reader := bufio.NewReader(controlConn)
	scanner := bufio.NewScanner(reader)

	if !scanner.Scan() {
		if scanner.Err() != nil {
			return fmt.Errorf("error reading auth response line: %w", scanner.Err())
		}
		return fmt.Errorf("connection closed before receiving auth response")
	}

	var authControlMsg protocol.ControlMessage
	err = json.Unmarshal(scanner.Bytes(), &authControlMsg)
	if err != nil {
		return fmt.Errorf("failed to unmarshal auth response wrapper: %w", err)
	}

	if authControlMsg.Type != protocol.MsgAuthResponse {
		return fmt.Errorf("expected MsgAuthResponse, but got: %s", authControlMsg.Type)
	}

	var authRespPayload protocol.AuthResponsePayload
	err = json.Unmarshal(authControlMsg.Payload, &authRespPayload)
	if err != nil {
		return fmt.Errorf("failed to unmarshal auth response payload: %w", err)
	}

	if authRespPayload.Status != "ok" {
		return fmt.Errorf("server authentication failed: %s", authRespPayload.Message)
	}

	log.Printf("Authentication successful. Storing proxy rules...")
	updateProxyRules(authRespPayload.Proxies)

	// 3. Process Subsequent Messages from Server
	log.Printf("Waiting for server messages...")
	for scanner.Scan() {
		var msg protocol.ControlMessage
		err := json.Unmarshal(scanner.Bytes(), &msg)
		if err != nil {
			log.Printf("Error unmarshalling control message: %v", err)
			continue
		}

		ss, _ := json.Marshal(msg)
		fmt.Println("receive msg from server: ", string(ss))

		switch msg.Type {
		case protocol.MsgNewConnection:
			var payload protocol.NewConnectionPayload
			if err := json.Unmarshal(msg.Payload, &payload); err != nil {
				log.Printf("Error unmarshalling NewConnection payload: %v", err)
				continue
			}
			go handleNewConnectionRequest(controlConn, payload)

		case protocol.MsgData:
			var payload protocol.DataPayload
			if err := json.Unmarshal(msg.Payload, &payload); err != nil {
				log.Printf("Error unmarshalling Data payload: %v", err)
				continue
			}
			handleDataFromServer(payload)

		case protocol.MsgCloseConnection:
			var payload protocol.CloseConnectionPayload
			if err := json.Unmarshal(msg.Payload, &payload); err != nil {
				log.Printf("Error unmarshalling CloseConnection payload: %v", err)
				continue
			}
			handleCloseConnectionFromServer(payload)

		default:
			log.Printf("Received unknown message type: %s", msg.Type)
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("error reading from server: %w", err)
	}

	return fmt.Errorf("server connection closed unexpectedly")
}

// updateProxyRules stores the proxy rules received from the server.
func updateProxyRules(proxies []protocol.ClientProxy) {
	proxyRulesMutex.Lock()
	defer proxyRulesMutex.Unlock()
	proxyRules = make(map[int]protocol.ClientProxy)
	for _, p := range proxies {
		proxyRules[p.ServerPort] = p
		log.Printf("  - Rule: ServerPort %d -> Local %s:%d", p.ServerPort, p.ClientIP, p.ClientPort)
	}
}

// handleNewConnectionRequest handles the MsgNewConnection from the server.
func handleNewConnectionRequest(controlConn net.Conn, payload protocol.NewConnectionPayload) {
	connID := payload.ConnID
	serverPort := payload.ServerPort
	log.Printf("[Conn %d] Received NewConnection request for server port %d", connID, serverPort)

	proxyRulesMutex.RLock()
	rule, ok := proxyRules[serverPort]
	proxyRulesMutex.RUnlock()

	if !ok {
		log.Printf("[Conn %d] No proxy rule found for server port %d. Sending close.", connID, serverPort)
		sendCloseToServer(controlConn, connID)
		return
	}

	targetAddr := fmt.Sprintf("%s:%d", rule.ClientIP, rule.ClientPort)
	log.Printf("[Conn %d] Dialing local target: %s", connID, targetAddr)

	localConn, err := net.DialTimeout("tcp", targetAddr, 10*time.Second)
	if err != nil {
		log.Printf("[Conn %d] Failed to dial local target %s: %v. Sending close.", connID, targetAddr, err)
		sendCloseToServer(controlConn, connID)
		return
	}

	log.Printf("[Conn %d] Successfully connected to local target %s", connID, targetAddr)

	localConnectionsMutex.Lock()
	localConnections[connID] = localConn
	localConnectionsMutex.Unlock()

	// Start goroutine to forward data from local target to server
	go forwardLocalToControl(controlConn, localConn, connID)

	// **** ADDED: Send ConnectionReady message back to server ****
	readyPayload := protocol.ConnectionReadyPayload{ConnID: connID}
	msgBytes, err := protocol.NewControlMessage(protocol.MsgConnectionReady, readyPayload)
	if err != nil {
		log.Printf("[Conn %d] Failed to create ConnectionReady message: %v", connID, err)
		// Close local connection if we can't notify server
		closeLocalConnection(connID)
		sendCloseToServer(controlConn, connID) // Also notify server about closing
		return
	}
	_, err = controlConn.Write(msgBytes)
	if err != nil {
		log.Printf("[Conn %d] Failed to send ConnectionReady message to server: %v", connID, err)
		// Close local connection if we can't notify server
		closeLocalConnection(connID)
		// No need to send close again, Write error implies control conn issue
		return
	}
	log.Printf("[Conn %d] Sent ConnectionReady to server.", connID)
}

// handleDataFromServer handles MsgData received from the server.
func handleDataFromServer(payload protocol.DataPayload) {
	connID := payload.ConnID
	data := payload.Data

	localConnectionsMutex.Lock()
	localConn, ok := localConnections[connID]
	localConnectionsMutex.Unlock()

	if !ok {
		// Connection might have been closed already, ignore data
		// log.Printf("[Conn %d] Received data for non-existent local connection. Ignoring %d bytes.", connID, len(data))
		return
	}

	// log.Printf("[Conn %d] Received %d bytes from server, writing to local connection %s", connID, len(data), localConn.RemoteAddr())
	n, err := localConn.Write(data)
	if err != nil {
		log.Printf("[Conn %d] Error writing %d bytes to local connection %s: %v. Closing connection.", connID, len(data), localConn.RemoteAddr(), err)
		closeLocalConnection(connID) // Close and remove from map
		return
	}
	if n != len(data) {
		log.Printf("[Conn %d] Short write to local connection %s: %d/%d bytes.", connID, localConn.RemoteAddr(), n, len(data))
	}
}

// handleCloseConnectionFromServer handles MsgCloseConnection received from the server.
func handleCloseConnectionFromServer(payload protocol.CloseConnectionPayload) {
	connID := payload.ConnID
	log.Printf("[Conn %d] Received close request from server.", connID)
	closeLocalConnection(connID)
}

// forwardLocalToControl reads from the local connection and sends data to the server.
func forwardLocalToControl(controlConn net.Conn, localConn net.Conn, connID uint64) {
	defer func() {
		log.Printf("[Conn %d] Stopped forwarding local -> control for %s.", connID, localConn.RemoteAddr())
		closeLocalConnection(connID) // Ensure cleanup if loop exits
	}()

	buf := make([]byte, 32*1024)
	for {
		n, err := localConn.Read(buf)
		if err != nil {
			if err != io.EOF {
				log.Printf("[Conn %d] Error reading from local connection %s: %v", connID, localConn.RemoteAddr(), err)
			}
			// Send close message to server upon read error/EOF
			sendCloseToServer(controlConn, connID)
			return // Exit goroutine
		}

		if n > 0 {
			dataToSend := make([]byte, n)
			copy(dataToSend, buf[:n])
			payload := protocol.DataPayload{ConnID: connID, Data: dataToSend}
			msgBytes, err := protocol.NewControlMessage(protocol.MsgData, payload)
			fmt.Println("send result payload: ", string(msgBytes))
			if err != nil {
				log.Printf("[Conn %d] Failed to create Data message for server: %v", connID, err)
				return // Exit if we can't marshal
			}

			// log.Printf("[Conn %d] Sending %d bytes to server from local connection %s", connID, len(dataToSend), localConn.RemoteAddr())
			_, err = controlConn.Write(msgBytes)
			if err != nil {
				log.Printf("[Conn %d] Failed to send Data message to server: %v", connID, err)
				return // Exit if control connection write fails
			}
		}
	}
}

// sendCloseToServer sends a MsgCloseConnection message to the server.
func sendCloseToServer(controlConn net.Conn, connID uint64) {
	payload := protocol.CloseConnectionPayload{ConnID: connID}
	msgBytes, err := protocol.NewControlMessage(protocol.MsgCloseConnection, payload)
	if err != nil {
		log.Printf("[Conn %d] Failed to create CloseConnection message for server: %v", connID, err)
		return
	}
	_, err = controlConn.Write(msgBytes)
	if err != nil {
		// Don't log if the error is about writing to a closed pipe (server might have disconnected)
		if !isUseOfClosedNetworkConnection(err) {
			log.Printf("[Conn %d] Failed to send CloseConnection message to server: %v", connID, err)
		}
	}
}

// closeLocalConnection closes and removes a local connection from the map.
func closeLocalConnection(connID uint64) {
	localConnectionsMutex.Lock()
	defer localConnectionsMutex.Unlock()

	if localConn, ok := localConnections[connID]; ok {
		log.Printf("[Conn %d] Closing local connection to %s", connID, localConn.RemoteAddr())
		localConn.Close()
		delete(localConnections, connID)
	}
}

// cleanupLocalConnections closes all active local connections, typically when the control connection is lost.
func cleanupLocalConnections() {
	localConnectionsMutex.Lock()
	defer localConnectionsMutex.Unlock()

	if len(localConnections) > 0 {
		log.Printf("Cleaning up %d active local connection(s)...", len(localConnections))
		for connID, localConn := range localConnections {
			log.Printf("[Conn %d] Force closing local connection to %s due to control link down.", connID, localConn.RemoteAddr())
			localConn.Close()
			delete(localConnections, connID)
		}
	}
}

// isUseOfClosedNetworkConnection checks if an error is the specific "use of closed network connection".
func isUseOfClosedNetworkConnection(err error) bool {
	if opErr, ok := err.(*net.OpError); ok {
		return opErr.Err.Error() == "use of closed network connection"
	}
	return false
}

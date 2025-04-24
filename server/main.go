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
	"sync/atomic"
	"time"

	"binproxy/protocol"

	"gopkg.in/yaml.v3"
)

// ClientConfig holds configuration for a specific client.
type ClientConfig struct {
	ClientName string                 `yaml:"client_name"`
	ClientKey  string                 `yaml:"client_key"`
	Proxies    []protocol.ClientProxy `yaml:"proxies"`
}

// Config holds the main application configuration.
type Config struct {
	ControlPort   int            `yaml:"control_port"`
	ClientConfigs []ClientConfig `yaml:"client_configs"`
}

// ActiveProxies stores the target address for each active server port (for logging/info).
var activeProxies = make(map[int]string)
var activeProxiesMutex sync.RWMutex

// serverPortToClientName maps a server port to the client responsible for it.
var serverPortToClientName = make(map[int]string)
var serverPortToClientNameMutex sync.RWMutex

// authenticatedClients stores authenticated control connections.
var authenticatedClients = make(map[string]net.Conn)
var authenticatedClientsMutex sync.Mutex

// activeExternalConns stores active external connections proxied through control channels.
// Key: ConnID, Value: the external connection. Access needs mutex.
var activeExternalConns = make(map[uint64]net.Conn)
var activeExternalConnsMutex sync.Mutex

// activeExternalConnStatus tracks the status of external connections before data forwarding starts.
// Key: ConnID, Value: chan struct{} (closed when ready or failed), or could use an enum/bool.
// Access needs mutex.
var activeExternalConnStatus = make(map[uint64]chan struct{}) // Channel used as a signal
var activeExternalConnStatusMutex sync.Mutex

// connIDCounter provides unique IDs for proxied connections.
var connIDCounter atomic.Uint64

func main() {
	cfg, err := loadConfig("config.yaml")
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// Initialize mappings and start data listeners
	activeProxiesMutex.Lock()
	serverPortToClientNameMutex.Lock()
	for _, clientCfg := range cfg.ClientConfigs {
		for _, proxy := range clientCfg.Proxies {
			if _, exists := activeProxies[proxy.ServerPort]; exists {
				log.Printf("Config Error: Server port %d conflict detected during listener setup.", proxy.ServerPort)
				continue
			}
			targetAddr := fmt.Sprintf("%s:%d", proxy.ClientIP, proxy.ClientPort)
			activeProxies[proxy.ServerPort] = targetAddr
			serverPortToClientName[proxy.ServerPort] = clientCfg.ClientName
			go startDataListener(proxy.ServerPort)
		}
	}
	serverPortToClientNameMutex.Unlock()
	activeProxiesMutex.Unlock()

	// Start Control Listener
	controlAddr := fmt.Sprintf(":%d", cfg.ControlPort)
	startControlListener(controlAddr, cfg)
}

func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file %s: %w", path, err)
	}

	var cfg Config
	err = yaml.Unmarshal(data, &cfg)
	if err != nil {
		return nil, fmt.Errorf("unmarshalling config: %w", err)
	}

	if cfg.ControlPort == 0 {
		return nil, fmt.Errorf("invalid configuration: control_port must be set")
	}

	usedServerPorts := make(map[int]string)

	for _, clientCfg := range cfg.ClientConfigs {
		if clientCfg.ClientName == "" || clientCfg.ClientKey == "" {
			return nil, fmt.Errorf("invalid configuration: client_name and client_key must be set for a client config")
		}
		for j, proxy := range clientCfg.Proxies {
			if proxy.ServerPort == 0 || proxy.ClientIP == "" || proxy.ClientPort == 0 {
				return nil, fmt.Errorf("invalid proxy config at index %d for client '%s': server_port, client_ip, and client_port must be set", j, clientCfg.ClientName)
			}
			if existingClient, exists := usedServerPorts[proxy.ServerPort]; exists {
				return nil, fmt.Errorf("server port conflict: port %d is used by both client '%s' and client '%s'", proxy.ServerPort, existingClient, clientCfg.ClientName)
			}
			usedServerPorts[proxy.ServerPort] = clientCfg.ClientName
		}
	}

	return &cfg, nil
}

func startDataListener(serverPort int) {
	serverAddr := fmt.Sprintf(":%d", serverPort)
	listener, err := net.Listen("tcp", serverAddr)
	if err != nil {
		log.Printf("Failed to listen on data port %d: %v", serverPort, err)
		return
	}
	defer listener.Close()

	serverPortToClientNameMutex.RLock()
	clientName, ok := serverPortToClientName[serverPort]
	serverPortToClientNameMutex.RUnlock()
	if !ok {
		log.Printf("CRITICAL ERROR: No client name found for server port %d during listener setup.", serverPort)
		return
	}

	log.Printf("Server listening on port %d for client '%s'", serverPort, clientName)

	for {
		externalConn, err := listener.Accept()
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Temporary() {
				log.Printf("Temporary error accepting connection on port %d: %v", serverPort, err)
				continue
			}
			if err.Error() == "use of closed network connection" {
				log.Printf("Listener for port %d closed.", serverPort)
				return
			}
			log.Printf("Error accepting connection on port %d: %v", serverPort, err)
			return
		}
		go handleNewExternalConnection(externalConn, serverPort, clientName)
	}
}

func handleNewExternalConnection(externalConn net.Conn, serverPort int, clientName string) {
	connID := connIDCounter.Add(1)
	remoteAddr := externalConn.RemoteAddr().String()
	log.Printf("[Conn %d] Accepted external connection from %s on server port %d for client '%s'", connID, remoteAddr, serverPort, clientName)

	authenticatedClientsMutex.Lock()
	controlConn, ok := authenticatedClients[clientName]
	authenticatedClientsMutex.Unlock()

	if !ok {
		log.Printf("[Conn %d] Client '%s' is not connected via control channel. Dropping connection from %s.", connID, clientName, remoteAddr)
		externalConn.Close()
		return
	}

	// Store connection and create a status channel (initially open)
	statusChan := make(chan struct{}) // Create an open channel
	activeExternalConnsMutex.Lock()
	activeExternalConns[connID] = externalConn
	activeExternalConnsMutex.Unlock()
	activeExternalConnStatusMutex.Lock()
	activeExternalConnStatus[connID] = statusChan
	activeExternalConnStatusMutex.Unlock()

	// Cleanup function for this connection state
	cleanup := func() {
		activeExternalConnsMutex.Lock()
		delete(activeExternalConns, connID)
		activeExternalConnsMutex.Unlock()
		activeExternalConnStatusMutex.Lock()
		delete(activeExternalConnStatus, connID)
		activeExternalConnStatusMutex.Unlock()
		externalConn.Close()
		log.Printf("[Conn %d] Cleaned up connection state.", connID)
	}

	// Send NewConnection message to client
	payload := protocol.NewConnectionPayload{ConnID: connID, ServerPort: serverPort}
	msgBytes, err := protocol.NewControlMessage(protocol.MsgNewConnection, payload)
	if err != nil {
		log.Printf("[Conn %d] Failed to create NewConnection message: %v", connID, err)
		cleanup()
		return
	}

	_, err = controlConn.Write(msgBytes)
	if err != nil {
		log.Printf("[Conn %d] Failed to send NewConnection message to client '%s': %v", connID, clientName, err)
		cleanup()
		return
	}

	// ** IMPORTANT: Do NOT start forwardExternalToControl here. **
	// Wait for MsgConnectionReady or MsgCloseConnection from client in handleControlConnection.

	// Optional: Add a timeout for waiting for the client response
	go func() {
		timer := time.NewTimer(15 * time.Second) // Example: 15 second timeout
		defer timer.Stop()
		select {
		case <-statusChan: // Channel closed by handleControlConnection (Ready or Close received)
			// log.Printf("[Conn %d] Status received from client.", connID)
			return
		case <-timer.C:
			log.Printf("[Conn %d] Timeout waiting for ConnectionReady/Close from client '%s'. Closing external connection.", connID, clientName)
			cleanup()
			// Optionally notify client? Difficult if control conn is still alive but unresponsive.
		}
	}()

	log.Printf("[Conn %d] Notified client '%s', waiting for ConnectionReady or Close...", connID, clientName)
}

// forwardExternalToControl is now started by handleControlConnection upon receiving MsgConnectionReady.
func forwardExternalToControl(externalConn, controlConn net.Conn, connID uint64, clientName string) {
	defer func() {
		log.Printf("[Conn %d] Stopped forwarding external -> control for %s.", connID, externalConn.RemoteAddr())
		activeExternalConnsMutex.Lock()
		delete(activeExternalConns, connID)
		activeExternalConnsMutex.Unlock()

		payload := protocol.CloseConnectionPayload{ConnID: connID}
		msgBytes, err := protocol.NewControlMessage(protocol.MsgCloseConnection, payload)
		if err == nil {
			_, err = controlConn.Write(msgBytes)
			if err != nil {
				// Log error, but proceed with closing externalConn
				log.Printf("[Conn %d] Failed to send CloseConnection message to client '%s': %v", connID, clientName, err)
			}
		} else {
			log.Printf("[Conn %d] Failed to create CloseConnection message: %v", connID, err)
		}
		externalConn.Close()
	}()

	buf := make([]byte, 32*1024)
	for {
		n, err := externalConn.Read(buf)
		if err != nil {
			if err != io.EOF {
				log.Printf("[Conn %d] Error reading from external connection %s: %v", connID, externalConn.RemoteAddr(), err)
			}
			return
		}

		if n > 0 {
			dataToSend := make([]byte, n)
			copy(dataToSend, buf[:n])
			payload := protocol.DataPayload{ConnID: connID, Data: dataToSend}
			msgBytes, err := protocol.NewControlMessage(protocol.MsgData, payload)
			fmt.Println("send external payload to client: ", string(msgBytes))
			if err != nil {
				log.Printf("[Conn %d] Failed to create Data message: %v", connID, err)
				return
			}
			_, err = controlConn.Write(msgBytes)
			if err != nil {
				log.Printf("[Conn %d] Failed to send Data message to client '%s': %v", connID, clientName, err)
				return
			}
		}
	}
}

func startControlListener(controlAddr string, cfg *Config) {
	listener, err := net.Listen("tcp", controlAddr)
	if err != nil {
		log.Fatalf("Failed to listen on control port %s: %v", controlAddr, err)
	}
	defer listener.Close()
	log.Printf("Control server listening on %s", controlAddr)

	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Printf("Failed to accept control connection: %v", err)
			continue
		}
		go handleControlConnection(conn, cfg)
	}
}

func handleControlConnection(conn net.Conn, cfg *Config) {
	remoteAddr := conn.RemoteAddr().String()
	log.Printf("Accepted control connection from %s", remoteAddr)

	var authenticatedClient *ClientConfig
	var authenticatedClientName string

	// Authentication Phase
	reader := bufio.NewReader(conn)
	line, err := reader.ReadBytes('\n')
	if err != nil {
		log.Printf("Failed to read auth request from %s: %v", remoteAddr, err)
		conn.Close()
		return
	}
	var authReq protocol.AuthRequest
	err = json.Unmarshal(line, &authReq)
	if err != nil {
		log.Printf("Failed to decode auth request from %s: %v", remoteAddr, err)
		sendAuthResponseWrapped(conn, "error", "invalid request format", nil)
		conn.Close()
		return
	}
	for i := range cfg.ClientConfigs {
		clientCfg := &cfg.ClientConfigs[i]
		if clientCfg.ClientName == authReq.ClientName && clientCfg.ClientKey == authReq.ClientKey {
			authenticatedClient = clientCfg
			authenticatedClientName = clientCfg.ClientName
			break
		}
	}
	if authenticatedClient == nil {
		log.Printf("Authentication failed for client '%s' from %s", authReq.ClientName, remoteAddr)
		sendAuthResponseWrapped(conn, "error", "authentication failed", nil)
		conn.Close()
		return
	}
	log.Printf("Client '%s' authenticated successfully from %s", authenticatedClientName, remoteAddr)
	if !sendAuthResponseWrapped(conn, "ok", "", authenticatedClient.Proxies) {
		conn.Close()
		return
	}

	authenticatedClientsMutex.Lock()
	oldConn, exists := authenticatedClients[authenticatedClientName]
	if exists {
		log.Printf("Client '%s' reconnected, closing previous control connection from %s", authenticatedClientName, oldConn.RemoteAddr())
		oldConn.Close()
	}
	authenticatedClients[authenticatedClientName] = conn
	authenticatedClientsMutex.Unlock()

	defer func() {
		conn.Close()
		authenticatedClientsMutex.Lock()
		if currentConn, ok := authenticatedClients[authenticatedClientName]; ok && currentConn == conn {
			delete(authenticatedClients, authenticatedClientName)
			log.Printf("Removed authenticated client '%s' from map.", authenticatedClientName)
			// TODO: Improve cleanup of associated external connections when control conn closes.
			// Currently relies on forwardExternalToControl detecting write errors.
		}
		authenticatedClientsMutex.Unlock()
		log.Printf("Control connection closed for client '%s' from %s", authenticatedClientName, remoteAddr)
	}()

	// Message processing loop
	scanner := bufio.NewScanner(conn)
	for scanner.Scan() {
		var msg protocol.ControlMessage
		err := json.Unmarshal(scanner.Bytes(), &msg)
		if err != nil {
			log.Printf("[Client %s] Failed to unmarshal control message: %v", authenticatedClientName, err)
			continue
		}

		switch msg.Type {
		case protocol.MsgConnectionReady: // <<< NEW CASE
			var payload protocol.ConnectionReadyPayload
			if err := json.Unmarshal(msg.Payload, &payload); err != nil {
				log.Printf("[Client %s][Conn %d] Failed to unmarshal ConnectionReady payload: %v", authenticatedClientName, payload.ConnID, err)
				continue
			}
			connID := payload.ConnID
			log.Printf("[Client %s][Conn %d] Received ConnectionReady.", authenticatedClientName, connID)

			// Find the status channel and signal readiness
			activeExternalConnStatusMutex.Lock()
			statusChan, statusOk := activeExternalConnStatus[connID]
			if statusOk {
				close(statusChan) // Signal the timeout goroutine
				// Remove from status map AFTER closing channel to prevent race condition with cleanup
				delete(activeExternalConnStatus, connID)
			}
			activeExternalConnStatusMutex.Unlock()

			if !statusOk {
				log.Printf("[Client %s][Conn %d] Received ConnectionReady for unknown or already handled connection.", authenticatedClientName, connID)
				continue
			}

			// Find the external connection
			activeExternalConnsMutex.Lock()
			externalConn, connOk := activeExternalConns[connID]
			activeExternalConnsMutex.Unlock()

			if !connOk {
				log.Printf("[Client %s][Conn %d] Received ConnectionReady but external connection not found (likely timed out or closed).", authenticatedClientName, connID)
				continue
			}

			// Start the forwarding goroutine now
			log.Printf("[Client %s][Conn %d] Starting data forwarding external -> control.", authenticatedClientName, connID)
			go forwardExternalToControl(externalConn, conn, connID, authenticatedClientName)

		case protocol.MsgData:
			var payload protocol.DataPayload
			err := json.Unmarshal(msg.Payload, &payload)
			if err != nil {
				log.Printf("[Client %s][Conn %d] Failed to unmarshal Data payload: %v", authenticatedClientName, payload.ConnID, err)
				continue
			}
			activeExternalConnsMutex.Lock()
			externalConn, ok := activeExternalConns[payload.ConnID]
			activeExternalConnsMutex.Unlock()
			if !ok {
				// Connection might have already been closed by the server side
				// log.Printf("[Client %s][Conn %d] Received data for unknown or closed connection. Ignoring.", authenticatedClientName, payload.ConnID)
				continue
			}
			fmt.Println("Received data for connID", payload.ConnID)
			_, err = externalConn.Write(payload.Data)
			if err != nil {
				// Error writing to external connection likely means it closed
				// log.Printf("[Client %s][Conn %d] Failed to write data to external connection %s: %v", authenticatedClientName, payload.ConnID, externalConn.RemoteAddr(), err)
				externalConn.Close() // This should trigger cleanup in forwardExternalToControl
			}

		case protocol.MsgCloseConnection:
			var payload protocol.CloseConnectionPayload
			err := json.Unmarshal(msg.Payload, &payload)
			if err != nil {
				log.Printf("[Client %s][Conn %d] Failed to unmarshal CloseConnection payload: %v", authenticatedClientName, payload.ConnID, err)
				continue
			}
			connID := payload.ConnID
			log.Printf("[Client %s][Conn %d] Received close request from client.", authenticatedClientName, connID)

			// Signal the timeout goroutine if it exists
			activeExternalConnStatusMutex.Lock()
			statusChan, statusOk := activeExternalConnStatus[connID]
			if statusOk {
				close(statusChan)
				delete(activeExternalConnStatus, connID)
			}
			activeExternalConnStatusMutex.Unlock()

			// Close the external connection if it exists
			activeExternalConnsMutex.Lock()
			if externalConn, connOk := activeExternalConns[connID]; connOk {
				delete(activeExternalConns, connID) // Remove before closing
				log.Printf("[Client %s][Conn %d] Closing external connection %s due to client request/failure.", authenticatedClientName, connID, externalConn.RemoteAddr())
				externalConn.Close()
			}
			activeExternalConnsMutex.Unlock()

		default:
			log.Printf("[Client %s] Received unknown control message type: %s", authenticatedClientName, msg.Type)
		}
	}

	if err := scanner.Err(); err != nil {
		authenticatedClientsMutex.Lock()
		closedIntentionally := false
		if currentConn, ok := authenticatedClients[authenticatedClientName]; !ok || currentConn != conn {
			closedIntentionally = true // Connection was likely closed due to new login
		}
		authenticatedClientsMutex.Unlock()
		if !closedIntentionally {
			log.Printf("[Client %s] Error reading control messages: %v", authenticatedClientName, err)
		}
	}
}

func sendAuthResponseWrapped(conn net.Conn, status string, message string, proxies []protocol.ClientProxy) bool {
	payload := protocol.AuthResponsePayload{Status: status, Message: message, Proxies: proxies}
	msgBytes, err := protocol.NewControlMessage(protocol.MsgAuthResponse, payload)
	if err != nil {
		log.Printf("Failed to marshal AuthResponse message: %v", err)
		return false
	}

	_, err = conn.Write(msgBytes)
	if err != nil {
		log.Printf("Failed to send AuthResponse message to %s: %v", conn.RemoteAddr(), err)
		return false
	}
	return true
}

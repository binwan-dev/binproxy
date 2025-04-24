package protocol

import "encoding/json"

// MessageType defines the type of control message.
type MessageType string

const (
	// MsgNewConnection is sent by the server to the client when a new external connection is accepted.
	MsgNewConnection MessageType = "new_connection"
	// MsgData is used to transfer data chunks over the control channel.
	MsgData MessageType = "data"
	// MsgCloseConnection is sent by either server or client to indicate the proxied connection should be closed.
	MsgCloseConnection MessageType = "close_connection"
	// MsgAuthResponse is the response to the initial authentication request.
	MsgAuthResponse MessageType = "auth_response"
	// MsgConnectionReady is sent by the client to the server when it has successfully connected to the local target.
	MsgConnectionReady MessageType = "connection_ready"
)

// ControlMessage is the wrapper for all messages exchanged over the control channel after authentication.
type ControlMessage struct {
	Type    MessageType     `json:"type"`
	Payload json.RawMessage `json:"payload"` // Use RawMessage to delay payload parsing
}

// NewConnectionPayload is the payload for MsgNewConnection.
type NewConnectionPayload struct {
	ConnID     uint64 `json:"conn_id"`
	ServerPort int    `json:"server_port"` // Tells client which proxy rule this is for
}

// DataPayload is the payload for MsgData.
type DataPayload struct {
	ConnID uint64 `json:"conn_id"`
	Data   []byte `json:"data"`
}

// CloseConnectionPayload is the payload for MsgCloseConnection.
type CloseConnectionPayload struct {
	ConnID uint64 `json:"conn_id"`
}

// ConnectionReadyPayload is the payload for MsgConnectionReady.
type ConnectionReadyPayload struct {
	ConnID uint64 `json:"conn_id"`
}

// ClientProxy defines a single proxy mapping rule for a client.
// It's used in both server config and sent to the client upon auth.
type ClientProxy struct {
	ServerPort int    `yaml:"server_port" json:"server_port"`
	ClientIP   string `yaml:"client_ip" json:"client_ip"`
	ClientPort int    `yaml:"client_port" json:"client_port"`
}

// AuthRequest defines the structure for the initial client authentication message.
// This message is sent *before* the ControlMessage wrapper is used.
type AuthRequest struct {
	ClientName string `json:"client_name"`
	ClientKey  string `json:"client_key"`
}

// AuthResponsePayload defines the structure for the server authentication response payload.
// This is used as the payload for MsgAuthResponse.
type AuthResponsePayload struct {
	Status  string        `json:"status"`
	Message string        `json:"message,omitempty"`
	Proxies []ClientProxy `json:"proxies,omitempty"` // List of proxies client should handle
}

// NewControlMessage creates a new JSON message with the correct wrapper and type.
func NewControlMessage(msgType MessageType, payload interface{}) ([]byte, error) {
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	msg := ControlMessage{
		Type:    msgType,
		Payload: payloadBytes,
	}
	msgBytes, err := json.Marshal(msg)
	if err != nil {
		return nil, err
	}
	return append(msgBytes, '\n'), nil // Append newline delimiter
}

// Helper function removed as it's no longer needed for the simplified protocol

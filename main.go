package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http" // Import for stack trace
	"sync"
	"time"

	"github.com/google/uuid" // 用于生成唯一的Room ID
	"github.com/gorilla/websocket"
)

// WebSocket通信相关的常量设置
const (
	// writeWait 是允许向对端写入消息的时间。
	writeWait = 10 * time.Second

	// pongWait 是允许从对端读取下一个pong消息的时间。
	// 必须大于pingPeriod。如果在这段时间内没有收到pong，连接将被视为断开。
	pongWait = 60 * time.Second

	// pingPeriod 是向对端发送ping消息的周期。必须小于pongWait。
	// 用于保持连接活跃并检查连接状态。
	pingPeriod = (pongWait * 9) / 10

	// maxMessageSize 是允许从对端接收的最大消息大小。
	maxMessageSize = 1024 // 稍微增大消息大小以容纳更复杂的游戏状态/房间列表
)

// upgrader 用于将HTTP连接升级到WebSocket连接。
var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024, // WebSocket连接的读缓冲大小
	WriteBufferSize: 1024, // WebSocket连接的写缓冲大小
	// CheckOrigin 用于校验请求的来源。这里设置为总是返回true，允许所有来源的连接。
	// 在生产环境中，应该实现一个更安全的检查，例如只允许来自特定域的请求。
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

// Forward declarations for request types used in channels
type createRoomRequest struct {
	client   *Client
	roomName string
	// Add other room params like maxPlayers, password if needed
}

type joinRoomRequest struct {
	client *Client
	roomID string
	// Add password if needed
}

// GameState is a placeholder for actual game-specific state.
// You'll replace this with your game's state structure.
type GameState struct {
	Status      string            `json:"status"`       // e.g., "waiting_for_players", "in_progress", "finished"
	CurrentTurn string            `json:"current_turn"` // Nickname of the player whose turn it is
	PlayerData  map[string]string `json:"player_data"`  // Placeholder for player-specific game data (e.g., cards, score)
	Board       interface{}       `json:"board"`        // Placeholder for common game board/area
}

// Client represents a connected user.
type Client struct {
	roomManager *RoomManager    // Pointer to the room manager
	conn        *websocket.Conn // The WebSocket connection
	send        chan []byte     // Buffered channel of outbound messages
	nickname    string          // User's nickname
	currentRoom *Room           // Pointer to the room the client is currently in (nil if in lobby)
}

// Message defines the structure for messages exchanged.
type Message struct {
	Type           string      `json:"type"`                      // e.g., "join_lobby", "create_room", ..., "private_room_chat"
	Nickname       string      `json:"nickname,omitempty"`        // Sender's nickname
	TargetNickname string      `json:"target_nickname,omitempty"` // For private messages
	Text           string      `json:"text,omitempty"`            // For chat messages or simple text payloads
	RoomID         string      `json:"room_id,omitempty"`         // Target or source Room ID
	RoomName       string      `json:"room_name,omitempty"`       // For creating rooms or displaying room info
	Payload        interface{} `json:"payload,omitempty"`         // For complex data
}

// Room represents a single game room.
type Room struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	clients     map[*Client]bool
	broadcast   chan Message // Channel for messages specific to this room (chat, game actions)
	register    chan *Client
	unregister  chan *Client
	roomManager *RoomManager // So room can notify manager (e.g., when empty)
	host        *Client      // The client who created the room
	gameState   GameState    // Current state of the game in this room
	maxPlayers  int          `json:"max_players"`
	// runOnce     sync.Once // To ensure run() is called only once for a room (managed by RoomManager on creation)
	mutex sync.RWMutex // For protecting concurrent access to room's clients map or gameState
}

// RoomSummary is a lighter struct for sending room list information.
type RoomSummary struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	MaxPlayers     int    `json:"max_players"`
	CurrentPlayers int    `json:"current_players"`
}

// RoomManager manages all active rooms and clients in the lobby.
type RoomManager struct {
	rooms            map[string]*Room       // Active rooms, key is RoomID
	clients          map[*Client]bool       // All connected clients (in lobby or in a room)
	registerClient   chan *Client           // Channel for new clients connecting to the server (lobby)
	unregisterClient chan *Client           // Channel for clients disconnecting from the server
	createRoomCh     chan createRoomRequest // Channel for create room requests
	joinRoomCh       chan joinRoomRequest   // Channel for join room requests
	listRoomsCh      chan *Client           // Channel for list rooms requests (client requests their own list)
	// broadcastToLobby chan Message // If you need to broadcast messages to all clients in the lobby

	mutex sync.RWMutex // For protecting concurrent access to rooms and clients maps
}

// newRoomManager 创建并返回一个新的RoomManager实例。
func newRoomManager() *RoomManager {
	return &RoomManager{
		rooms:            make(map[string]*Room),
		clients:          make(map[*Client]bool),
		registerClient:   make(chan *Client),
		unregisterClient: make(chan *Client),
		createRoomCh:     make(chan createRoomRequest),
		joinRoomCh:       make(chan joinRoomRequest),
		listRoomsCh:      make(chan *Client),
	}
}

// run 是RoomManager的主事件循环。它必须在一个单独的goroutine中运行。
// run负责处理来自register, unregister, 和 broadcast 通道的消息。
func (rm *RoomManager) run() {
	log.Println("RoomManager started")
	defer func() {
		if r := recover(); r != nil {
			log.Printf("RoomManager FATAL ERROR: run() panicked: %v", r)
		} else {
			log.Println("RoomManager: run() goroutine finished.")
		}
	}()

	for {
		select {
		case client := <-rm.registerClient:
			rm.mutex.Lock()
			rm.clients[client] = true
			rm.mutex.Unlock()
			log.Printf("RoomManager: Client %s (%s) registered.", client.nickname, client.conn.RemoteAddr())
		case client := <-rm.unregisterClient:
			rm.mutex.Lock()
			if _, ok := rm.clients[client]; ok {
				if client.currentRoom != nil {
					select {
					case client.currentRoom.unregister <- client:
					default:
						// If room is already stopped or channel full, this might not be processed by room.
						// Client's currentRoom reference will be nilled below.
						log.Printf("RoomManager: Failed to send unregister notification for %s to room %s channel (room stopped or channel full?).", client.nickname, client.currentRoom.ID)
						client.currentRoom = nil
					}
				}
				delete(rm.clients, client)
				close(client.send)
				log.Printf("RoomManager: Client %s (%s) unregistered.", client.nickname, client.conn.RemoteAddr())
			}
			rm.mutex.Unlock()

		case req := <-rm.createRoomCh:
			roomID := uuid.New().String()
			newRoom := &Room{
				ID:          roomID,
				Name:        req.roomName,
				clients:     make(map[*Client]bool),
				broadcast:   make(chan Message, 256),
				register:    make(chan *Client),
				unregister:  make(chan *Client),
				roomManager: rm,
				host:        req.client,
				gameState:   GameState{Status: "waiting_for_players", PlayerData: make(map[string]string)},
				maxPlayers:  4,
				mutex:       sync.RWMutex{},
			}
			rm.mutex.Lock()
			rm.rooms[roomID] = newRoom
			rm.mutex.Unlock()
			go newRoom.run()
			log.Printf("RoomManager: Room %s ('%s') created by %s.", roomID, req.roomName, req.client.nickname)
			newRoom.register <- req.client
			confirmMsg := Message{
				Type:     "room_created_ok",
				RoomID:   roomID,
				RoomName: newRoom.Name,
				Payload:  newRoom.getRoomInfoPayload(req.client),
			}
			confirmBytes, _ := json.Marshal(confirmMsg)
			select {
			case req.client.send <- confirmBytes:
				log.Printf("RoomManager: Successfully sent room_created_ok to %s.", req.client.nickname)
			default:
				log.Printf("RoomManager: Failed to send room_created_ok to %s.", req.client.nickname)
			}

		case req := <-rm.joinRoomCh:
			rm.mutex.RLock()
			room, exists := rm.rooms[req.roomID]
			rm.mutex.RUnlock()

			if exists {
				room.mutex.RLock()
				canJoin := len(room.clients) < room.maxPlayers
				currentPlayersInRoom := len(room.clients)
				room.mutex.RUnlock()

				if canJoin {
					room.register <- req.client
				} else {
					errorMsg := Message{Type: "error", Text: fmt.Sprintf("Room is full (%d/%d players).", currentPlayersInRoom, room.maxPlayers)}
					errorBytes, _ := json.Marshal(errorMsg)
					select {
					case req.client.send <- errorBytes:
					default:
						log.Printf("RoomManager: Failed to send 'room full' error to %s.", req.client.nickname)
					}
					log.Printf("RoomManager: Client %s failed to join room %s (full %d/%d).", req.client.nickname, req.roomID, currentPlayersInRoom, room.maxPlayers)
				}
			} else {
				errorMsg := Message{Type: "error", Text: "Room not found."}
				errorBytes, _ := json.Marshal(errorMsg)
				select {
				case req.client.send <- errorBytes:
				default:
					log.Printf("RoomManager: Failed to send 'room not found' error to %s.", req.client.nickname)
				}
				log.Printf("RoomManager: Client %s failed to join room %s (not found).", req.client.nickname, req.roomID)
			}

		case client := <-rm.listRoomsCh:
			var roomSummaries []RoomSummary
			rm.mutex.RLock()
			for _, room := range rm.rooms {
				room.mutex.RLock()
				summary := RoomSummary{
					ID:             room.ID,
					Name:           room.Name,
					MaxPlayers:     room.maxPlayers,
					CurrentPlayers: len(room.clients),
				}
				room.mutex.RUnlock()
				roomSummaries = append(roomSummaries, summary)
			}
			rm.mutex.RUnlock()

			listMsg := Message{Type: "room_list_update", Payload: roomSummaries}
			listBytes, err := json.Marshal(listMsg)
			if err != nil {
				log.Printf("RoomManager: ERROR marshalling room list for %s: %v", client.nickname, err)
				continue
			}
			select {
			case client.send <- listBytes:
				log.Printf("RoomManager: Successfully sent room_list_update to %s.", client.nickname)
			default:
				log.Printf("RoomManager: Failed to send room list update to %s.", client.nickname)
			}
		}
	}
}

func (room *Room) run() {
	log.Printf("Room %s ('%s') started.", room.ID, room.Name)
	defer func() {
		if r := recover(); r != nil {
			log.Printf("Room %s ('%s') FATAL ERROR: run() panicked: %v", room.ID, room.Name, r)
		}
		log.Printf("Room %s ('%s') stopping.", room.ID, room.Name)
		room.roomManager.notifyRoomClosed(room.ID)
	}()

	for {
		select {
		case client := <-room.register:
			log.Printf("Room %s: ENTERING register case for %s", room.ID, client.nickname)

			var roomInfoPayload interface{}
			var currentPlayerNicksForNotification []string
			var joinOkMsg Message
			var notification Message
			sendJoinOk := false
			sendNotification := false

			room.mutex.Lock() // Acquire Write Lock
			log.Printf("Room %s: Acquired LOCK for register %s", room.ID, client.nickname)

			if len(room.clients) >= room.maxPlayers {
				// Room is full, unlock before sending error
				room.mutex.Unlock()
				log.Printf("Room %s: Client %s attempted to join full room (max %d).", room.ID, client.nickname, room.maxPlayers)
				errorMsg := Message{Type: "error", Text: fmt.Sprintf("Room is full (%d/%d players).", len(room.clients), room.maxPlayers)} // len(room.clients) might be slightly stale here, but ok for error
				errorBytes, _ := json.Marshal(errorMsg)
				select {
				case client.send <- errorBytes:
				default:
					log.Printf("Room %s: Failed to send 'room full' error to %s.", room.ID, client.nickname)
				}
				continue // Continue to next select iteration
			}

			// Add client to room
			room.clients[client] = true
			client.currentRoom = room
			log.Printf("Room %s: Client %s added to room.clients. Total clients: %d", room.ID, client.nickname, len(room.clients))

			// Prepare payload for the joining client (needs current player list)
			roomInfoPayload = room.getRoomInfoPayload(client) // getRoomInfoPayload expects lock to be held

			// Prepare notification for other clients (needs current player list)
			for c := range room.clients {
				currentPlayerNicksForNotification = append(currentPlayerNicksForNotification, c.nickname)
			}
			notification = Message{
				Type:     "player_joined_room",
				Nickname: client.nickname,
				RoomID:   room.ID,
				Payload:  map[string]interface{}{"players": currentPlayerNicksForNotification},
			}
			sendNotification = true // Mark notification for sending

			// Prepare join_ok message for the joining client
			joinOkMsg = Message{
				Type:     "room_joined_ok",
				RoomID:   room.ID,
				RoomName: room.Name,
				Payload:  roomInfoPayload,
			}
			sendJoinOk = true // Mark join_ok for sending

			log.Printf("Room %s: Releasing LOCK for register %s", room.ID, client.nickname)
			room.mutex.Unlock() // Release Write Lock BEFORE any sends or broadcast calls

			// Send confirmation to the joining client (if marked for sending)
			if sendJoinOk {
				joinOkBytes, err := json.Marshal(joinOkMsg)
				if err != nil {
					log.Printf("Room %s: ERROR marshalling joinOkMsg for %s: %v", room.ID, client.nickname, err)
				} else {
					log.Printf("Room %s: Marshalled joinOkMsg for %s. Attempting to send.", room.ID, client.nickname)
					select {
					case client.send <- joinOkBytes:
						log.Printf("Room %s: Successfully sent room_joined_ok to %s.", room.ID, client.nickname)
					default:
						log.Printf("Room %s: Failed to send room_joined_ok to %s (channel full/closed?).", room.ID, client.nickname)
					}
				}
			}

			// Broadcast to OTHERS (if marked for sending)
			if sendNotification {
				log.Printf("Room %s: Preparing player_joined_room notification for others about %s", room.ID, client.nickname)
				room.broadcastMessage(notification, client) // Pass client to exclude them
				log.Printf("Room %s: broadcastMessage called for player_joined_room about %s.", room.ID, client.nickname)
			}
			log.Printf("Room %s: Finished processing registration for %s.", room.ID, client.nickname)

		case client := <-room.unregister:
			originalNickname := client.nickname
			var playersAfterLeave []string
			wasInRoom := false
			var notification Message
			sendNotification := false

			room.mutex.Lock() // Acquire Write Lock
			if _, ok := room.clients[client]; ok {
				wasInRoom = true
				delete(room.clients, client)
				client.currentRoom = nil // Important to do this under lock

				// Get updated player list for notification
				for c := range room.clients {
					playersAfterLeave = append(playersAfterLeave, c.nickname)
				}
				notification = Message{
					Type:     "player_left_room",
					Nickname: originalNickname,
					RoomID:   room.ID,
					Payload:  map[string]interface{}{"players": playersAfterLeave},
				}
				sendNotification = true
			}
			room.mutex.Unlock() // Release Write Lock

			if wasInRoom {
				log.Printf("Room %s: Client %s left. Total clients: %d", room.ID, originalNickname, len(playersAfterLeave))
				if sendNotification {
					room.broadcastMessage(notification, nil) // Broadcast to all remaining
				}
			} else {
				log.Printf("Room %s: unregister: Client %s was not in room.clients map.", room.ID, originalNickname)
			}

		case message := <-room.broadcast:
			log.Printf("Room %s: DEBUG: Received message on broadcast channel. Type: %s, Sender: %s, Target: %s, Text: %s", room.ID, message.Type, message.Nickname, message.TargetNickname, message.Text)

			if message.Type == "game_action" {
				var updatedStateMsg Message
				var gameStateToBroadcast GameState

				room.mutex.Lock()
				log.Printf("Room %s: Game action from %s: %v", room.ID, message.Nickname, message.Payload)
				// --- TODO: Implement Game Logic Here ---
				gameStateToBroadcast = room.gameState
				room.mutex.Unlock()

				updatedStateMsg = Message{
					Type:    "game_state_update",
					RoomID:  room.ID,
					Payload: gameStateToBroadcast,
				}
				room.broadcastMessage(updatedStateMsg, nil)

			} else if message.Type == "room_chat" {
				log.Printf("Room %s: Chat from %s: %s", room.ID, message.Nickname, message.Text)
				room.broadcastMessage(message, nil) // Broadcast to all
			} else if message.Type == "private_room_chat" {
				if message.TargetNickname == "" {
					log.Printf("Room %s: Private chat from %s missing target_nickname. Ignoring.", room.ID, message.Nickname)
					// Client-side should ideally prevent sending such messages.
					// sendPrivateMessage will handle the case where target is not found, which is effectively the same here.
					continue
				}
				log.Printf("Room %s: Private chat from %s to %s: %s", room.ID, message.Nickname, message.TargetNickname, message.Text)
				room.sendPrivateMessage(message)
			} else {
				log.Printf("Room %s: Received unknown message type '%s' on broadcast channel. Ignoring.", room.ID, message.Type)
			}
		}
	}
}

// Helper to broadcast a message to all clients in the room, optionally excluding one client.
func (room *Room) broadcastMessage(message Message, exclude *Client) {
	messageBytes, err := json.Marshal(message)
	if err != nil {
		log.Printf("Room %s: ERROR marshalling message for broadcast: %v", room.ID, err)
		return
	}

	room.mutex.RLock()
	defer room.mutex.RUnlock()

	for client := range room.clients {
		if client == exclude {
			continue
		}
		select {
		case client.send <- messageBytes:
		default:
			log.Printf("Room %s: Client %s send channel full/closed during broadcast. Will be cleaned up.", room.ID, client.nickname)
		}
	}
}

// Helper to get room info payload, including current players
// This function assumes that the caller is holding an appropriate lock (read or write) on the room.
func (room *Room) getRoomInfoPayload(requestingClient *Client) interface{} {
	// room.mutex.RLock() // REMOVED: Caller (e.g., register case) holds the lock
	// defer room.mutex.RUnlock() // REMOVED

	playerNicknames := []string{}
	for c := range room.clients {
		playerNicknames = append(playerNicknames, c.nickname)
	}

	return struct {
		ID         string    `json:"id"`
		Name       string    `json:"name"`
		Host       string    `json:"host"` // Nickname of the host
		MaxPlayers int       `json:"max_players"`
		Players    []string  `json:"players"`
		GameState  GameState `json:"game_state"`
	}{
		ID:         room.ID,
		Name:       room.Name,
		Host:       room.host.nickname,
		MaxPlayers: room.maxPlayers,
		Players:    playerNicknames,
		GameState:  room.gameState,
	}
}

// Method for RoomManager to be notified by a room when it's closing (e.g., empty)
func (rm *RoomManager) notifyRoomClosed(roomID string) {
	rm.mutex.Lock()
	defer rm.mutex.Unlock()
	if room, ok := rm.rooms[roomID]; ok {
		// Ensure all clients are out of the room conceptually
		room.mutex.Lock() // Lock room before iterating its clients
		for client := range room.clients {
			client.currentRoom = nil // Ensure client knows it's not in a room anymore
		}
		room.clients = make(map[*Client]bool) // Clear room's client list
		room.mutex.Unlock()

		delete(rm.rooms, roomID)
		log.Printf("RoomManager: Room %s closed and removed.", roomID)
	}
}

// readPump 从WebSocket连接中读取消息，并将其转发给Hub。
// 每个客户端连接都会在其自己的goroutine中运行一个readPump。
func (c *Client) readPump() {
	defer func() {
		// This defer handles client disconnection cleanup
		log.Printf("Client %s (%s) readPump defer: Unregistering from RoomManager.", c.nickname, c.conn.RemoteAddr())
		c.roomManager.unregisterClient <- c // This will also handle room unregistration if applicable
		c.conn.Close()
	}()

	c.conn.SetReadLimit(maxMessageSize)
	c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error { c.conn.SetReadDeadline(time.Now().Add(pongWait)); return nil })

	// First message from client is expected to be a "join_lobby" message with nickname
	// (This replaces the old "join" message logic from the simple chat)
	_, rawJoinMsg, err := c.conn.ReadMessage()
	if err != nil {
		log.Printf("Error reading initial message from %s: %v", c.conn.RemoteAddr(), err)
		return
	}

	var joinLobbyMsg Message
	if err := json.Unmarshal(rawJoinMsg, &joinLobbyMsg); err != nil || (joinLobbyMsg.Type != "join_lobby" && joinLobbyMsg.Type != "join") || joinLobbyMsg.Nickname == "" {
		log.Printf("Invalid initial message from %s: type '%s', nick '%s', err: %v, raw: %s", c.conn.RemoteAddr(), joinLobbyMsg.Type, joinLobbyMsg.Nickname, err, rawJoinMsg)
		c.conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "Invalid initial message: must be {type: 'join_lobby', nickname: 'your_nick'}"))
		return
	}
	c.nickname = joinLobbyMsg.Nickname
	log.Printf("Client %s (%s) sent initial join_lobby. Registering with RoomManager.", c.nickname, c.conn.RemoteAddr())
	c.roomManager.registerClient <- c // Now register with RoomManager after nickname is known

	// Main message loop
	for {
		_, messageBytes, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("Client %s (%s) readPump error: %v", c.nickname, c.conn.RemoteAddr(), err)
			} else {
				log.Printf("Client %s (%s) connection closed or read error: %v", c.nickname, c.conn.RemoteAddr(), err)
			}
			break // Exit loop, defer will handle unregistration
		}

		var msg Message
		if err := json.Unmarshal(messageBytes, &msg); err != nil {
			log.Printf("Client %s (%s) sent invalid JSON: %v. Message: %s", c.nickname, c.conn.RemoteAddr(), err, messageBytes)
			// Optionally send an error back to client
			errorResponse := Message{Type: "error", Text: "Invalid JSON format."}
			errorBytes, _ := json.Marshal(errorResponse)
			c.send <- errorBytes
			continue
		}

		msg.Nickname = c.nickname // Ensure server-side nickname is used for messages processed further

		// Route message based on type
		switch msg.Type {
		case "create_room":
			log.Printf("Client %s: Handling create_room request for room name '%s'", c.nickname, msg.RoomName)
			c.roomManager.createRoomCh <- createRoomRequest{client: c, roomName: msg.RoomName}
		case "join_room":
			log.Printf("Client %s: Handling join_room request for room ID '%s'", c.nickname, msg.RoomID)
			if c.currentRoom != nil {
				errorResponse := Message{Type: "error", Text: "You are already in a room. Leave current room first."}
				errorBytes, _ := json.Marshal(errorResponse)
				c.send <- errorBytes
				continue
			}
			c.roomManager.joinRoomCh <- joinRoomRequest{client: c, roomID: msg.RoomID}
		case "list_rooms":
			log.Printf("Client %s: Handling list_rooms request", c.nickname)
			c.roomManager.listRoomsCh <- c
		case "leave_room":
			log.Printf("Client %s: Handling leave_room request from room %s", c.nickname, c.currentRoom.ID) // Potentially nil if not in room
			if c.currentRoom != nil {
				c.currentRoom.unregister <- c
			} else {
				errorResponse := Message{Type: "error", Text: "You are not in a room."}
				errorBytes, _ := json.Marshal(errorResponse)
				c.send <- errorBytes
			}
		case "room_chat":
			if c.currentRoom != nil {
				log.Printf("Client %s in room %s: Forwarding room_chat: %s", c.nickname, c.currentRoom.ID, msg.Text)
				c.currentRoom.broadcast <- msg
			} else {
				log.Printf("Client %s: Tried to send room_chat while not in a room.", c.nickname)
				errorResponse := Message{Type: "error", Text: "You must be in a room to chat."}
				errorBytes, _ := json.Marshal(errorResponse)
				c.send <- errorBytes
			}
		case "private_room_chat":
			if c.currentRoom != nil {
				log.Printf("Client %s in room %s: Forwarding private_room_chat to target %s: %s", c.nickname, c.currentRoom.ID, msg.TargetNickname, msg.Text)
				// msg already has Nickname (sender) and TargetNickname set by client
				c.currentRoom.broadcast <- msg // Room.run() will handle routing this as private
			} else {
				log.Printf("Client %s: Tried to send private_room_chat while not in a room.", c.nickname)
				errorResponse := Message{Type: "error", Text: "You must be in a room to send a private chat."}
				errorBytes, _ := json.Marshal(errorResponse)
				c.send <- errorBytes
			}
		case "game_action":
			if c.currentRoom != nil {
				log.Printf("Client %s in room %s: Forwarding game_action: %v", c.nickname, c.currentRoom.ID, msg.Payload)
				c.currentRoom.broadcast <- msg
			} else {
				log.Printf("Client %s: Tried to send game_action while not in a room.", c.nickname)
				errorResponse := Message{Type: "error", Text: "You must be in a room to perform game actions."}
				errorBytes, _ := json.Marshal(errorResponse)
				c.send <- errorBytes
			}
		default:
			log.Printf("Client %s (%s) sent unknown message type: %s", c.nickname, c.conn.RemoteAddr(), msg.Type)
			errorResponse := Message{Type: "error", Text: "Unknown message type: " + msg.Type}
			errorBytes, _ := json.Marshal(errorResponse)
			c.send <- errorBytes
		}
	}
}

// writePump 将Hub传来的消息写入WebSocket连接。
// 每个客户端连接都会在其自己的goroutine中运行一个writePump。
func (c *Client) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		// Note: conn.Close() is handled by readPump's defer or RoomManager's unregister logic
		// if readPump exits first. If writePump exits due to an error,
		// it should also ensure conn.Close() is called, possibly by triggering readPump's exit.
		// For simplicity now, readPump's defer is the primary closer.
		log.Printf("Client %s (%s) writePump stopping.", c.nickname, c.conn.RemoteAddr())
	}()
	for {
		select {
		case message, ok := <-c.send:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				// The RoomManager or Room has closed the channel.
				log.Printf("Client %s (%s) send channel closed.", c.nickname, c.conn.RemoteAddr()) // Simplified log
				c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			// Send the message as a single WebSocket TextMessage
			if err := c.conn.WriteMessage(websocket.TextMessage, message); err != nil {
				log.Printf("Client %s (%s) writePump WriteMessage error: %v", c.nickname, c.conn.RemoteAddr(), err)
				return
			}

		case <-ticker.C:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				log.Printf("Client %s (%s) writePump Ping error: %v", c.nickname, c.conn.RemoteAddr(), err)
				return // Assume connection is dead
			}
		}
	}
}

// serveWs 处理来自客户端的WebSocket请求。
// 它负责将HTTP连接升级为WebSocket连接，并为该连接创建一个新的Client实例。
func serveWs(rm *RoomManager, w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("Failed to upgrade connection:", err)
		return
	}

	// Create a new Client instance. Nickname will be set after initial "join_lobby" message.
	// The client is not registered with RoomManager until nickname is known via readPump.
	client := &Client{
		roomManager: rm,
		conn:        conn,
		send:        make(chan []byte, 256),
		// nickname and currentRoom are set later
	}
	// Log pending client immediately for debugging if needed.
	log.Printf("Pending client connection from %s. Waiting for join_lobby message.", conn.RemoteAddr())

	// Start read and write pumps. readPump will handle registration with RoomManager.
	go client.writePump()
	go client.readPump()
}

// main 是程序的入口点。
func main() {
	roomManager := newRoomManager()
	go roomManager.run()

	http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		serveWs(roomManager, w, r)
	})

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		http.ServeFile(w, r, "index.html")
	})

	port := ":8084"
	log.Println("HTTP server with WebSocket Room Manager started on", port)
	err := http.ListenAndServe(port, nil)
	if err != nil {
		log.Fatal("ListenAndServe: ", err)
	}
}

// Helper to send a private message to a specific client in the room and echo to sender.
func (room *Room) sendPrivateMessage(message Message) {
	messageBytes, err := json.Marshal(message)
	if err != nil {
		log.Printf("Room %s: ERROR marshalling private message for %s to %s: %v", room.ID, message.Nickname, message.TargetNickname, err)
		return
	}

	room.mutex.RLock() // Lock for reading room.clients
	defer room.mutex.RUnlock()

	var targetClient *Client
	var senderClient *Client

	for clientInRoom := range room.clients { // Renamed loop variable for clarity
		if clientInRoom.nickname == message.TargetNickname {
			targetClient = clientInRoom
		}
		if clientInRoom.nickname == message.Nickname { // Assuming message.Nickname is the sender
			senderClient = clientInRoom
		}
		// Optimization: if both found and target is not sender, can break early if only one target/sender matters.
		// However, if there could be multiple clients with same nickname (not ideal), this loop structure is safer.
	}

	// Send to target (if found and not the sender themselves)
	if targetClient != nil && targetClient != senderClient {
		select {
		case targetClient.send <- messageBytes:
			log.Printf("Room %s: Sent private message from %s to %s.", room.ID, message.Nickname, targetClient.nickname)
		default:
			log.Printf("Room %s: Target client %s send channel full/closed during private message. Will be cleaned up.", room.ID, targetClient.nickname)
		}
	} else if targetClient == nil && (senderClient == nil || message.TargetNickname != senderClient.nickname) { // Only send error if target truly not found AND not a self-whisper attempt that got filtered
		log.Printf("Room %s: Target client %s not found for private message from %s.", room.ID, message.TargetNickname, message.Nickname)
		if senderClient != nil { // Notify sender that target was not found
			errorToSender := Message{
				Type: "error",
				Text: fmt.Sprintf("User '%s' not found in this room or is not available for private messages.", message.TargetNickname),
			}
			errorBytes, _ := json.Marshal(errorToSender)
			select {
			case senderClient.send <- errorBytes:
			default:
				log.Printf("Room %s: Failed to send target_not_found error to sender %s", room.ID, senderClient.nickname)
			}
		}
	}

	// Echo to sender (if found). This ensures the sender sees what they sent.
	// If it was a self-chat (targetNickname == Nickname), it would only be sent here if targetClient == senderClient.
	// The previous block (targetClient != senderClient) would not have sent.
	if senderClient != nil {
		select {
		case senderClient.send <- messageBytes:
			log.Printf("Room %s: Echoed private message (from %s to target: %s) to sender.", room.ID, senderClient.nickname, message.TargetNickname)
		default:
			log.Printf("Room %s: Sender client %s send channel full/closed during private message echo. Will be cleaned up.", room.ID, senderClient.nickname)
		}
	}
}

package main

import (
	"fmt"
	"log"
	"time"

	"github.com/gorilla/websocket"
)

type ClientMessage struct {
	Type    string `json:"type"`
	To      string `json:"to,omitempty"`
	Content string `json:"content,omitempty"`
	UserID  string `json:"userId,omitempty"`
}

type ServerMessage struct {
	Type    string `json:"type"`
	From    string `json:"from,omitempty"`
	Content string `json:"content,omitempty"`
	Error   string `json:"error,omitempty"`
}

func connectAndRegister(userID, gatewayURL string) (*websocket.Conn, error) {
	conn, _, err := websocket.DefaultDialer.Dial(gatewayURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to connect: %w", err)
	}

	// Register
	registerMsg := ClientMessage{
		Type:   "register",
		UserID: userID,
	}

	if err := conn.WriteJSON(registerMsg); err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to register: %w", err)
	}

	// Wait for registration confirmation
	var msg ServerMessage
	if err := conn.ReadJSON(&msg); err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to read registration response: %w", err)
	}

	if msg.Type != "registered" {
		conn.Close()
		return nil, fmt.Errorf("unexpected response: %s", msg.Type)
	}

	fmt.Printf("✓ %s registered on %s\n", userID, gatewayURL)
	return conn, nil
}

func sendMessage(conn *websocket.Conn, to, content string) error {
	msg := ClientMessage{
		Type:    "message",
		To:      to,
		Content: content,
	}
	return conn.WriteJSON(msg)
}

func readMessage(conn *websocket.Conn) (*ServerMessage, error) {
	var msg ServerMessage
	err := conn.ReadJSON(&msg)
	return &msg, err
}

func main() {
	fmt.Println("=== Testing Cross-Gateway Messaging ===\n")

	// Connect Alice to Gateway-01
	fmt.Println("1. Connecting Alice to Gateway-01 (port 8080)...")
	aliceConn, err := connectAndRegister("alice", "ws://localhost:8080/ws")
	if err != nil {
		log.Fatalf("Failed to connect Alice: %v", err)
	}
	defer aliceConn.Close()

	// Connect Bob to Gateway-02
	fmt.Println("2. Connecting Bob to Gateway-02 (port 8081)...")
	bobConn, err := connectAndRegister("bob", "ws://localhost:8081/ws")
	if err != nil {
		log.Fatalf("Failed to connect Bob: %v", err)
	}
	defer bobConn.Close()

	fmt.Println("\n=== Testing Message Routing ===\n")

	// Alice sends message to Bob (cross-gateway)
	fmt.Println("3. Alice → Bob: 'Hello from Gateway-01!'")
	if err := sendMessage(aliceConn, "bob", "Hello from Gateway-01!"); err != nil {
		log.Fatalf("Failed to send message: %v", err)
	}

	// Bob should receive the message
	done := make(chan bool)
	go func() {
		msg, err := readMessage(bobConn)
		if err != nil {
			log.Printf("Bob failed to read message: %v", err)
			done <- false
			return
		}

		if msg.Type == "message" && msg.From == "alice" {
			fmt.Printf("✓ Bob received: '%s' from %s\n", msg.Content, msg.From)
			done <- true
		} else {
			fmt.Printf("✗ Bob received unexpected message: %+v\n", msg)
			done <- false
		}
	}()

	select {
	case success := <-done:
		if !success {
			log.Fatal("Message delivery failed")
		}
	case <-time.After(5 * time.Second):
		log.Fatal("Timeout waiting for message")
	}

	// Bob sends message to Alice (cross-gateway)
	fmt.Println("\n4. Bob → Alice: 'Hi from Gateway-02!'")
	if err := sendMessage(bobConn, "alice", "Hi from Gateway-02!"); err != nil {
		log.Fatalf("Failed to send message: %v", err)
	}

	// Alice should receive the message
	go func() {
		msg, err := readMessage(aliceConn)
		if err != nil {
			log.Printf("Alice failed to read message: %v", err)
			done <- false
			return
		}

		if msg.Type == "message" && msg.From == "bob" {
			fmt.Printf("✓ Alice received: '%s' from %s\n", msg.Content, msg.From)
			done <- true
		} else {
			fmt.Printf("✗ Alice received unexpected message: %+v\n", msg)
			done <- false
		}
	}()

	select {
	case success := <-done:
		if !success {
			log.Fatal("Message delivery failed")
		}
	case <-time.After(5 * time.Second):
		log.Fatal("Timeout waiting for message")
	}

	fmt.Println("\n=== ✓ All Tests Passed! ===")
	fmt.Println("\nKey Achievements:")
	fmt.Println("  • Alice connected to Gateway-01")
	fmt.Println("  • Bob connected to Gateway-02")
	fmt.Println("  • Messages successfully routed across different gateways")
	fmt.Println("  • Redis Presence and Pub/Sub working correctly")
}

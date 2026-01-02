package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
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

func main() {
	userID := flag.String("user", "", "User ID (required)")
	gatewayURL := flag.String("gateway", "ws://localhost:8080/ws", "Gateway WebSocket URL")
	flag.Parse()

	if *userID == "" {
		log.Fatal("User ID is required (use -user flag)")
	}

	// Connect to gateway
	log.Printf("Connecting to %s as user %s...", *gatewayURL, *userID)

	conn, _, err := websocket.DefaultDialer.Dial(*gatewayURL, nil)
	if err != nil {
		log.Fatalf("Failed to connect: %v", err)
	}
	defer conn.Close()

	log.Println("Connected to gateway")

	// Register user
	registerMsg := ClientMessage{
		Type:   "register",
		UserID: *userID,
	}

	if err := conn.WriteJSON(registerMsg); err != nil {
		log.Fatalf("Failed to register: %v", err)
	}

	// Handle graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Start message receiver
	go receiveMessages(conn)

	// Start heartbeat
	go sendHeartbeat(conn)

	// Start interactive mode
	go interactiveMode(conn, *userID)

	<-sigChan
	log.Println("Shutting down...")
}

func receiveMessages(conn *websocket.Conn) {
	for {
		var msg ServerMessage
		if err := conn.ReadJSON(&msg); err != nil {
			log.Printf("Read error: %v", err)
			return
		}

		switch msg.Type {
		case "registered":
			fmt.Println("\n✓ Successfully registered")
			fmt.Println("\nCommands:")
			fmt.Println("  send <userId> <message>  - Send a message to a user")
			fmt.Println("  quit                      - Exit the client")
			fmt.Print("\n> ")

		case "pong":
			// Heartbeat response, no need to print

		case "message":
			fmt.Printf("\n📨 Message from %s: %s\n> ", msg.From, msg.Content)

		case "error":
			fmt.Printf("\n❌ Error: %s\n> ", msg.Error)

		default:
			fmt.Printf("\n📩 %s: %s\n> ", msg.Type, msg.Content)
		}
	}
}

func sendHeartbeat(conn *websocket.Conn) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		msg := ClientMessage{Type: "ping"}
		if err := conn.WriteJSON(msg); err != nil {
			log.Printf("Failed to send heartbeat: %v", err)
			return
		}
	}
}

func interactiveMode(conn *websocket.Conn, userID string) {
	scanner := bufio.NewScanner(os.Stdin)
	fmt.Print("> ")

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			fmt.Print("> ")
			continue
		}

		parts := strings.SplitN(line, " ", 3)
		command := parts[0]

		switch command {
		case "send":
			if len(parts) < 3 {
				fmt.Println("Usage: send <userId> <message>")
				fmt.Print("> ")
				continue
			}

			to := parts[1]
			content := parts[2]

			msg := ClientMessage{
				Type:    "message",
				To:      to,
				Content: content,
			}

			if err := conn.WriteJSON(msg); err != nil {
				fmt.Printf("Failed to send message: %v\n", err)
			} else {
				fmt.Printf("✓ Message sent to %s\n", to)
			}

		case "quit", "exit":
			os.Exit(0)

		default:
			fmt.Println("Unknown command. Available commands:")
			fmt.Println("  send <userId> <message>")
			fmt.Println("  quit")
		}

		fmt.Print("> ")
	}

	if err := scanner.Err(); err != nil {
		log.Printf("Scanner error: %v", err)
	}
}

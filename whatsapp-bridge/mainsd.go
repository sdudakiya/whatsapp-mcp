package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"

	"encoding/json"
	"net/http"
	"reflect"
	"strings"

	_ "github.com/mattn/go-sqlite3"
	"github.com/mdp/qrterminal"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

var (
	upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
	}
	clients = make(map[*websocket.Conn]bool)
	qrCode  = ""
)

// Broadcast messages to WebSocket clients
func broadcastMessage(message string) {
	for client := range clients {
		err := client.WriteMessage(websocket.TextMessage, []byte(message))
		if err != nil {
			log.Printf("WebSocket error: %v", err)
			client.Close()
			delete(clients, client)
		}
	}
}

// Handle WebSocket connections
func wsHandler(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("WebSocket upgrade error:", err)
		return
	}
	clients[conn] = true

	// Send QR code if available
	if qrCode != "" {
		conn.WriteMessage(websocket.TextMessage, []byte("QR_CODE:" + qrCode))
	}

	defer func() {
		delete(clients, conn)
		conn.Close()
	}()
}

// Serve the HTML page
func serveHTML(w http.ResponseWriter, r *http.Request) {
	htmlContent := `
	<!DOCTYPE html>
	<html lang="en">
	<head>
	    <meta charset="UTF-8">
	    <meta name="viewport" content="width=device-width, initial-scale=1.0">
	    <title>WhatsApp Client</title>
	    <script>
	        var socket = new WebSocket("ws://" + window.location.host + "/ws");
	        socket.onmessage = function(event) {
	            var logDiv = document.getElementById("logs");
	            if (event.data.startsWith("QR_CODE:")) {
	                document.getElementById("qr").innerText = event.data.replace("QR_CODE:", "");
	            } else {
	                logDiv.innerHTML += "<p>" + event.data + "</p>";
	            }
	        };
	    </script>
	</head>
	<body>
	    <h2>WhatsApp Client</h2>
	    <h3>QR Code:</h3>
	    <pre id="qr">Waiting for QR Code...</pre>
	    <h3>Logs:</h3>
	    <div id="logs"></div>
	</body>
	</html>`
	w.Header().Set("Content-Type", "text/html")
	w.Write([]byte(htmlContent))
}

// Start HTTP server
func startHTTPServer() {
	http.HandleFunc("/", serveHTML)
	http.HandleFunc("/ws", wsHandler)

	go func() {
		log.Println("Starting HTTP server on port 8081...")
		if err := http.ListenAndServe(":8081", nil); err != nil {
			log.Fatalf("Failed to start server: %v", err)
		}
	}()
}

// Main function
func main() {
	logger := waLog.Stdout("Client", "INFO", true)
	logger.Infof("Starting WhatsApp client...")

	// Set up database
	dbLog := waLog.Stdout("Database", "INFO", true)
	if err := os.MkdirAll("store", 0755); err != nil {
		logger.Errorf("Failed to create store directory: %v", err)
		return
	}

	container, err := sqlstore.New("sqlite3", "file:store/whatsapp.db?_foreign_keys=on", dbLog)
	if err != nil {
		logger.Errorf("Failed to connect to database: %v", err)
		return
	}

	deviceStore, err := container.GetFirstDevice()
	if err != nil {
		if err == sql.ErrNoRows {
			deviceStore = container.NewDevice()
			logger.Infof("Created new device")
		} else {
			logger.Errorf("Failed to get device: %v", err)
			return
		}
	}

	client := whatsmeow.NewClient(deviceStore, logger)
	if client == nil {
		logger.Errorf("Failed to create WhatsApp client")
		return
	}

	// Initialize message store (assuming you have this function)
	messageStore, err := NewMessageStore()
	if err != nil {
		logger.Errorf("Failed to initialize message store: %v", err)
		return
	}
	defer messageStore.Close()

	// Start HTTP server for WebSocket and QR Code display
	startHTTPServer()

	// Handle WhatsApp events
	client.AddEventHandler(func(evt interface{}) {
		switch v := evt.(type) {
		case *events.Message:
			// Process messages (assuming `handleMessage` exists)
			handleMessage(client, messageStore, v, logger)
			broadcastMessage(fmt.Sprintf("New message received from %s", v.Info.Sender.User))

		case *events.HistorySync:
			// Process history sync events (assuming `handleHistorySync` exists)
			handleHistorySync(client, messageStore, v, logger)
			broadcastMessage("History sync received")

		case *events.Connected:
			logger.Infof("Connected to WhatsApp")
			broadcastMessage("Connected to WhatsApp")

		case *events.LoggedOut:
			logger.Warnf("Device logged out, please scan QR code to log in again")
			broadcastMessage("Device logged out. Please scan the QR code again.")
		}
	})

	// Connection handling
	connected := make(chan bool, 1)
	if client.Store.ID == nil {
		qrChan, _ := client.GetQRChannel(context.Background())
		err = client.Connect()
		if err != nil {
			logger.Errorf("Failed to connect: %v", err)
			return
		}

		for evt := range qrChan {
			if evt.Event == "code" {
				qrCode = evt.Code
				broadcastMessage("QR_CODE:" + evt.Code)
				fmt.Println("\nScan this QR code with your WhatsApp app:")
				qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stdout)
			} else if evt.Event == "success" {
				connected <- true
				break
			}
		}

		select {
		case <-connected:
			broadcastMessage("Successfully connected and authenticated!")
			fmt.Println("\nSuccessfully connected and authenticated!")
		case <-time.After(3 * time.Minute):
			logger.Errorf("Timeout waiting for QR code scan")
			return
		}
	} else {
		err = client.Connect()
		if err != nil {
			logger.Errorf("Failed to connect: %v", err)
			return
		}
		connected <- true
	}

	time.Sleep(2 * time.Second)
	if !client.IsConnected() {
		logger.Errorf("Failed to establish stable connection")
		return
	}

	broadcastMessage("✓ Connected to WhatsApp!")
	fmt.Println("\n✓ Connected to WhatsApp!")

	// Start REST API server (assuming it exists)
	startRESTServer(client, 8081)

	// Wait for termination signal
	exitChan := make(chan os.Signal, 1)
	signal.Notify(exitChan, syscall.SIGINT, syscall.SIGTERM)

	fmt.Println("REST server is running. Press Ctrl+C to disconnect and exit.")
	<-exitChan

	fmt.Println("Disconnecting...")
	client.Disconnect()
}

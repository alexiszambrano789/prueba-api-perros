package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"

	firebase "firebase.google.com/go/v4"
	"firebase.google.com/go/v4/messaging"
	"github.com/gorilla/websocket"
	"google.golang.org/api/option"
)

// --------------------------------------------------
// Types
// --------------------------------------------------

type Client struct {
	conn *websocket.Conn
	send chan []byte
}

type NotificationRequest struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

type TokenRequest struct {
	Token string `json:"token"`
}

// --------------------------------------------------
// Global state
// --------------------------------------------------

var (
	clients    = make(map[*Client]bool)
	pushTokens = make(map[string]bool)
	mu         sync.Mutex
	fcmClient  *messaging.Client
	upgrader   = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}
)

// --------------------------------------------------
// WebSocket handlers
// --------------------------------------------------

func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("Upgrade error:", err)
		return
	}

	client := &Client{conn: conn, send: make(chan []byte, 256)}
	mu.Lock()
	clients[client] = true
	mu.Unlock()

	log.Printf("✅ Nuevo cliente WebSocket conectado. Total: %d\n", len(clients))

	go client.writePump()
	go client.readPump()
}

func (c *Client) readPump() {
	defer func() {
		mu.Lock()
		delete(clients, c)
		mu.Unlock()
		c.conn.Close()
		log.Printf("❌ Cliente desconectado. Total: %d\n", len(clients))
	}()

	for {
		_, message, err := c.conn.ReadMessage()
		if err != nil {
			break
		}
		log.Printf("📩 Mensaje recibido del cliente: %s\n", string(message))

		// Check if it's a token registration
		var tokenReq TokenRequest
		if json.Unmarshal(message, &tokenReq) == nil && tokenReq.Token != "" {
			mu.Lock()
			pushTokens[tokenReq.Token] = true
			mu.Unlock()
			log.Printf("🔑 Push token registrado via WebSocket: %s\n", tokenReq.Token)
		}
	}
}

func (c *Client) writePump() {
	defer c.conn.Close()
	for msg := range c.send {
		if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
			break
		}
	}
}

func broadcast(message []byte) {
	mu.Lock()
	defer mu.Unlock()
	for client := range clients {
		select {
		case client.send <- message:
		default:
			close(client.send)
			delete(clients, client)
		}
	}
}

// --------------------------------------------------
// REST handlers
// --------------------------------------------------

func handleRegisterToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req TokenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	mu.Lock()
	pushTokens[req.Token] = true
	mu.Unlock()

	log.Printf("🔑 Push token registrado: %s\n", req.Token)
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func handleSendNotification(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req NotificationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	log.Printf("📤 Enviando notificación - Título: %s, Cuerpo: %s\n", req.Title, req.Body)

	// 1. Broadcast via WebSocket (foreground)
	msg, _ := json.Marshal(map[string]string{
		"type":  "notification",
		"title": req.Title,
		"body":  req.Body,
	})
	broadcast(msg)
	log.Printf("📡 Enviado por WebSocket a %d clientes\n", len(clients))

	// 2. Send push notification via Firebase (background)
	mu.Lock()
	tokens := make([]string, 0, len(pushTokens))
	for token := range pushTokens {
		tokens = append(tokens, token)
	}
	mu.Unlock()

	if len(tokens) > 0 {
		go sendFirebasePushNotifications(tokens, req.Title, req.Body)
		log.Printf("🚀 Enviando push notification a %d tokens vía FCM\n", len(tokens))
	} else {
		log.Println("⚠️  No hay push tokens registrados")
	}

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":            "ok",
		"websocket_clients": len(clients),
		"push_tokens":       len(tokens),
	})
}

func handleStatus(w http.ResponseWriter, r *http.Request) {
	mu.Lock()
	defer mu.Unlock()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"websocket_clients": len(clients),
		"push_tokens":       len(pushTokens),
	})
}

// --------------------------------------------------
// Firebase Push API
// --------------------------------------------------

func sendFirebasePushNotifications(tokens []string, title, body string) {
	if fcmClient == nil {
		log.Println("❌ FCM Client no está inicializado")
		return
	}

	ctx := context.Background()

	// Firebase Multicast message
	message := &messaging.MulticastMessage{
		Tokens: tokens,
		Notification: &messaging.Notification{
			Title: title,
			Body:  body,
		},
	}

	br, err := fcmClient.SendEachForMulticast(ctx, message)
	if err != nil {
		log.Printf("❌ Error enviando mensaje por Firebase: %v\n", err)
		return
	}
	
	log.Printf("📬 Respuesta de Firebase: %d enviados con éxito, %d fallas\n", br.SuccessCount, br.FailureCount)
	if br.FailureCount > 0 {
		for i, resp := range br.Responses {
			if !resp.Success {
				log.Printf("  ⚠️ Falló el token %s: %v\n", tokens[i], resp.Error)
			}
		}
	}
}

// --------------------------------------------------
// CORS middleware
// --------------------------------------------------

func corsMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}

		next(w, r)
	}
}

// --------------------------------------------------
// Firebase Initialization
// --------------------------------------------------

func initFirebase() {
	ctx := context.Background()
	opt := option.WithCredentialsFile("prueba-perros-23231-firebase-adminsdk-fbsvc-0558e06804.json")
	app, err := firebase.NewApp(ctx, nil, opt)
	if err != nil {
		log.Fatalf("❌ Error inicializando Firebase app: %v\n", err)
	}

	client, err := app.Messaging(ctx)
	if err != nil {
		log.Fatalf("❌ Error obteniendo cliente FCM: %v\n", err)
	}
	fcmClient = client
	log.Println("✅ Firebase Admin SDK inicializado correctamente")
}

// --------------------------------------------------
// Main
// --------------------------------------------------

func main() {
	initFirebase()

	http.HandleFunc("/ws", handleWebSocket)
	http.HandleFunc("/register-token", corsMiddleware(handleRegisterToken))
	http.HandleFunc("/send-notification", corsMiddleware(handleSendNotification))
	http.HandleFunc("/status", corsMiddleware(handleStatus))

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	addr := ":" + port

	fmt.Println("===========================================")
	fmt.Println("  🐕 API Prueba Perros - Notificaciones")
	fmt.Println("===========================================")
	fmt.Printf("  Servidor escuchando en el puerto: %s\n", port)
	fmt.Printf("  WebSocket:          ws://localhost:%s/ws\n", port)
	fmt.Printf("  Send Notification:  POST http://localhost:%s/send-notification\n", port)
	fmt.Printf("  Register Token:     POST http://localhost:%s/register-token\n", port)
	fmt.Printf("  Status:             GET  http://localhost:%s/status\n", port)
	fmt.Println("===========================================")

	log.Fatal(http.ListenAndServe(addr, nil))
}

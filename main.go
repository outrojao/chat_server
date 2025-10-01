package main

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type Message struct {
	Name      string         `json:"name"`
	Message   string         `json:"message"`
	Type      string         `json:"type,omitempty"` // "message", "user_count", "reaction", "timeout_vote", "timeout_result"
	Count     int            `json:"count,omitempty"`
	ID        string         `json:"id,omitempty"`        // ID único da mensagem
	Reactions map[string]int `json:"reactions,omitempty"` // "like" e "dislike"
	ReactTo   string         `json:"reactTo,omitempty"`   // ID da mensagem para reagir
	Reaction  string         `json:"reaction,omitempty"`  // "like" ou "dislike"
	Timestamp int64          `json:"timestamp,omitempty"`
	// Campos para timeout
	TargetUser     string `json:"targetUser,omitempty"`
	TimeoutMinutes int    `json:"timeoutMinutes,omitempty"`
	VoteEndTime    int64  `json:"voteEndTime,omitempty"`
}

type Client struct {
	conn     *websocket.Conn
	username string
}

var clients = make(map[*websocket.Conn]*Client)
var broadcast = make(chan Message)
var mutex = &sync.Mutex{}
var messages = make(map[string]*Message)               // Armazenar mensagens por ID
var messageReactions = make(map[string]map[string]int) // ID da mensagem -> tipo reação -> count
var timeoutUsers = make(map[string]time.Time)          // Usuários em timeout
var timeoutVotes = make(map[string]*Message)           // Votos de timeout ativos

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

func generateMessageID() string {
	return fmt.Sprintf("%d", time.Now().UnixNano())
}

func getUniqueUsers() map[string]bool {
	uniqueUsers := make(map[string]bool)
	for _, client := range clients {
		if client.username != "" {
			uniqueUsers[client.username] = true
		}
	}
	return uniqueUsers
}

func isUserTimedOut(username string) bool {
	if timeoutEnd, exists := timeoutUsers[username]; exists {
		if time.Now().Before(timeoutEnd) {
			return true
		} else {
			delete(timeoutUsers, username)
			return false
		}
	}
	return false
}

func processTimeoutCommand(msg Message) *Message {
	parts := strings.Split(msg.Message, " ")
	if len(parts) < 3 {
		return nil
	}

	targetUser := parts[1]
	minutesStr := parts[2]
	minutes, err := strconv.Atoi(minutesStr)
	if err != nil || minutes <= 0 || minutes > 60 {
		return nil
	}

	// Não permitir timeout em si mesmo
	if targetUser == msg.Name {
		return nil
	}

	timeoutMsg := Message{
		Name:           msg.Name,
		Message:        fmt.Sprintf("Votação para timeout de %s por %d minuto(s). Vote com 👍 (aprovar) ou 🍅 (rejeitar)", targetUser, minutes),
		Type:           "timeout_vote",
		ID:             generateMessageID(),
		Timestamp:      time.Now().Unix(),
		Reactions:      make(map[string]int),
		TargetUser:     targetUser,
		TimeoutMinutes: minutes,
		VoteEndTime:    time.Now().Add(2 * time.Minute).Unix(), // 2 minutos para votar
	}

	mutex.Lock()
	messages[timeoutMsg.ID] = &timeoutMsg
	messageReactions[timeoutMsg.ID] = make(map[string]int)
	timeoutVotes[timeoutMsg.ID] = &timeoutMsg
	mutex.Unlock()

	// Agendar fim da votação
	go func() {
		time.Sleep(2 * time.Minute)
		checkTimeoutVote(timeoutMsg.ID)
	}()

	return &timeoutMsg
}

func checkTimeoutVote(voteID string) {
	mutex.Lock()
	defer mutex.Unlock()

	vote, exists := timeoutVotes[voteID]
	if !exists {
		return
	}

	reactions := messageReactions[voteID]
	likes := reactions["like"]
	dislikes := reactions["dislike"]

	// Calcular quantos usuários únicos podem votar (total - quem iniciou - alvo)
	uniqueUsers := getUniqueUsers()
	totalUsers := len(uniqueUsers)
	eligibleVoters := max(
		// Removes the initiator and the target
		totalUsers-2,
		// At least 1 person must be able to vote
		1)

	// Needs a majority of eligible voters AND at least 1 vote
	requiredVotes := max((eligibleVoters/2)+1, 1)

	var resultMsg Message
	if likes > dislikes && likes >= requiredVotes {
		// Apply timeout
		timeoutEnd := time.Now().Add(time.Duration(vote.TimeoutMinutes) * time.Minute)
		timeoutUsers[vote.TargetUser] = timeoutEnd

		resultMsg = Message{
			Type:      "timeout_result",
			Message:   fmt.Sprintf("⚠️ %s foi colocado em timeout por %d minutos! (Votos: 👍%d 🍅%d | Necessário: %d)", vote.TargetUser, vote.TimeoutMinutes, likes, dislikes, requiredVotes),
			ID:        generateMessageID(),
			Timestamp: time.Now().Unix(),
		}
	} else {
		resultMsg = Message{
			Type:      "timeout_result",
			Message:   fmt.Sprintf("❌ Timeout de %s foi rejeitado. (Votos: 👍%d 🍅%d | Necessário: %d)", vote.TargetUser, likes, dislikes, requiredVotes),
			ID:        generateMessageID(),
			Timestamp: time.Now().Unix(),
		}
	}

	// Broadcast do resultado
	for conn := range clients {
		err := conn.WriteJSON(resultMsg)
		if err != nil {
			conn.Close()
			delete(clients, conn)
		}
	}

	delete(timeoutVotes, voteID)
}

func broadcastUserCount() {
	mutex.Lock()
	uniqueUsers := getUniqueUsers()
	count := len(uniqueUsers)

	countMsg := Message{
		Type:  "user_count",
		Count: count,
	}

	for conn := range clients {
		err := conn.WriteJSON(countMsg)
		if err != nil {
			conn.Close()
			delete(clients, conn)
		}
	}
	mutex.Unlock()
}

func handleConnections(w http.ResponseWriter, r *http.Request) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		fmt.Println("Erro ao fazer upgrade:", err)
		return
	}
	defer ws.Close()

	mutex.Lock()
	clients[ws] = &Client{conn: ws, username: ""}
	mutex.Unlock()

	// Enviar contagem inicial
	broadcastUserCount()

	for {
		var msg Message
		err := ws.ReadJSON(&msg)
		if err != nil {
			mutex.Lock()
			delete(clients, ws)
			mutex.Unlock()
			broadcastUserCount()
			break
		}

		// Atualizar username do cliente se ainda não foi definido
		mutex.Lock()
		if clients[ws].username == "" {
			clients[ws].username = msg.Name
		}
		mutex.Unlock()

		// Enviar contagem atualizada quando o username for definido
		broadcastUserCount()

		// Verificar se usuário está em timeout
		if isUserTimedOut(msg.Name) {
			timeoutMsg := Message{
				Type:      "timeout_result",
				Message:   fmt.Sprintf("⚠️ %s está em timeout e não pode enviar mensagens!", msg.Name),
				ID:        generateMessageID(),
				Timestamp: time.Now().Unix(),
			}
			ws.WriteJSON(timeoutMsg)
			continue
		}

		if msg.Type == "reaction" {
			handleReaction(msg)
		} else if msg.Type == "user_register" {
			// Apenas registra o usuário, não envia mensagem
			continue
		} else {
			// Verificar comando /timeout
			if strings.HasPrefix(msg.Message, "/timeout ") {
				timeoutMsg := processTimeoutCommand(msg)
				if timeoutMsg != nil {
					broadcast <- *timeoutMsg
				}
			} else {
				// Mensagem normal - só processar se não for vazia
				if msg.Message != "" {
					msg.Type = "message"
					msg.ID = generateMessageID()
					msg.Timestamp = time.Now().Unix()
					msg.Reactions = make(map[string]int)

					mutex.Lock()
					messages[msg.ID] = &msg
					messageReactions[msg.ID] = make(map[string]int)
					mutex.Unlock()

					broadcast <- msg
				}
			}
		}
	}
}

func handleReaction(reaction Message) {
	mutex.Lock()
	defer mutex.Unlock()

	if reactions, exists := messageReactions[reaction.ReactTo]; exists {
		reactions[reaction.Reaction]++

		// Broadcast da reação atualizada
		reactionMsg := Message{
			Type:      "reaction",
			ReactTo:   reaction.ReactTo,
			Reactions: make(map[string]int),
		}

		for k, v := range reactions {
			reactionMsg.Reactions[k] = v
		}

		for conn := range clients {
			err := conn.WriteJSON(reactionMsg)
			if err != nil {
				conn.Close()
				delete(clients, conn)
			}
		}
	}
}

func handleMessages() {
	for {
		msg := <-broadcast
		mutex.Lock()
		for conn := range clients {
			err := conn.WriteJSON(msg)
			if err != nil {
				conn.Close()
				delete(clients, conn)
			}
		}
		mutex.Unlock()
	}
}

func main() {
	fs := http.FileServer(http.Dir("./"))
	http.Handle("/", fs)

	http.HandleFunc("/ws", handleConnections)

	go handleMessages()

	fmt.Println("Servidor iniciado em :8080")
	err := http.ListenAndServe("0.0.0.0:8080", nil)
	if err != nil {
		fmt.Println("Erro no servidor:", err)
	}
}

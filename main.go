package main

import (
	"encoding/json"
	"log"
	"math/rand"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/websocket/v2"
)

type Player struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Score int             `json:"score"`
	Role  string          `json:"role"`
	Conn  *websocket.Conn `json:"-"`
}

type Room struct {
	Code      string             `json:"code"`
	State     string             `json:"state"`
	HostID    string             `json:"hostId"`
	JudgeID   string             `json:"judgeId"`
	InsiderID string             `json:"insiderId"`
	Timer     int                `json:"timer"`

	SecretWord string `json:"secretWord,omitempty"`

	Players map[string]*Player `json:"players"`
	Votes map[string]string `json:"-"`

	timerRunning bool
	timerCancel  chan struct{}

	mu sync.Mutex
}

type OutgoingRoomMessage struct {
	Type   string `json:"type"`
	SelfID string `json:"selfId,omitempty"`
	Room   *Room  `json:"room"`
}

type ErrorMessage struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

type ClientMessage struct {
	Type       string `json:"type"`
	TargetID   string `json:"targetId,omitempty"`  
	Duration   int    `json:"duration,omitempty"`  
	SuspectID  string `json:"suspectId,omitempty"` 
	SecretWord string `json:"secretWord,omitempty"` 
}

const (
	RoundDurationSeconds = 300 // เวลา phase 
	VoteDurationSeconds  = 90  // เวลา phase 
)

var (
	rooms   = make(map[string]*Room)
	roomsMu sync.Mutex
)

func init() {
	rand.Seed(time.Now().UnixNano())
}

func getOrCreateRoom(code string, create bool) (*Room, bool) {
	roomsMu.Lock()
	defer roomsMu.Unlock()

	if room, ok := rooms[code]; ok {
		return room, true
	}
	if !create {
		return nil, false
	}

	room := &Room{
		Code:    code,
		State:   "lobby",
		Players: make(map[string]*Player),
		Votes:   make(map[string]string),
	}
	rooms[code] = room
	return room, true
}

func deleteRoomIfEmpty(room *Room) {
	roomsMu.Lock()
	defer roomsMu.Unlock()

	if len(room.Players) == 0 {
		delete(rooms, room.Code)
	}
}

func makePlayerID() string {
	return strconv.FormatInt(time.Now().UnixNano(), 36) + "-" + strconv.Itoa(rand.Intn(100000))
}

func broadcastRoom(room *Room) {
	room.mu.Lock()
	defer room.mu.Unlock()

	snap := &Room{
		Code:       room.Code,
		State:      room.State,
		HostID:     room.HostID,
		JudgeID:    room.JudgeID,
		InsiderID:  room.InsiderID,
		Timer:      room.Timer,
		SecretWord: room.SecretWord,
		Players:    make(map[string]*Player),
	}

	for id, p := range room.Players {
		snap.Players[id] = &Player{
			ID:    p.ID,
			Name:  p.Name,
			Score: p.Score,
			Role:  p.Role,
		}
	}

	for _, p := range room.Players {
		if p.Conn == nil {
			continue
		}
		msg := OutgoingRoomMessage{
			Type: "room",
			Room: snap,
		}
		_ = p.Conn.WriteJSON(msg)
	}
}

func sendRoomToPlayer(room *Room, player *Player) {
	room.mu.Lock()
	defer room.mu.Unlock()

	snap := &Room{
		Code:      room.Code,
		State:     room.State,
		HostID:    room.HostID,
		JudgeID:   room.JudgeID,
		InsiderID: room.InsiderID,
		Timer:     room.Timer,
		Players:   make(map[string]*Player),
	}

	for id, p := range room.Players {
		snap.Players[id] = &Player{
			ID:    p.ID,
			Name:  p.Name,
			Score: p.Score,
			Role:  p.Role,
		}
	}

	msg := OutgoingRoomMessage{
		Type:   "room",
		SelfID: player.ID,
		Room:   snap,
	}
	_ = player.Conn.WriteJSON(msg)
}

func sendError(conn *websocket.Conn, text string) {
	_ = conn.WriteJSON(ErrorMessage{
		Type:    "error",
		Message: text,
	})
}

// แจก role ให้ทุกคน + random insider (ไม่นับกรรมการ)
func assignRoles(room *Room) {
	room.mu.Lock()
	defer room.mu.Unlock()
	room.InsiderID = ""
	for _, p := range room.Players {
		p.Role = "normal"
	}
	if room.JudgeID != "" {
		if j, ok := room.Players[room.JudgeID]; ok {
			j.Role = "judge"
		}
	}

	candidates := make([]*Player, 0)
	for _, p := range room.Players {
		if p.ID == room.JudgeID {
			continue
		}
		candidates = append(candidates, p)
	}
	if len(candidates) == 0 {
		return
	}

	ins := candidates[rand.Intn(len(candidates))]
	room.InsiderID = ins.ID
	ins.Role = "insider"

	room.State = "assign_roles"
}


func startCountdownTimer(room *Room, duration int) {
	room.mu.Lock()
	if room.timerCancel != nil {
		close(room.timerCancel)
	}
	room.Timer = duration
	room.State = "countdown"
	room.timerRunning = true
	room.timerCancel = make(chan struct{})
	cancelChan := room.timerCancel
	room.mu.Unlock()

	go func(r *Room, cancel <-chan struct{}) {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				r.mu.Lock()
				if !r.timerRunning {
					r.mu.Unlock()
					return
				}
				if r.Timer > 0 {
					r.Timer--
				}
				if r.Timer <= 0 {
					r.Timer = 0
					r.timerRunning = false
					r.State = "voting"
					r.Votes = make(map[string]string)
					r.mu.Unlock()
					broadcastRoom(r)
					startVoteTimer(r, VoteDurationSeconds)
					return
				}
				r.mu.Unlock()
				broadcastRoom(r)
			case <-cancel:
				return
			}
		}
	}(room, cancelChan)
}


func startVoteTimer(room *Room, duration int) {
	room.mu.Lock()
	if room.timerCancel != nil {
		close(room.timerCancel)
	}
	room.Timer = duration
	room.State = "voting"
	room.timerRunning = true
	room.timerCancel = make(chan struct{})
	cancelChan := room.timerCancel
	room.mu.Unlock()

	go func(r *Room, cancel <-chan struct{}) {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				r.mu.Lock()
				if !r.timerRunning {
					r.mu.Unlock()
					return
				}
				if r.Timer > 0 {
					r.Timer--
				}
				if r.Timer <= 0 {
					r.Timer = 0
					r.timerRunning = false
					r.mu.Unlock()
					handleTallyVotes(r)
					broadcastRoom(r)
					return
				}
				r.mu.Unlock()
				broadcastRoom(r)
			case <-cancel:
				return
			}
		}
	}(room, cancelChan)
}

func handleGuessCorrect(room *Room) {
	room.mu.Lock()
	if room.timerRunning {
		room.timerRunning = false
		if room.timerCancel != nil {
			close(room.timerCancel)
			room.timerCancel = nil
		}
	}


	for _, p := range room.Players {
		if p.ID == room.InsiderID || p.ID == room.JudgeID {
			continue
		}
		p.Score++
	}

	room.mu.Unlock()
	startVoteTimer(room, VoteDurationSeconds)
	broadcastRoom(room)
}


func handleTallyVotes(room *Room) {
	room.mu.Lock()
	defer room.mu.Unlock()

	if len(room.Players) == 0 {
		return
	}

	count := make(map[string]int)
	for _, suspectID := range room.Votes {
		count[suspectID]++
	}

	if len(count) == 0 {
		room.State = "scoreboard"
		return
	}

	var votedID string
	maxVote := -1
	for id, c := range count {
		if c > maxVote {
			maxVote = c
			votedID = id
		}
	}

	isCorrect := votedID == room.InsiderID

	if isCorrect {
		for _, p := range room.Players {
			if p.ID == room.InsiderID || p.ID == room.JudgeID {
				continue
			}
			p.Score++
		}
	} else {
		if ins, ok := room.Players[room.InsiderID]; ok {
			ins.Score += 2
		}
	}

	room.State = "scoreboard"
	room.Votes = make(map[string]string)
}

func handleNextRound(room *Room) {
	room.mu.Lock()
	defer room.mu.Unlock()
	for _, p := range room.Players {
		p.Role = ""
	}
	room.InsiderID = ""
	room.SecretWord = ""
	room.Timer = 0
	room.timerRunning = false
	if room.timerCancel != nil {
		close(room.timerCancel)
		room.timerCancel = nil
	}
	room.State = "lobby"
	room.Votes = make(map[string]string)
}

func wsHandler(c *websocket.Conn) {
	roomCode := c.Query("room")
	playerName := c.Query("name")
	mode := c.Query("mode")

	if roomCode == "" || playerName == "" {
		sendError(c, "missing room or name")
		_ = c.Close()
		return
	}

	create := mode == "create"
	room, ok := getOrCreateRoom(roomCode, create)
	if !ok || room == nil {
		sendError(c, "room not found")
		_ = c.Close()
		return
	}

	playerID := makePlayerID()
	player := &Player{
		ID:    playerID,
		Name:  playerName,
		Score: 0,
		Role:  "",
		Conn:  c,
	}

	room.mu.Lock()
	room.Players[playerID] = player
	if room.HostID == "" {
		room.HostID = playerID
	}
	room.mu.Unlock()

	sendRoomToPlayer(room, player)
	broadcastRoom(room)

	log.Printf("[WS] %s joined room %s (mode=%s)\n", playerName, roomCode, mode)

	defer func() {
		log.Printf("[WS] %s disconnected from room %s\n", playerName, roomCode)

		room.mu.Lock()
		delete(room.Players, playerID)
		if room.HostID == playerID {
			room.HostID = ""
			for id := range room.Players {
				room.HostID = id
				break
			}
		}
		if room.JudgeID == playerID {
			room.JudgeID = ""
		}
		room.mu.Unlock()

		broadcastRoom(room)
		deleteRoomIfEmpty(room)
	}()

	for {
		_, data, err := c.ReadMessage()
		if err != nil {
			return
		}
		var msg ClientMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			sendError(c, "invalid message format")
			continue
		}

		switch msg.Type {
		case "set_judge":
			room.mu.Lock()
			if _, ok := room.Players[msg.TargetID]; ok {
				room.JudgeID = msg.TargetID
			}
			room.mu.Unlock()
			broadcastRoom(room)

		case "start_round":
			if msg.Duration <= 0 {
				msg.Duration = RoundDurationSeconds
			}

			room.mu.Lock()
			totalPlayers := len(room.Players)
			hasJudge := room.JudgeID != ""
			nonJudgeCount := totalPlayers
			if hasJudge {
				nonJudgeCount = totalPlayers - 1
			}

			room.SecretWord = msg.SecretWord
			room.mu.Unlock()

			if msg.SecretWord == "" {
				sendError(c, "กรรมการต้องกำหนดคำปริศนาก่อนเริ่มเกม")
				continue
			}
			if !hasJudge || nonJudgeCount < 3 || nonJudgeCount%2 == 0 {
				sendError(c, "ต้องมีผู้เล่น (ไม่นับกรรมการ) อย่างน้อย 3 คน และเป็นจำนวนคี่")
				continue
			}

			assignRoles(room)
			broadcastRoom(room)
			startCountdownTimer(room, msg.Duration)

		case "guess_correct":
			room.mu.Lock()
			isJudge := room.JudgeID == playerID
			room.mu.Unlock()
			if !isJudge {
				sendError(c, "เฉพาะกรรมการเท่านั้นที่กดทายถูกได้")
				continue
			}
			handleGuessCorrect(room)

				case "vote_insider":
			if msg.SuspectID == "" {
				sendError(c, "suspectId is required")
				continue
			}

			room.mu.Lock()

			if room.State != "voting" {
				room.mu.Unlock()
				sendError(c, "ยังไม่อยู่ในช่วงโหวต")
				continue
			}

			if playerID == room.JudgeID {
				room.mu.Unlock()
				sendError(c, "กรรมการไม่สามารถโหวตได้")
				continue
			}

			if msg.SuspectID == playerID {
				room.mu.Unlock()
				sendError(c, "ไม่สามารถโหวตตัวเองได้")
				continue
			}


			if _, ok := room.Players[msg.SuspectID]; !ok {
				room.mu.Unlock()
				sendError(c, "invalid suspectId")
				continue
			}

			if room.Votes == nil {
				room.Votes = make(map[string]string)
			}
			room.Votes[playerID] = msg.SuspectID

			expectedVotes := len(room.Players)
			if room.JudgeID != "" {
				expectedVotes = len(room.Players) - 1
			}

			if len(room.Votes) >= expectedVotes && expectedVotes > 0 {
				if room.timerRunning {
					room.timerRunning = false
					if room.timerCancel != nil {
						close(room.timerCancel)
						room.timerCancel = nil
					}
				}
				room.mu.Unlock()
				handleTallyVotes(room)
				broadcastRoom(room)
			} else {
				room.mu.Unlock()
				broadcastRoom(room)
			}

		case "next_round":
			handleNextRound(room)
			broadcastRoom(room)

		default:
			sendError(c, "unknown message type: "+msg.Type)
		}
	}
}

func main() {
	app := fiber.New()
	app.Get("/ws", websocket.New(wsHandler))

	port := os.Getenv("PORT")
	if port == "" {
		port = "3001"
	}

	log.Println("Go WebSocket server running on port", port)
	if err := app.Listen(":" + port); err != nil {
		log.Fatal(err)
	}
}

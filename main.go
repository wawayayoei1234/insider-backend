package main

import (
	"encoding/json"
	"log"
	"math/rand"
	"os"
	"strconv"
	"strings"
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
	Code      string `json:"code"`
	State     string `json:"state"`
	HostID    string `json:"hostId"`
	JudgeID   string `json:"judgeId"`
	InsiderID string `json:"insiderId"`
	Timer     int    `json:"timer"`

	SecretWord        string `json:"secretWord,omitempty"`
	RoundEndByTimeout bool   `json:"roundEndByTimeout"`
	ChatEnabled       bool   `json:"chatEnabled"`

	BlockedVoters map[string]bool `json:"blockedVoters,omitempty"`
	Voted         map[string]bool `json:"voted,omitempty"`
	LastVotes     []VotePair      `json:"lastVotes,omitempty"`

	Players map[string]*Player `json:"players"`
	Votes   map[string]string  `json:"-"`

	timerRunning bool
	timerCancel  chan struct{}

	mu sync.Mutex
}

type VotePair struct {
	VoterID  string `json:"voterId"`
	TargetID string `json:"targetId"`
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
	Type        string `json:"type"`
	TargetID    string `json:"targetId,omitempty"`
	Duration    int    `json:"duration,omitempty"`
	SuspectID   string `json:"suspectId,omitempty"`
	SecretWord  string `json:"secretWord,omitempty"`
	Text        string `json:"text,omitempty"`
	ChatEnabled *bool  `json:"chatEnabled,omitempty"`
}

type ChatFrom struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type ChatPayload struct {
	Type string   `json:"type"`
	From ChatFrom `json:"from"`
	Text string   `json:"text"`
	Ts   int64    `json:"ts"`
}

const (
	RoundDurationSeconds = 300
	VoteDurationSeconds  = 90
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
		if create {
			return nil, false
		}
		return room, true
	}

	if !create {
		return nil, false
	}

	room := &Room{
		Code:          code,
		State:         "lobby",
		Players:       make(map[string]*Player),
		Votes:         make(map[string]string),
		BlockedVoters: make(map[string]bool),
		Voted:         make(map[string]bool),
		LastVotes:     []VotePair{},
		ChatEnabled:   true,
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
		Code:              room.Code,
		State:             room.State,
		HostID:            room.HostID,
		JudgeID:           room.JudgeID,
		InsiderID:         room.InsiderID,
		Timer:             room.Timer,
		SecretWord:        room.SecretWord,
		RoundEndByTimeout: room.RoundEndByTimeout,
		ChatEnabled:       room.ChatEnabled,

		BlockedVoters: make(map[string]bool),
		Voted:         make(map[string]bool),
		LastVotes:     append([]VotePair(nil), room.LastVotes...),
		Players:       make(map[string]*Player),
	}

	for id, b := range room.BlockedVoters {
		snap.BlockedVoters[id] = b
	}
	for id, v := range room.Voted {
		snap.Voted[id] = v
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
		Code:              room.Code,
		State:             room.State,
		HostID:            room.HostID,
		JudgeID:           room.JudgeID,
		InsiderID:         room.InsiderID,
		Timer:             room.Timer,
		SecretWord:        room.SecretWord,
		RoundEndByTimeout: room.RoundEndByTimeout,
		ChatEnabled:       room.ChatEnabled,

		BlockedVoters: make(map[string]bool),
		Voted:         make(map[string]bool),
		LastVotes:     append([]VotePair(nil), room.LastVotes...),
		Players:       make(map[string]*Player),
	}

	for id, b := range room.BlockedVoters {
		snap.BlockedVoters[id] = b
	}
	for id, v := range room.Voted {
		snap.Voted[id] = v
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

	room.RoundEndByTimeout = false
	room.BlockedVoters = make(map[string]bool)
	room.Voted = make(map[string]bool)
	room.LastVotes = []VotePair{}

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
					// เวลาหมด → ไม่มีใครทายถูก → ทุกคนแพ้
					r.Timer = 0
					r.timerRunning = false
					r.State = "scoreboard"
					r.RoundEndByTimeout = true
					r.Votes = make(map[string]string)
					r.mu.Unlock()
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
	// ทายถูก → ไป phase โหวต (คะแนนไปตัดสินที่ handleTallyVotes)
	room.RoundEndByTimeout = false
	room.State = "voting"
	room.Votes = make(map[string]string)
	room.Voted = make(map[string]bool)
	room.BlockedVoters = make(map[string]bool)
	room.LastVotes = []VotePair{}
	room.mu.Unlock()

	broadcastRoom(room)
	startVoteTimer(room, VoteDurationSeconds)
}

func handleTallyVotes(room *Room) {
	room.mu.Lock()
	defer room.mu.Unlock()

	if len(room.Players) == 0 {
		return
	}

	// เก็บประวัติว่าใครโหวตใคร
	lastVotes := make([]VotePair, 0, len(room.Votes))
	for voterID, targetID := range room.Votes {
		lastVotes = append(lastVotes, VotePair{
			VoterID:  voterID,
			TargetID: targetID,
		})
	}
	room.LastVotes = lastVotes

	// นับคะแนน
	count := make(map[string]int)
	for _, suspectID := range room.Votes {
		count[suspectID]++
	}

	if len(count) == 0 {
		// ไม่มีใครโหวต → จบรอบ แบบไม่มีใครได้แต้มเพิ่ม
		room.State = "scoreboard"
		room.Votes = make(map[string]string)
		room.Voted = make(map[string]bool)
		room.BlockedVoters = make(map[string]bool)
		return
	}

	// หา max vote
	maxVote := -1
	for _, c := range count {
		if c > maxVote {
			maxVote = c
		}
	}

	// คนที่ได้คะแนนสูงสุด
	top := []string{}
	for id, c := range count {
		if c == maxVote {
			top = append(top, id)
		}
	}

	// เสมอ → โหวตรอบใหม่ โดย "ผู้ต้องสงสัยที่คะแนนเท่ากัน" ถูก block ไม่ให้โหวต
	if len(top) > 1 {
		room.State = "voting"
		room.Votes = make(map[string]string)
		room.Voted = make(map[string]bool)

		room.BlockedVoters = make(map[string]bool)
		for _, id := range top {
			room.BlockedVoters[id] = true
		}
		return
	}

	// มีผู้ถูกโหวตชัดเจน
	votedID := top[0]
	isCorrect := votedID == room.InsiderID

	if isCorrect {
		// โหวตโดน Insider → คนทั่วไปชนะ (ไม่รวม Insider / Judge)
		for _, p := range room.Players {
			if p.ID == room.InsiderID || p.ID == room.JudgeID {
				continue
			}
			p.Score++
		}
	} else {
		// โหวตผิด → Insider ชนะคนเดียว
		if ins, ok := room.Players[room.InsiderID]; ok {
			ins.Score += 2 // จะปรับเป็น 1 แต้มก็ได้
		}
	}

	room.State = "scoreboard"
	room.Votes = make(map[string]string)
	room.Voted = make(map[string]bool)
	room.BlockedVoters = make(map[string]bool)
}

func handleNextRound(room *Room) {
	room.mu.Lock()
	defer room.mu.Unlock()

	for _, p := range room.Players {
		p.Role = ""
	}
	room.InsiderID = ""
	room.Timer = 0
	room.timerRunning = false
	if room.timerCancel != nil {
		close(room.timerCancel)
		room.timerCancel = nil
	}
	room.State = "lobby"
	room.Votes = make(map[string]string)
	room.RoundEndByTimeout = false
	room.BlockedVoters = make(map[string]bool)
	room.Voted = make(map[string]bool)
	room.LastVotes = []VotePair{}
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
		if create {
			sendError(c, "ห้องนี้มีอยู่แล้ว กรุณาใช้รหัสห้องอื่น หรือกดเข้าห้องแทน")
		} else {
			sendError(c, "room not found")
		}
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

		case "set_chat_enabled":
			if msg.ChatEnabled == nil {
				sendError(c, "chatEnabled is required")
				continue
			}
			room.mu.Lock()
			if room.HostID != playerID {
				room.mu.Unlock()
				sendError(c, "เฉพาะ Host เท่านั้นที่ตั้งค่าแชทได้")
				continue
			}
			room.ChatEnabled = *msg.ChatEnabled
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
			if !hasJudge || nonJudgeCount < 3 {
				sendError(c, "ต้องมีผู้เล่น (ไม่นับกรรมการ) อย่างน้อย 3 คน")
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

			if room.BlockedVoters != nil && room.BlockedVoters[playerID] {
				room.mu.Unlock()
				sendError(c, "คุณอยู่ในกลุ่มที่ถูกสงสัย จึงไม่มีสิทธิ์โหวตรอบนี้")
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

			// mark คนนี้ว่าโหวตแล้ว (ให้ front-end ใช้โชว์)
			if room.Voted == nil {
				room.Voted = make(map[string]bool)
			}
			room.Voted[playerID] = true

			// คำนวณจำนวน "คนที่มีสิทธิ์โหวตจริง ๆ"
			eligible := 0
			for id := range room.Players {
				if id == room.JudgeID {
					continue
				}
				if room.BlockedVoters != nil && room.BlockedVoters[id] {
					continue
				}
				eligible++
			}

			if len(room.Votes) >= eligible && eligible > 0 {
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

		case "kick":
			room.mu.Lock()

			if room.HostID != playerID {
				room.mu.Unlock()
				sendError(c, "เฉพาะ Host เท่านั้นที่เตะผู้เล่นได้")
				continue
			}

			if msg.TargetID == "" {
				room.mu.Unlock()
				sendError(c, "targetId is required")
				continue
			}

			if msg.TargetID == room.HostID {
				room.mu.Unlock()
				sendError(c, "ไม่สามารถเตะตัวเองได้")
				continue
			}

			target, ok := room.Players[msg.TargetID]
			if !ok {
				room.mu.Unlock()
				sendError(c, "ผู้เล่นที่ต้องการเตะไม่อยู่ในห้องแล้ว")
				continue
			}

			if room.JudgeID == msg.TargetID {
				room.JudgeID = ""
			}

			delete(room.Players, msg.TargetID)
			room.mu.Unlock()

			if target.Conn != nil {
				_ = target.Conn.WriteJSON(ErrorMessage{
					Type:    "error",
					Message: "คุณถูกเชิญออกจากห้องโดย Host",
				})
				_ = target.Conn.Close()
			}

			broadcastRoom(room)
			deleteRoomIfEmpty(room)

		case "chat":
			txt := strings.TrimSpace(msg.Text)
			if txt == "" {
				continue
			}
			if len(txt) > 300 {
				txt = txt[:300]
			}

			room.mu.Lock()
			enabled := room.ChatEnabled
			sender, ok := room.Players[playerID]
			room.mu.Unlock()

			if !ok || sender == nil {
				continue
			}

			if !enabled {
				sendError(c, "ตอนนี้ Host ปิดแชทอยู่")
				continue
			}

			payload := ChatPayload{
				Type: "chat",
				From: ChatFrom{
					ID:   sender.ID,
					Name: sender.Name,
				},
				Text: txt,
				Ts:   time.Now().Unix(),
			}

			room.mu.Lock()
			for _, p := range room.Players {
				if p.Conn == nil {
					continue
				}
				_ = p.Conn.WriteJSON(payload)
			}
			room.mu.Unlock()

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

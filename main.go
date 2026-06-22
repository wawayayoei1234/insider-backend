package main

import (
	"bufio"
	"encoding/json"
	"log"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	rtctokenbuilder "github.com/AgoraIO/Tools/DynamicKey/AgoraDynamicKey/go/src/rtctokenbuilder2"
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/cors"
	"github.com/gofiber/websocket/v2"
)

type Player struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Score     int             `json:"score"`
	Role      string          `json:"role"`
	Connected bool            `json:"connected"`
	Spectator bool            `json:"spectator,omitempty"`
	Token     string          `json:"-"`
	Conn      *websocket.Conn `json:"-"`
	lastChat  time.Time
	lastReact time.Time
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

	CorrectGuesserID string `json:"correctGuesserId,omitempty"`

	Hints         []string `json:"hints,omitempty"`         // T8: ใบ้สาธารณะจากกรรมการ
	QuestionCount int      `json:"questionCount,omitempty"` // T8: ตัวนับจำนวนคำถาม

	BlockedVoters map[string]bool `json:"blockedVoters,omitempty"`
	Voted         map[string]bool `json:"voted,omitempty"`
	LastVotes     []VotePair      `json:"lastVotes,omitempty"`

	Players map[string]*Player `json:"players"`
	Votes   map[string]string  `json:"-"`

	timerRunning      bool
	timerCancel       chan struct{}
	voteTimerDuration int

	mu sync.Mutex
}

type VotePair struct {
	VoterID  string `json:"voterId"`
	TargetID string `json:"targetId"`
}

type OutgoingRoomMessage struct {
	Type       string   `json:"type"`
	SelfID     string   `json:"selfId,omitempty"`
	Token      string   `json:"token,omitempty"`
	Categories []string `json:"categories,omitempty"` // T7: รายชื่อหมวดคำ (ส่งครั้งเดียวตอน join)
	Room       *Room    `json:"room"`
}

type ReactionPayload struct {
	Type  string   `json:"type"`
	From  ChatFrom `json:"from"`
	Emoji string   `json:"emoji"`
	Ts    int64    `json:"ts"`
}

type ErrorMessage struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

type NoticeMessage struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

type ClientMessage struct {
	Type             string `json:"type"`
	TargetID         string `json:"targetId,omitempty"`
	Duration         int    `json:"duration,omitempty"`
	SuspectID        string `json:"suspectId,omitempty"`
	SecretWord       string `json:"secretWord,omitempty"`
	Text             string `json:"text,omitempty"`
	ChatEnabled      *bool  `json:"chatEnabled,omitempty"`
	CorrectGuesserID string `json:"correctGuesserId,omitempty"`
	Category         string `json:"category,omitempty"` // T7
	Random           bool   `json:"random,omitempty"`   // T7
	Emoji            string `json:"emoji,omitempty"`    // T10
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

	// T12 hardening
	MaxPlayers      = 12
	MaxNameLen      = 24
	ChatMinInterval = 500 * time.Millisecond
	MaxChatLen      = 300

	// T8 / T10
	MaxHints         = 10
	MaxHintLen       = 80
	MaxEmojiLen      = 16
	ReactMinInterval = 400 * time.Millisecond
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

func deleteRoom(room *Room) {
	roomsMu.Lock()
	defer roomsMu.Unlock()
	delete(rooms, room.Code)
}

func makePlayerID() string {
	return strconv.FormatInt(time.Now().UnixNano(), 36) + "-" + strconv.Itoa(rand.Intn(100000))
}

func makeToken() string {
	const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, 32)
	for i := range b {
		b[i] = charset[rand.Intn(len(charset))]
	}
	return string(b)
}

// countConnected returns the number of players still connected.
// Caller must hold room.mu.
func countConnected(room *Room) int {
	n := 0
	for _, p := range room.Players {
		if p.Connected {
			n++
		}
	}
	return n
}

// eligibleVoters counts players allowed to vote this round:
// not the judge, not blocked, and currently connected (T6).
// Caller must hold room.mu.
func eligibleVoters(room *Room) int {
	n := 0
	for id, p := range room.Players {
		if id == room.JudgeID {
			continue
		}
		if p.Spectator {
			continue
		}
		if room.BlockedVoters != nil && room.BlockedVoters[id] {
			continue
		}
		if !p.Connected {
			continue
		}
		n++
	}
	return n
}

// countActivePlayers counts non-spectator seats (used for the room cap).
// Caller must hold room.mu.
func countActivePlayers(room *Room) int {
	n := 0
	for _, p := range room.Players {
		if !p.Spectator {
			n++
		}
	}
	return n
}

// buildSnapshotFor builds a per-recipient redacted snapshot (T1).
// Secret word goes only to judge + insider; every player's role is hidden
// except the viewer's own; the insider's identity is revealed only on the
// scoreboard. Tokens are never serialized. Caller must hold room.mu.
func buildSnapshotFor(room *Room, viewerID string) *Room {
	snap := &Room{
		Code:              room.Code,
		State:             room.State,
		HostID:            room.HostID,
		JudgeID:           room.JudgeID,
		Timer:             room.Timer,
		RoundEndByTimeout: room.RoundEndByTimeout,
		ChatEnabled:       room.ChatEnabled,
		CorrectGuesserID:  room.CorrectGuesserID,
		Hints:             append([]string(nil), room.Hints...),
		QuestionCount:     room.QuestionCount,

		BlockedVoters: make(map[string]bool),
		Voted:         make(map[string]bool),
		LastVotes:     append([]VotePair(nil), room.LastVotes...),
		Players:       make(map[string]*Player),
	}

	// Secret word: only judge + insider may see it during play, but revealed to everyone on scoreboard phase.
	if viewerID == room.JudgeID || viewerID == room.InsiderID || room.State == "scoreboard" {
		snap.SecretWord = room.SecretWord
	}

	// Insider identity: revealed only on scoreboard.
	if room.State == "scoreboard" {
		snap.InsiderID = room.InsiderID
	}

	for id, b := range room.BlockedVoters {
		snap.BlockedVoters[id] = b
	}
	for id, v := range room.Voted {
		snap.Voted[id] = v
	}

	for id, p := range room.Players {
		role := ""
		if id == viewerID {
			// Viewer always sees their own role.
			role = p.Role
		}
		snap.Players[id] = &Player{
			ID:        p.ID,
			Name:      p.Name,
			Score:     p.Score,
			Role:      role,
			Connected: p.Connected,
			Spectator: p.Spectator,
		}
	}

	return snap
}

func broadcastRoom(room *Room) {
	room.mu.Lock()
	defer room.mu.Unlock()

	for _, p := range room.Players {
		if p.Conn == nil {
			continue
		}
		snap := buildSnapshotFor(room, p.ID)
		_ = p.Conn.WriteJSON(OutgoingRoomMessage{
			Type: "room",
			Room: snap,
		})
	}
}

func sendRoomToPlayer(room *Room, player *Player) {
	room.mu.Lock()
	defer room.mu.Unlock()

	snap := buildSnapshotFor(room, player.ID)
	msg := OutgoingRoomMessage{
		Type:       "room",
		SelfID:     player.ID,
		Token:      player.Token, // self-only; never sent to others
		Categories: categories(), // T7: ส่งรายชื่อหมวดครั้งเดียวตอน join
		Room:       snap,
	}
	_ = player.Conn.WriteJSON(msg)
}

func broadcastNotice(room *Room, text string) {
	room.mu.Lock()
	defer room.mu.Unlock()
	for _, p := range room.Players {
		if p.Conn == nil {
			continue
		}
		_ = p.Conn.WriteJSON(NoticeMessage{Type: "notice", Message: text})
	}
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
		if p.Spectator {
			continue
		}
		p.Role = "normal"
	}
	if room.JudgeID != "" {
		if j, ok := room.Players[room.JudgeID]; ok {
			j.Role = "judge"
		}
	}

	// Only connected, non-judge, non-spectator players can be the insider.
	candidates := make([]*Player, 0)
	for _, p := range room.Players {
		if p.ID == room.JudgeID {
			continue
		}
		if p.Spectator || !p.Connected {
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
	room.CorrectGuesserID = ""
	room.BlockedVoters = make(map[string]bool)
	room.Voted = make(map[string]bool)
	room.LastVotes = []VotePair{}
	room.Hints = nil
	room.QuestionCount = 0

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
	room.voteTimerDuration = duration
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
					needRevote := handleTallyVotes(r)
					broadcastRoom(r)
					if needRevote {
						r.mu.Lock()
						newDuration := r.voteTimerDuration / 2
						if newDuration < 15 {
							newDuration = 15
						}
						r.mu.Unlock()
						startVoteTimer(r, newDuration)
					}
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

func handleGuessCorrect(room *Room, correctGuesserID string) {
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
	room.CorrectGuesserID = correctGuesserID
	room.State = "voting"
	room.Votes = make(map[string]string)
	room.Voted = make(map[string]bool)
	room.BlockedVoters = make(map[string]bool)
	room.LastVotes = []VotePair{}
	room.mu.Unlock()

	broadcastRoom(room)
	startVoteTimer(room, VoteDurationSeconds)
}

// handleTallyVotes tallies votes and returns true if a revote is needed.
func handleTallyVotes(room *Room) bool {
	room.mu.Lock()
	defer room.mu.Unlock()

	if len(room.Players) == 0 {
		return false
	}

	lastVotes := make([]VotePair, 0, len(room.Votes))
	for voterID, targetID := range room.Votes {
		lastVotes = append(lastVotes, VotePair{VoterID: voterID, TargetID: targetID})
	}
	room.LastVotes = lastVotes

	count := make(map[string]int)
	for _, suspectID := range room.Votes {
		count[suspectID]++
	}

	if len(count) == 0 {
		room.State = "scoreboard"
		room.Votes = make(map[string]string)
		room.Voted = make(map[string]bool)
		room.BlockedVoters = make(map[string]bool)
		return false
	}

	maxVote := -1
	for _, c := range count {
		if c > maxVote {
			maxVote = c
		}
	}

	top := []string{}
	for id, c := range count {
		if c == maxVote {
			top = append(top, id)
		}
	}

	var votedID string

	if len(top) > 1 {
		// Accumulate: block all tied players across revote rounds
		if room.BlockedVoters == nil {
			room.BlockedVoters = make(map[string]bool)
		}
		for _, id := range top {
			room.BlockedVoters[id] = true
		}

		// Count eligible voters remaining (not judge, not blocked, connected)
		remaining := eligibleVoters(room)

		if remaining > 0 {
			// Need another revote round
			room.State = "voting"
			room.Votes = make(map[string]string)
			room.Voted = make(map[string]bool)
			return true
		}

		// Edge case: no eligible voters left — pick a random loser from the tied group
		votedID = top[rand.Intn(len(top))]
	} else {
		votedID = top[0]
	}

	isCorrect := votedID == room.InsiderID
	if isCorrect {
		for _, p := range room.Players {
			if p.ID == room.InsiderID || p.ID == room.JudgeID || p.ID == room.CorrectGuesserID {
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
	room.Voted = make(map[string]bool)
	room.BlockedVoters = make(map[string]bool)
	return false
}

func handleNextRound(room *Room) {
	room.mu.Lock()
	defer room.mu.Unlock()

	// T6: cleanup offline (ghost) seats when returning to lobby.
	for id, p := range room.Players {
		if !p.Connected {
			delete(room.Players, id)
			if room.HostID == id {
				room.HostID = ""
			}
			if room.JudgeID == id {
				room.JudgeID = ""
			}
		}
	}
	// Ensure there is a host among remaining players.
	if _, ok := room.Players[room.HostID]; !ok || room.HostID == "" {
		room.HostID = ""
		for id, p := range room.Players {
			if p.Connected {
				room.HostID = id
				break
			}
		}
	}

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
	room.SecretWord = ""
	room.Votes = make(map[string]string)
	room.RoundEndByTimeout = false
	room.CorrectGuesserID = ""
	room.BlockedVoters = make(map[string]bool)
	room.Voted = make(map[string]bool)
	room.LastVotes = []VotePair{}
	room.Hints = nil
	room.QuestionCount = 0
}

func sanitizeName(name string) string {
	name = strings.TrimSpace(name)
	// Collapse newlines/tabs to spaces.
	name = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		return r
	}, name)
	runes := []rune(name)
	if len(runes) > MaxNameLen {
		runes = runes[:MaxNameLen]
	}
	return strings.TrimSpace(string(runes))
}

func wsHandler(c *websocket.Conn) {
	roomCode := c.Query("room")
	playerName := sanitizeName(c.Query("name"))
	mode := c.Query("mode")
	token := c.Query("token")

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

	var player *Player
	reattached := false

	room.mu.Lock()
	// T4: try to re-attach to an existing seat via reconnect token.
	if token != "" {
		for _, p := range room.Players {
			if p.Token == token {
				player = p
				break
			}
		}
	}

	if player != nil {
		// Close any stale connection still bound to this seat (e.g. duplicate tab).
		if player.Conn != nil {
			_ = player.Conn.Close()
		}
		player.Conn = c
		player.Connected = true
		player.Name = playerName
		reattached = true
		// Restore host if the seat had become host while offline-less, or if no host
		// (spectators never become host).
		if room.HostID == "" && !player.Spectator {
			room.HostID = player.ID
		}
	} else {
		isSpectator := mode == "spectate"
		// T12: enforce room capacity for brand-new (non-spectator) seats.
		if !isSpectator && countActivePlayers(room) >= MaxPlayers {
			room.mu.Unlock()
			sendError(c, "ห้องเต็มแล้ว (สูงสุด "+strconv.Itoa(MaxPlayers)+" คน)")
			_ = c.Close()
			return
		}
		playerID := makePlayerID()
		player = &Player{
			ID:        playerID,
			Name:      playerName,
			Score:     0,
			Role:      "",
			Connected: true,
			Spectator: isSpectator,
			Token:     makeToken(),
			Conn:      c,
		}
		room.Players[playerID] = player
		if room.HostID == "" && !isSpectator {
			room.HostID = playerID
		}
	}
	playerID := player.ID
	room.mu.Unlock()

	sendRoomToPlayer(room, player)
	broadcastRoom(room)

	if reattached {
		log.Printf("[WS] %s re-attached to room %s\n", playerName, roomCode)
	} else {
		log.Printf("[WS] %s joined room %s (mode=%s)\n", playerName, roomCode, mode)
	}

	defer func() {
		log.Printf("[WS] %s disconnected from room %s\n", playerName, roomCode)

		room.mu.Lock()
		p := room.Players[playerID]
		if p != nil {
			// Only clear if this disconnect belongs to the live connection
			// (avoid a stale defer clobbering a fresh re-attach).
			if p.Conn == c {
				p.Connected = false
				p.Conn = nil
			} else {
				// This seat was already re-attached by a newer connection.
				room.mu.Unlock()
				return
			}
		}

		wasJudge := room.JudgeID == playerID
		wasInsider := room.InsiderID == playerID
		activeRound := room.State == "assign_roles" || room.State == "countdown" || room.State == "voting"

		// Host transfer: if the host went offline, hand it to a connected player.
		if room.HostID == playerID {
			room.HostID = ""
			for id, pp := range room.Players {
				if pp.Connected {
					room.HostID = id
					break
				}
			}
			if room.HostID == "" {
				// No one online: keep seat as host so it can reclaim on reconnect.
				room.HostID = playerID
			}
		}

		// In lobby, a judge leaving frees the judge slot so a new one can be picked.
		if room.JudgeID == playerID && !activeRound {
			room.JudgeID = ""
		}

		// T5: void the round if the judge or insider drops mid-round.
		voided := false
		if activeRound && (wasJudge || wasInsider) {
			room.timerRunning = false
			if room.timerCancel != nil {
				close(room.timerCancel)
				room.timerCancel = nil
			}
			room.State = "lobby"
			room.InsiderID = ""
			room.SecretWord = ""
			room.JudgeID = ""
			for _, pp := range room.Players {
				pp.Role = ""
			}
			room.Votes = make(map[string]string)
			room.Voted = make(map[string]bool)
			room.BlockedVoters = make(map[string]bool)
			room.LastVotes = []VotePair{}
			room.RoundEndByTimeout = false
			room.CorrectGuesserID = ""
			room.Hints = nil
			room.QuestionCount = 0
			voided = true
		}

		noConnected := countConnected(room) == 0
		room.mu.Unlock()

		if noConnected {
			// No one is left connected — drop the room to avoid leaking memory.
			deleteRoom(room)
			return
		}

		if voided {
			broadcastNotice(room, "รอบถูกยกเลิกเพราะ Judge หรือ Insider หลุดการเชื่อมต่อ")
		}
		broadcastRoom(room)
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
			if room.HostID != playerID {
				room.mu.Unlock()
				sendError(c, "เฉพาะ Host เท่านั้นที่ตั้งกรรมการได้")
				continue
			}
			if room.State != "lobby" {
				room.mu.Unlock()
				sendError(c, "ตั้งกรรมการได้เฉพาะตอนอยู่ใน Lobby")
				continue
			}
			if target, ok := room.Players[msg.TargetID]; ok && target.Connected && !target.Spectator {
				room.JudgeID = msg.TargetID
			} else {
				room.mu.Unlock()
				sendError(c, "ผู้เล่นที่เลือกไม่อยู่ในห้อง ออฟไลน์ หรือเป็นผู้ชม")
				continue
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
			// T2: only the judge may start the round (matches the UI).
			if room.JudgeID != playerID {
				room.mu.Unlock()
				sendError(c, "เฉพาะกรรมการเท่านั้นที่เริ่มรอบได้")
				continue
			}
			if room.State != "lobby" {
				room.mu.Unlock()
				sendError(c, "เริ่มรอบได้เฉพาะตอนอยู่ใน Lobby")
				continue
			}
			// T3/T2: validate BEFORE mutating room state.
			var secret string
			if msg.Random {
				secret = pickRandomWord(msg.Category)
			} else {
				secret = strings.TrimSpace(msg.SecretWord)
			}
			hasJudge := room.JudgeID != ""
			nonJudgeCount := 0
			for id, p := range room.Players {
				if id == room.JudgeID {
					continue
				}
				if p.Spectator || !p.Connected {
					continue
				}
				nonJudgeCount++
			}
			if secret == "" {
				room.mu.Unlock()
				sendError(c, "กรรมการต้องกำหนดคำปริศนาก่อนเริ่มเกม")
				continue
			}
			if !hasJudge || nonJudgeCount < 2 {
				room.mu.Unlock()
				sendError(c, "ต้องมีผู้เล่น (ไม่นับกรรมการ) อย่างน้อย 2 คน (ต้องการผู้เล่นทั้งหมดอย่างน้อย 3 คนขึ้นไป)")
				continue
			}
			room.SecretWord = secret
			room.mu.Unlock()

			assignRoles(room)
			broadcastRoom(room)
			startCountdownTimer(room, msg.Duration)

		case "guess_word":
			room.mu.Lock()
			if room.State != "countdown" {
				room.mu.Unlock()
				continue
			}
			guess := strings.ToLower(strings.TrimSpace(msg.Text))
			secret := strings.ToLower(strings.TrimSpace(room.SecretWord))
			if guess == "" || secret == "" {
				room.mu.Unlock()
				continue
			}
			if strings.Contains(guess, secret) {
				room.mu.Unlock()
				log.Printf("[SPEECH] Correct guess by player %s in room %s: %s contains %s\n", playerID, room.Code, guess, secret)
				handleGuessCorrect(room, playerID)
			} else {
				room.mu.Unlock()
			}

		case "guess_correct":
			room.mu.Lock()
			isJudge := room.JudgeID == playerID
			if !isJudge {
				room.mu.Unlock()
				sendError(c, "เฉพาะกรรมการเท่านั้นที่กดทายถูกได้")
				continue
			}
			if room.State != "countdown" {
				room.mu.Unlock()
				sendError(c, "ยังไม่อยู่ในช่วงเล่น")
				continue
			}
			// T3: validate correctGuesserId (must be a real, non-judge player).
			guesser := msg.CorrectGuesserID
			if guesser != "" {
				gp, ok := room.Players[guesser]
				if !ok || guesser == room.JudgeID || gp == nil {
					guesser = ""
				}
			}
			room.mu.Unlock()
			handleGuessCorrect(room, guesser)

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

			if self := room.Players[playerID]; self != nil && self.Spectator {
				room.mu.Unlock()
				sendError(c, "ผู้ชมไม่สามารถโหวตได้")
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

			if sp, ok := room.Players[msg.SuspectID]; !ok || sp.Spectator || msg.SuspectID == room.JudgeID {
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

			// T6: only connected, non-judge, non-blocked players count as eligible.
			eligible := eligibleVoters(room)

			if len(room.Votes) >= eligible && eligible > 0 {
				if room.timerRunning {
					room.timerRunning = false
					if room.timerCancel != nil {
						close(room.timerCancel)
						room.timerCancel = nil
					}
				}
				room.mu.Unlock()
				needRevote := handleTallyVotes(room)
				broadcastRoom(room)
				if needRevote {
					if eligibleVoters(room) == 0 {
						// Deadlock: no eligible voters remain after blocking tied players — resolve immediately
						handleTallyVotes(room)
						broadcastRoom(room)
					} else {
						room.mu.Lock()
						newDuration := room.voteTimerDuration / 2
						if newDuration < 15 {
							newDuration = 15
						}
						room.mu.Unlock()
						startVoteTimer(room, newDuration)
					}
				}
			} else {
				room.mu.Unlock()
				broadcastRoom(room)
			}

		case "next_round":
			room.mu.Lock()
			isHost := room.HostID == playerID
			room.mu.Unlock()
			if !isHost {
				sendError(c, "เฉพาะ Host เท่านั้นที่เริ่มรอบถัดไปได้")
				continue
			}
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

		case "chat":
			txt := strings.TrimSpace(msg.Text)
			if txt == "" {
				continue
			}
			if len(txt) > MaxChatLen {
				txt = txt[:MaxChatLen]
			}

			room.mu.Lock()
			enabled := room.ChatEnabled
			sender, ok := room.Players[playerID]
			// T12: simple per-player chat rate limit.
			rateLimited := false
			if ok && sender != nil {
				now := time.Now()
				if !sender.lastChat.IsZero() && now.Sub(sender.lastChat) < ChatMinInterval {
					rateLimited = true
				} else {
					sender.lastChat = now
				}
			}
			room.mu.Unlock()

			if !ok || sender == nil {
				continue
			}
			if rateLimited {
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

		case "add_hint":
			// T8: กรรมการโพสต์ "ใบ้" สาธารณะระหว่างเล่น (ไม่ใช่คำลับ)
			hint := strings.TrimSpace(msg.Text)
			if hint == "" {
				continue
			}
			if len([]rune(hint)) > MaxHintLen {
				hint = string([]rune(hint)[:MaxHintLen])
			}
			room.mu.Lock()
			if room.JudgeID != playerID {
				room.mu.Unlock()
				sendError(c, "เฉพาะกรรมการเท่านั้นที่ให้ใบ้ได้")
				continue
			}
			if room.State != "countdown" {
				room.mu.Unlock()
				sendError(c, "ให้ใบ้ได้เฉพาะตอนกำลังเล่น")
				continue
			}
			if len(room.Hints) >= MaxHints {
				room.mu.Unlock()
				sendError(c, "ให้ใบ้ครบจำนวนสูงสุดแล้ว")
				continue
			}
			room.Hints = append(room.Hints, hint)
			room.mu.Unlock()
			broadcastRoom(room)

		case "ask_question":
			// T8: ตัวนับจำนวนคำถาม (ข้อมูลช่วยจับจังหวะเกม)
			room.mu.Lock()
			self := room.Players[playerID]
			if self == nil || self.Spectator || room.State != "countdown" {
				room.mu.Unlock()
				continue
			}
			room.QuestionCount++
			room.mu.Unlock()
			broadcastRoom(room)

		case "react":
			// T10: emoji reaction ชั่วคราว (ไม่กระทบ state ตัดสิน)
			emoji := strings.TrimSpace(msg.Emoji)
			if emoji == "" {
				continue
			}
			if len([]rune(emoji)) > MaxEmojiLen {
				emoji = string([]rune(emoji)[:MaxEmojiLen])
			}
			room.mu.Lock()
			sender, ok := room.Players[playerID]
			rateLimited := false
			if ok && sender != nil {
				now := time.Now()
				if !sender.lastReact.IsZero() && now.Sub(sender.lastReact) < ReactMinInterval {
					rateLimited = true
				} else {
					sender.lastReact = now
				}
			}
			room.mu.Unlock()
			if !ok || sender == nil || rateLimited {
				continue
			}
			payload := ReactionPayload{
				Type:  "reaction",
				From:  ChatFrom{ID: sender.ID, Name: sender.Name},
				Emoji: emoji,
				Ts:    time.Now().Unix(),
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

func loadEnv() {
	file, err := os.Open(".env")
	if err != nil {
		return // no .env file, ignore
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 {
			key := strings.TrimSpace(parts[0])
			val := strings.TrimSpace(parts[1])
			val = strings.Trim(val, `"'`)
			os.Setenv(key, val)
		}
	}
}

func main() {
	loadEnv()

	app := fiber.New()

	app.Use(cors.New(cors.Config{
		AllowOrigins: "*",
		AllowHeaders: "Origin, Content-Type, Accept",
	}))

	app.Get("/ws", websocket.New(wsHandler))

	app.Get("/api/agora-token", func(c *fiber.Ctx) error {
		channelName := c.Query("channelName")
		uid := c.Query("uid")
		if channelName == "" || uid == "" {
			return c.Status(400).JSON(fiber.Map{
				"error": "missing channelName or uid",
			})
		}

		appID := os.Getenv("AGORA_APP_ID")
		appCertificate := os.Getenv("AGORA_APP_CERTIFICATE")

		if appID == "" || appCertificate == "" {
			return c.Status(500).JSON(fiber.Map{
				"error": "Agora credentials are not configured on the server",
			})
		}

		// Token expires in 2 hours
		expireTimeInSeconds := uint32(7200)

		token, err := rtctokenbuilder.BuildTokenWithUserAccount(
			appID,
			appCertificate,
			channelName,
			uid,
			rtctokenbuilder.RolePublisher,
			expireTimeInSeconds,
			expireTimeInSeconds,
		)
		if err != nil {
			return c.Status(500).JSON(fiber.Map{
				"error": "failed to generate token: " + err.Error(),
			})
		}

		return c.JSON(fiber.Map{
			"token": token,
		})
	})

	port := os.Getenv("PORT")
	if port == "" {
		port = "3001"
	}

	log.Println("Go WebSocket server running on port", port)
	if err := app.Listen(":" + port); err != nil {
		log.Fatal(err)
	}
}

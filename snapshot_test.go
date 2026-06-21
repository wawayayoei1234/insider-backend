package main

import "testing"

func makeTestRoom(state string) (*Room, string, string, string) {
	hostID := "host1"
	judgeID := "judge1"
	insiderID := "ins1"
	commonsID := "com1"

	room := &Room{
		Code:      "TEST",
		State:     state,
		HostID:    hostID,
		JudgeID:   judgeID,
		InsiderID: insiderID,
		SecretWord: "ส้มตำ",
		Players: map[string]*Player{
			hostID:    {ID: hostID, Name: "Host", Role: "normal", Connected: true},
			judgeID:   {ID: judgeID, Name: "Judge", Role: "judge", Connected: true},
			insiderID: {ID: insiderID, Name: "Ins", Role: "insider", Connected: true},
			commonsID: {ID: commonsID, Name: "Com", Role: "normal", Connected: true},
		},
		BlockedVoters: map[string]bool{},
		Voted:         map[string]bool{},
	}
	return room, judgeID, insiderID, commonsID
}

func TestCommonsCannotSeeSecretOrInsider(t *testing.T) {
	room, _, insiderID, commonsID := makeTestRoom("countdown")

	snap := buildSnapshotFor(room, commonsID)

	if snap.SecretWord != "" {
		t.Errorf("LEAK: commons received secretWord=%q, want empty", snap.SecretWord)
	}
	if snap.InsiderID != "" {
		t.Errorf("LEAK: commons received insiderId=%q, want empty during countdown", snap.InsiderID)
	}
	// Commons must not learn the insider's role.
	if r := snap.Players[insiderID].Role; r != "" {
		t.Errorf("LEAK: commons sees insider role=%q, want empty", r)
	}
	// Commons sees their own role.
	if r := snap.Players[commonsID].Role; r != "normal" {
		t.Errorf("commons should see own role=normal, got %q", r)
	}
}

func TestJudgeAndInsiderSeeSecret(t *testing.T) {
	room, judgeID, insiderID, _ := makeTestRoom("countdown")

	js := buildSnapshotFor(room, judgeID)
	if js.SecretWord != "ส้มตำ" {
		t.Errorf("judge should see secret, got %q", js.SecretWord)
	}
	is := buildSnapshotFor(room, insiderID)
	if is.SecretWord != "ส้มตำ" {
		t.Errorf("insider should see secret, got %q", is.SecretWord)
	}
	if is.Players[insiderID].Role != "insider" {
		t.Errorf("insider should see own role=insider, got %q", is.Players[insiderID].Role)
	}
	// Even the insider must not learn the secret is sent to commons.
	if is.InsiderID != "" {
		t.Errorf("insiderId should stay hidden during countdown, got %q", is.InsiderID)
	}
}

func TestInsiderRevealedOnScoreboard(t *testing.T) {
	room, _, insiderID, commonsID := makeTestRoom("scoreboard")

	snap := buildSnapshotFor(room, commonsID)
	if snap.InsiderID != insiderID {
		t.Errorf("on scoreboard, insiderId should be revealed=%q, got %q", insiderID, snap.InsiderID)
	}
	// Secret word is fine to reveal on the scoreboard too? No — we still only
	// send it to judge/insider. Commons should still not get it here.
	if snap.SecretWord != "" {
		t.Errorf("commons still should not receive secretWord via snapshot, got %q", snap.SecretWord)
	}
}

func TestTokenNeverInSnapshot(t *testing.T) {
	room, judgeID, _, commonsID := makeTestRoom("countdown")
	room.Players[commonsID].Token = "supersecrettoken"

	snap := buildSnapshotFor(room, judgeID)
	if snap.Players[commonsID].Token != "" {
		t.Errorf("LEAK: token present in snapshot for another player")
	}
}

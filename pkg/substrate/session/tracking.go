// Package session holds the substrate-canonical session-tracking schema.
//
// This file holds pure schema only — no I/O, no goroutines, no kernel
// coupling. The .cog/status/.session file is structured as a Tracking
// document; its readers (sessionStatus, sessionEnd) and writers
// (sessionInit, sessionTrackSpawn, sessionTrackReap) live in the kernel
// root package and consume this type.
package session

// Tracking represents the persisted session-tracking record at
// .cog/status/.session. It carries the session identifier, the branch
// the session was opened on, lifecycle timestamps, and the
// spawned/active/reaped agent rosters maintained as agents come and go.
//
// Naming per ADR-100 substrate convention: the redundant "Session"
// prefix is dropped now that the type lives in package session. Callers
// at the root use the alias SessionTracking from session.go.
type Tracking struct {
	SessionID     string   `json:"sessionId"`
	Branch        string   `json:"branch"`
	StartedAt     string   `json:"startedAt"`
	EndedAt       *string  `json:"endedAt,omitempty"`
	Status        *string  `json:"status,omitempty"`
	RootAgent     string   `json:"rootAgent"`
	SpawnedAgents []string `json:"spawnedAgents"`
	ActiveAgents  []string `json:"activeAgents"`
	ReapedAgents  []string `json:"reapedAgents"`
}

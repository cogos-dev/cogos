// cmd_channel.go — CLI commands for the channel bridge.
//
// Commands:
//   cog channel post <channel> <message>  — Post message + trigger agent via gateway
//   cog channel send <channel> <message>  — Send message (no agent trigger)
//   cog channel read [channel] [-n N]     — Read recent messages from bus
//   cog channel list                      — List configured channels
//   cog channel help                      — Show usage

package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ─── Dispatcher ─────────────────────────────────────────────────────────────────

func cmdChannel(args []string) int {
	if len(args) == 0 {
		cmdChannelHelp()
		return 0
	}

	switch args[0] {
	case "post":
		return cmdChannelPost(args[1:])
	case "send":
		return cmdChannelSend(args[1:])
	case "read":
		return cmdChannelRead(args[1:])
	case "list":
		return cmdChannelList()
	case "help", "-h", "--help":
		cmdChannelHelp()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "Unknown channel command: %s\n", args[0])
		cmdChannelHelp()
		return 1
	}
}

// ─── send ───────────────────────────────────────────────────────────────────────

// cmdChannelSend posts a message to a channel without triggering agent response.
// This lets an agent (e.g. Cog) speak in a channel as itself.
func cmdChannelSend(args []string) int {
	if len(args) < 2 {
		fmt.Fprintf(os.Stderr, "Usage: cog channel send <channel> <message>\n")
		return 1
	}

	channelName := args[0]
	message := strings.Join(args[1:], " ")

	root, _, err := ResolveWorkspace()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	cfg, err := LoadChannelBridgeConfig(root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	entry, err := cfg.Lookup(channelName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	gatewayURL := os.Getenv("OPENCLAW_URL")
	if gatewayURL == "" {
		gatewayURL = "http://localhost:18789"
	}
	gatewayToken := os.Getenv("OPENCLAW_TOKEN")

	client := NewGatewayClient(gatewayURL, gatewayToken)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := client.Connect(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}
	defer client.Close()

	sendResult, err := client.Send(ctx, SendParams{
		To:             entry.DiscordID,
		Message:        message,
		Channel:        "discord",
		IdempotencyKey: randomID() + "-send",
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}
	fmt.Printf("sent: messageId=%s\n", sendResult.MessageID)
	return 0
}

// ─── post ───────────────────────────────────────────────────────────────────────

func cmdChannelPost(args []string) int {
	if len(args) < 2 {
		fmt.Fprintf(os.Stderr, "Usage: cog channel post <channel> <message>\n")
		return 1
	}

	channelName := args[0]
	message := strings.Join(args[1:], " ")

	// 1. Resolve workspace and load config
	root, _, err := ResolveWorkspace()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	cfg, err := LoadChannelBridgeConfig(root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	entry, err := cfg.Lookup(channelName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	// 2. Resolve gateway URL from env
	gatewayURL := os.Getenv("OPENCLAW_URL")
	if gatewayURL == "" {
		gatewayURL = "http://localhost:18789"
	}
	gatewayToken := os.Getenv("OPENCLAW_TOKEN")

	// 3. Connect to gateway
	client := NewGatewayClient(gatewayURL, gatewayToken)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	fmt.Printf("Connecting to gateway %s ...\n", gatewayURL)
	if err := client.Connect(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}
	defer client.Close()

	// 4. Generate idempotency key prefix
	idempotencyPrefix := randomID()

	// 5. Fan-out: send + agent (sequential, same connection)

	// 5a. Send — makes the message visible in Discord
	fmt.Printf("Sending to #%s ...\n", channelName)
	sendResult, err := client.Send(ctx, SendParams{
		To:             entry.DiscordID,
		Message:        message,
		Channel:        "discord",
		IdempotencyKey: idempotencyPrefix + "-send",
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error (send): %v\n", err)
		return 1
	}
	fmt.Printf("  send: messageId=%s\n", sendResult.MessageID)

	// 5b. Agent — triggers the target agent to respond
	if entry.AgentID != "" {
		fmt.Printf("Triggering agent %q ...\n", entry.AgentID)
		agentResult, err := client.Agent(ctx, AgentParams{
			Message:        message,
			AgentID:        entry.AgentID,
			To:             entry.DiscordID,
			Deliver:        true,
			Channel:        "discord",
			IdempotencyKey: idempotencyPrefix + "-agent",
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error (agent): %v\n", err)
			return 1
		}
		fmt.Printf("  agent: status=%s runId=%s\n", agentResult.Status, agentResult.RunID)
	}

	fmt.Println("Done.")
	return 0
}

// ─── read ───────────────────────────────────────────────────────────────────────

// cmdChannelRead reads recent channel messages from the cogbus event log.
// Messages arrive via the cogbus-message-bridge hook in OpenClaw — no direct
// Discord API access needed.
//
// Usage:
//
//	cog channel read [channel] [-n N]
//
// Without a channel filter, shows all channel messages. With a channel name,
// filters to that channel's messages. -n limits output (default 20).
func cmdChannelRead(args []string) int {
	// Parse flags
	limit := 20
	channel := ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-n":
			if i+1 >= len(args) {
				fmt.Fprintf(os.Stderr, "Error: -n requires a number\n")
				return 1
			}
			n, err := strconv.Atoi(args[i+1])
			if err != nil || n <= 0 {
				fmt.Fprintf(os.Stderr, "Error: -n must be a positive integer\n")
				return 1
			}
			limit = n
			i++ // skip value
		default:
			if channel == "" {
				channel = args[i]
			} else {
				fmt.Fprintf(os.Stderr, "Error: unexpected argument %q\n", args[i])
				return 1
			}
		}
	}

	root, _, err := ResolveWorkspace()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	// Find the cogbus bridge bus — look for a bus with the openclaw participant
	busesDir := filepath.Join(root, ".cog", ".state", "buses")
	registryPath := filepath.Join(busesDir, "registry.json")

	data, err := os.ReadFile(registryPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error reading bus registry: %v\n", err)
		return 1
	}

	var entries []busRegistryEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing bus registry: %v\n", err)
		return 1
	}

	// Find the cogbus (has an openclaw participant)
	var cogbusID string
	for _, entry := range entries {
		for _, p := range entry.Participants {
			if strings.Contains(p, "openclaw") {
				cogbusID = entry.BusID
				break
			}
		}
		if cogbusID != "" {
			break
		}
	}

	if cogbusID == "" {
		fmt.Println("No cogbus bridge found in bus registry.")
		fmt.Println("The cogbus-message-bridge hook must be active in OpenClaw.")
		return 1
	}

	// Read events from the bus JSONL, collecting channel messages
	eventsPath := filepath.Join(busesDir, cogbusID, "events.jsonl")
	f, err := os.Open(eventsPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error reading bus events: %v\n", err)
		return 1
	}
	defer f.Close()

	type channelMsg struct {
		ts      string
		channel string
		from    string
		content string
	}

	var msgs []channelMsg
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		var block CogBlock
		if err := json.Unmarshal(scanner.Bytes(), &block); err != nil {
			continue
		}

		// Match channel message events: "discord.message", "telegram.message", etc.
		if !strings.HasSuffix(block.Type, BlockChannelMessageSuffix) || block.Type == BlockMessage {
			continue
		}

		// Parse the message payload (stored as JSON string in the "message" field)
		msgStr, ok := block.Payload["message"].(string)
		if !ok {
			// Payload might already be structured (from direct bus events)
			msgStr = ""
		}

		var payload map[string]interface{}
		if msgStr != "" {
			if err := json.Unmarshal([]byte(msgStr), &payload); err != nil {
				payload = block.Payload
			}
		} else {
			payload = block.Payload
		}

		msgChannel := ""
		if v, ok := payload["channel"].(string); ok {
			msgChannel = v
		}
		if v, ok := payload["channel_name"].(string); ok && v != "" {
			msgChannel = v
		}

		// Apply channel filter
		if channel != "" && msgChannel != channel {
			continue
		}

		from := ""
		if v, ok := payload["username"].(string); ok && v != "" {
			from = v
		} else if v, ok := payload["display_name"].(string); ok && v != "" {
			from = v
		} else if v, ok := payload["from"].(string); ok {
			from = v
		}

		content := ""
		if v, ok := payload["content"].(string); ok {
			content = v
		}

		ts := block.Ts
		if v, ok := payload["timestamp"].(string); ok && v != "" {
			ts = v
		}

		msgs = append(msgs, channelMsg{
			ts:      ts,
			channel: msgChannel,
			from:    from,
			content: content,
		})
	}

	if len(msgs) == 0 {
		if channel != "" {
			fmt.Printf("No messages found for channel %q on the cogbus.\n", channel)
		} else {
			fmt.Println("No channel messages found on the cogbus.")
		}
		fmt.Println("Messages appear here after the cogbus-message-bridge hook is active.")
		return 0
	}

	// Show last N messages
	start := 0
	if len(msgs) > limit {
		start = len(msgs) - limit
	}

	for _, m := range msgs[start:] {
		ts := m.ts
		// Format timestamp for display (extract time portion)
		if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
			ts = t.Format("15:04:05")
		} else if t, err := time.Parse(time.RFC3339, ts); err == nil {
			ts = t.Format("15:04:05")
		}

		if m.channel != "" {
			fmt.Printf("[%s] #%s %s: %s\n", ts, m.channel, m.from, m.content)
		} else {
			fmt.Printf("[%s] %s: %s\n", ts, m.from, m.content)
		}
	}

	fmt.Printf("\n(%d messages shown, %d total)\n", len(msgs)-start, len(msgs))
	return 0
}

// ─── list ───────────────────────────────────────────────────────────────────────

func cmdChannelList() int {
	root, _, err := ResolveWorkspace()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	cfg, err := LoadChannelBridgeConfig(root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	if len(cfg.Channels) == 0 {
		fmt.Println("No channels configured.")
		fmt.Println("Add channels in .cog/config/channels.yaml")
		return 0
	}

	fmt.Println("Configured channels:")
	for name, entry := range cfg.Channels {
		agent := entry.AgentID
		if agent == "" {
			agent = "(none)"
		}
		fmt.Printf("  %-20s discord=%s  agent=%s\n", name, entry.DiscordID, agent)
	}
	return 0
}

// ─── help ───────────────────────────────────────────────────────────────────────

func cmdChannelHelp() {
	fmt.Println("Usage: cog channel <command>")
	fmt.Println()
	fmt.Println("Commands:")
	fmt.Println("  send <channel> <message>   Send a message (no agent trigger)")
	fmt.Println("  post <channel> <message>   Post a message and trigger agent response")
	fmt.Println("  read [channel] [-n N]      Read recent messages from cogbus (default: 20)")
	fmt.Println("  list                       List configured channels")
	fmt.Println("  help                       Show this help")
	fmt.Println()
	fmt.Println("Examples:")
	fmt.Println("  cog channel send colony-chat \"[Cog · a1b2c3] Status update.\"")
	fmt.Println("  cog channel post colony-chat \"Hey Whirl, how's the colony?\"")
	fmt.Println("  cog channel read colony-chat -n 10")
	fmt.Println("  cog channel read")
	fmt.Println("  cog channel list")
	fmt.Println()
	fmt.Println("Config: .cog/config/channels.yaml")
	fmt.Println("Env: OPENCLAW_URL, OPENCLAW_TOKEN, CHANNEL_*_DISCORD_ID")
}

// ─── Helpers ────────────────────────────────────────────────────────────────────

// randomID generates a short random hex string for idempotency key prefixes.
func randomID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return fmt.Sprintf("%x", b)
}

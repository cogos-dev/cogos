// tui.go - CogOS Terminal User Interface
// Provides real-time observability into TAA pipeline and workspace state

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// === STYLES ===

var (
	// Colors
	primaryColor   = lipgloss.Color("#7C3AED") // Purple
	secondaryColor = lipgloss.Color("#10B981") // Green
	warningColor   = lipgloss.Color("#F59E0B") // Yellow
	errorColor     = lipgloss.Color("#EF4444") // Red
	mutedColor     = lipgloss.Color("#6B7280") // Gray
	bgColor        = lipgloss.Color("#1F2937") // Dark

	// Panel styles
	panelStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(primaryColor).
			Padding(0, 1)

	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(primaryColor).
			MarginBottom(1)

	labelStyle = lipgloss.NewStyle().
			Foreground(mutedColor)

	valueStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#FFFFFF"))

	goodStyle = lipgloss.NewStyle().
			Foreground(secondaryColor)

	warnStyle = lipgloss.NewStyle().
			Foreground(warningColor)

	badStyle = lipgloss.NewStyle().
			Foreground(errorColor)

	barFullStyle = lipgloss.NewStyle().
			Foreground(secondaryColor)

	barEmptyStyle = lipgloss.NewStyle().
			Foreground(mutedColor)

	// Chat styles
	userMsgStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#60A5FA")).
			Bold(true)

	assistantMsgStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#34D399"))

	chatInputStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(primaryColor).
			Padding(0, 1)

	activePanelStyle = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(secondaryColor).
				Padding(0, 1)
)

// === MODEL ===

type tuiModel struct {
	// Dimensions
	width  int
	height int

	// Data
	taaConfig      *TAADisplayConfig
	identity       *IdentityDisplay
	coherence      *CoherenceDisplay
	ontology       *OntologyDisplay
	events         []EventDisplay
	pipelineState  *PipelineDisplay

	// Chat state
	chatMessages   []TUIChatMessage
	chatInput      textarea.Model
	chatViewport   viewport.Model
	isStreaming    bool
	streamingText  string
	currentModel   string

	// UI state
	viewMode       string // "dashboard" or "chat"
	activePanel    int
	eventsViewport viewport.Model
	ready          bool

	// Paths
	cogRoot       string
	workspaceRoot string
}

// TUIChatMessage represents a chat message in the TUI
type TUIChatMessage struct {
	Role      string    // "user" or "assistant"
	Content   string
	Timestamp time.Time
	Tokens    int
}

// TAADisplayConfig holds TAA config for display
type TAADisplayConfig struct {
	TotalTokens   int
	Tier1Budget   int
	Tier1Percent  int
	Tier2Budget   int
	Tier2Percent  int
	Tier3Budget   int
	Tier3Percent  int
	Tier4Budget   int
	Tier4Percent  int

	// Semantic config
	MaxCandidates int
	MaxResults    int

	// Ranking weights
	BM25Weight      float64
	SubstanceWeight float64
	RecencyWeight   float64
}

// IdentityDisplay holds identity info for display
type IdentityDisplay struct {
	Name          string
	Role          string
	ContextPlugin string
	MemoryPath    string
	Loaded        bool
}

// CoherenceDisplay holds coherence state for display
type CoherenceDisplay struct {
	Coherent      bool
	CanonicalHash string
	CurrentHash   string
	DriftCount    int
	LastCheck     time.Time
}

// OntologyDisplay holds ontology info for display
type OntologyDisplay struct {
	ID            string
	Version       string
	Primitives    int
	Projections   int
	CogdocTypes   int
	Relations     int
	TrackedPaths  int
	ExcludedPaths int
}

// EventDisplay represents a single event
type EventDisplay struct {
	Timestamp time.Time
	Type      string
	Source    string
	Message   string
	Level     string // info, warn, error
}

// PipelineDisplay shows the current inference pipeline state
type PipelineDisplay struct {
	Stage           string // "idle", "tier1", "tier2", "tier3", "tier4", "inference", "streaming"
	UserMessage     string
	Tier1Tokens     int
	Tier2Tokens     int
	Tier3Tokens     int
	Tier4Tokens     int
	TotalTokens     int
	Anchor          string
	Goal            string
	Model           string
	ResponseTokens  int
	LastLatency     time.Duration
}

// === MESSAGES ===

type tickMsg time.Time
type windowSizeMsg struct {
	width  int
	height int
}

// Chat messages
type inferenceStartMsg struct{}
type inferenceChunkMsg struct {
	content string
}
type inferenceDoneMsg struct {
	content string
	err     error
}
type streamTickMsg time.Time

// === INIT ===

func initialTUIModel(cogRoot, workspaceRoot string) tuiModel {
	// Initialize textarea for chat input
	ti := textarea.New()
	ti.Placeholder = "Type a message... (Enter to send, Shift+Enter for newline)"
	ti.Focus()
	ti.CharLimit = 4000
	ti.SetWidth(80)
	ti.SetHeight(3)
	ti.ShowLineNumbers = false

	return tuiModel{
		cogRoot:       cogRoot,
		workspaceRoot: workspaceRoot,
		events:        make([]EventDisplay, 0),
		chatMessages:  make([]TUIChatMessage, 0),
		chatInput:     ti,
		pipelineState: &PipelineDisplay{Stage: "idle"},
		viewMode:      "dashboard",
		currentModel:  "claude-sonnet-4-20250514",
		activePanel:   0,
	}
}

func (m tuiModel) Init() tea.Cmd {
	return tea.Batch(
		tea.EnterAltScreen,
		tickCmd(),
		textarea.Blink,
	)
}

func tickCmd() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

// === UPDATE ===

func (m tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.KeyMsg:
		// Global keys (work in any mode)
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "ctrl+d":
			m.viewMode = "dashboard"
			return m, nil
		case "ctrl+t":
			m.viewMode = "chat"
			m.chatInput.Focus()
			return m, textarea.Blink
		}

		// Mode-specific keys
		if m.viewMode == "dashboard" {
			switch msg.String() {
			case "q":
				return m, tea.Quit
			case "c":
				m.viewMode = "chat"
				m.chatInput.Focus()
				return m, textarea.Blink
			case "tab":
				m.activePanel = (m.activePanel + 1) % 4
			case "shift+tab":
				m.activePanel = (m.activePanel + 3) % 4
			case "r":
				m.loadData()
			}
		} else if m.viewMode == "chat" {
			switch msg.String() {
			case "esc":
				m.viewMode = "dashboard"
				m.chatInput.Blur()
				return m, nil
			case "enter":
				if !m.isStreaming && strings.TrimSpace(m.chatInput.Value()) != "" {
					// Send message
					userMsg := m.chatInput.Value()
					m.chatMessages = append(m.chatMessages, TUIChatMessage{
						Role:      "user",
						Content:   userMsg,
						Timestamp: time.Now(),
					})
					m.chatInput.Reset()
					m.isStreaming = true
					m.streamingText = ""
					m.pipelineState.Stage = "tier1"
					return m, m.runInferenceCmd(userMsg)
				}
			}

			// Update textarea
			var cmd tea.Cmd
			m.chatInput, cmd = m.chatInput.Update(msg)
			cmds = append(cmds, cmd)
		}

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.ready = true

		// Initialize viewport for events
		headerHeight := 3
		footerHeight := 2
		verticalMargin := headerHeight + footerHeight
		m.eventsViewport = viewport.New(m.width/2-4, m.height/2-verticalMargin)
		m.eventsViewport.SetContent(m.renderEvents())

		// Initialize chat viewport
		m.chatViewport = viewport.New(m.width-4, m.height-10)
		m.chatViewport.SetContent(m.renderChatHistory())

		// Resize textarea
		m.chatInput.SetWidth(m.width - 4)

	case tickMsg:
		// Periodic refresh (less frequent in chat mode)
		if m.viewMode == "dashboard" {
			m.loadData()
			m.eventsViewport.SetContent(m.renderEvents())
		}
		cmds = append(cmds, tickCmd())

	case inferenceDoneMsg:
		m.isStreaming = false
		m.pipelineState.Stage = "idle"
		if msg.err != nil {
			m.chatMessages = append(m.chatMessages, TUIChatMessage{
				Role:      "assistant",
				Content:   "Error: " + msg.err.Error(),
				Timestamp: time.Now(),
			})
		} else {
			m.chatMessages = append(m.chatMessages, TUIChatMessage{
				Role:      "assistant",
				Content:   msg.content,
				Timestamp: time.Now(),
			})
		}
		m.streamingText = ""
		m.chatViewport.SetContent(m.renderChatHistory())
		m.chatViewport.GotoBottom()

	case inferenceChunkMsg:
		m.streamingText += msg.content
		m.chatViewport.SetContent(m.renderChatHistory())
		m.chatViewport.GotoBottom()
	}

	// Update viewports
	var cmd tea.Cmd
	m.eventsViewport, cmd = m.eventsViewport.Update(msg)
	cmds = append(cmds, cmd)
	m.chatViewport, cmd = m.chatViewport.Update(msg)
	cmds = append(cmds, cmd)

	return m, tea.Batch(cmds...)
}

// === DATA LOADING ===

func (m *tuiModel) loadData() {
	m.loadTAAConfig()
	m.loadIdentity()
	m.loadCoherence()
	m.loadOntology()
	m.loadEvents()
}

func (m *tuiModel) loadTAAConfig() {
	configPath := filepath.Join(m.cogRoot, "conf", "config", "taa.yaml")
	data, err := os.ReadFile(configPath)
	if err != nil {
		m.taaConfig = &TAADisplayConfig{TotalTokens: 100000}
		return
	}

	// Parse YAML (simplified - in production use yaml.Unmarshal)
	cfg := &TAADisplayConfig{
		TotalTokens:     100000,
		Tier1Percent:    33,
		Tier2Percent:    25,
		Tier3Percent:    33,
		Tier4Percent:    6,
		MaxCandidates:   20,
		MaxResults:      10,
		BM25Weight:      0.5,
		SubstanceWeight: 0.3,
		RecencyWeight:   0.2,
	}

	// Quick parse for key values
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "total_tokens:") {
			fmt.Sscanf(line, "total_tokens: %d", &cfg.TotalTokens)
		} else if strings.HasPrefix(line, "tier1_identity:") {
			fmt.Sscanf(line, "tier1_identity: %d", &cfg.Tier1Percent)
		} else if strings.HasPrefix(line, "tier2_temporal:") {
			fmt.Sscanf(line, "tier2_temporal: %d", &cfg.Tier2Percent)
		} else if strings.HasPrefix(line, "tier3_present:") {
			fmt.Sscanf(line, "tier3_present: %d", &cfg.Tier3Percent)
		} else if strings.HasPrefix(line, "tier4_semantic:") {
			fmt.Sscanf(line, "tier4_semantic: %d", &cfg.Tier4Percent)
		}
	}

	cfg.Tier1Budget = cfg.TotalTokens * cfg.Tier1Percent / 100
	cfg.Tier2Budget = cfg.TotalTokens * cfg.Tier2Percent / 100
	cfg.Tier3Budget = cfg.TotalTokens * cfg.Tier3Percent / 100
	cfg.Tier4Budget = cfg.TotalTokens * cfg.Tier4Percent / 100

	m.taaConfig = cfg
}

func (m *tuiModel) loadIdentity() {
	configPath := filepath.Join(m.cogRoot, "conf", "config", "identity.yaml")
	data, err := os.ReadFile(configPath)
	if err != nil {
		m.identity = &IdentityDisplay{Name: "unknown", Loaded: false}
		return
	}

	id := &IdentityDisplay{Loaded: true}

	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "default_identity:") {
			id.Name = strings.TrimSpace(strings.TrimPrefix(line, "default_identity:"))
		} else if strings.HasPrefix(line, "identity_directory:") {
			id.MemoryPath = strings.TrimSpace(strings.TrimPrefix(line, "identity_directory:"))
		}
	}

	// Try to load actual identity card for role
	if id.Name != "" {
		cardPath := filepath.Join(m.workspaceRoot, "projects", "cog_lab_package", "identities", "identity_"+id.Name+".md")
		if cardData, err := os.ReadFile(cardPath); err == nil {
			// Parse frontmatter for role
			content := string(cardData)
			if strings.HasPrefix(content, "---\n") {
				end := strings.Index(content[4:], "\n---")
				if end > 0 {
					fm := content[4 : 4+end]
					for _, line := range strings.Split(fm, "\n") {
						if strings.HasPrefix(line, "role:") {
							id.Role = strings.TrimSpace(strings.TrimPrefix(line, "role:"))
						} else if strings.HasPrefix(line, "context_plugin:") {
							id.ContextPlugin = strings.TrimSpace(strings.TrimPrefix(line, "context_plugin:"))
						}
					}
				}
			}
		}
	}

	m.identity = id
}

func (m *tuiModel) loadCoherence() {
	statePath := filepath.Join(m.cogRoot, ".state", "coherence.json")
	data, err := os.ReadFile(statePath)
	if err != nil {
		m.coherence = &CoherenceDisplay{Coherent: true}
		return
	}

	var state struct {
		Current struct {
			Coherent      bool   `json:"coherent"`
			CanonicalHash string `json:"canonical_hash"`
			CurrentHash   string `json:"current_hash"`
			Timestamp     string `json:"timestamp"`
		} `json:"current"`
	}

	if err := json.Unmarshal(data, &state); err != nil {
		m.coherence = &CoherenceDisplay{Coherent: true}
		return
	}

	ts, _ := time.Parse(time.RFC3339, state.Current.Timestamp)
	m.coherence = &CoherenceDisplay{
		Coherent:      state.Current.Coherent,
		CanonicalHash: state.Current.CanonicalHash,
		CurrentHash:   state.Current.CurrentHash,
		LastCheck:     ts,
	}
}

func (m *tuiModel) loadOntology() {
	ont, err := getOntology(m.cogRoot)
	if err != nil {
		m.ontology = &OntologyDisplay{ID: "error"}
		return
	}

	m.ontology = &OntologyDisplay{
		ID:            ont.ID,
		Version:       ont.Version,
		Primitives:    len(ont.Topology.Primitives),
		Projections:   len(ont.URIScheme.Projections),
		CogdocTypes:   len(ont.Cogdoc.Types.Core) + len(ont.Cogdoc.Types.Semantic),
		Relations:     len(ont.Cogdoc.Relations.Structural) + len(ont.Cogdoc.Relations.Semantic),
		TrackedPaths:  len(ont.Coherence.Tracked),
		ExcludedPaths: len(ont.Coherence.Excluded),
	}
}

func (m *tuiModel) loadEvents() {
	// Load recent events from ledger
	ledgerPath := filepath.Join(m.cogRoot, "ledger")
	entries, err := os.ReadDir(ledgerPath)
	if err != nil {
		return
	}

	// Get most recent session
	var latestSession string
	var latestTime time.Time
	for _, entry := range entries {
		if entry.IsDir() {
			info, err := entry.Info()
			if err != nil {
				continue
			}
			if info.ModTime().After(latestTime) {
				latestTime = info.ModTime()
				latestSession = entry.Name()
			}
		}
	}

	if latestSession == "" {
		return
	}

	// Read events from session
	eventsPath := filepath.Join(ledgerPath, latestSession, "events.jsonl")
	data, err := os.ReadFile(eventsPath)
	if err != nil {
		return
	}

	lines := strings.Split(string(data), "\n")
	m.events = make([]EventDisplay, 0)

	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var evt struct {
			Type      string `json:"type"`
			Timestamp string `json:"timestamp"`
			Data      struct {
				Message string `json:"message"`
				Source  string `json:"source"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(line), &evt); err != nil {
			continue
		}

		ts, _ := time.Parse(time.RFC3339, evt.Timestamp)
		m.events = append(m.events, EventDisplay{
			Timestamp: ts,
			Type:      evt.Type,
			Source:    evt.Data.Source,
			Message:   evt.Data.Message,
			Level:     "info",
		})
	}

	// Keep last 50
	if len(m.events) > 50 {
		m.events = m.events[len(m.events)-50:]
	}
}

// === CHAT ===

// runInferenceCmd runs inference asynchronously
func (m tuiModel) runInferenceCmd(userMsg string) tea.Cmd {
	return func() tea.Msg {
		// Try HTTP server first (if running)
		resp, err := m.inferViaHTTP(userMsg)
		if err == nil {
			return inferenceDoneMsg{content: resp}
		}

		// Fall back to CLI
		resp, err = m.inferViaCLI(userMsg)
		if err != nil {
			return inferenceDoneMsg{err: err}
		}
		return inferenceDoneMsg{content: resp}
	}
}

// inferViaHTTP calls the local serve endpoint
func (m tuiModel) inferViaHTTP(userMsg string) (string, error) {
	// Build messages array
	messages := make([]map[string]string, 0)
	for _, msg := range m.chatMessages {
		messages = append(messages, map[string]string{
			"role":    msg.Role,
			"content": msg.Content,
		})
	}

	reqBody := map[string]interface{}{
		"model":    m.currentModel,
		"messages": messages,
		"stream":   false,
	}

	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}

	resp, err := http.Post(
		"http://localhost:5100/v1/chat/completions",
		"application/json",
		bytes.NewBuffer(jsonBody),
	)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("server returned %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}

	if err := json.Unmarshal(body, &result); err != nil {
		return "", err
	}

	if len(result.Choices) == 0 {
		return "", fmt.Errorf("no choices in response")
	}

	return result.Choices[0].Message.Content, nil
}

// inferViaCLI falls back to cog infer command
func (m tuiModel) inferViaCLI(userMsg string) (string, error) {
	cmd := exec.Command(filepath.Join(m.cogRoot, "cog"), "infer", userMsg)
	cmd.Dir = m.workspaceRoot

	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("inference failed: %w", err)
	}

	return strings.TrimSpace(string(output)), nil
}

// renderChatHistory renders all chat messages
func (m tuiModel) renderChatHistory() string {
	var b strings.Builder

	for _, msg := range m.chatMessages {
		ts := msg.Timestamp.Format("15:04")

		if msg.Role == "user" {
			b.WriteString(userMsgStyle.Render(fmt.Sprintf("[%s] You:", ts)))
			b.WriteString("\n")
			b.WriteString(msg.Content)
			b.WriteString("\n\n")
		} else {
			b.WriteString(assistantMsgStyle.Render(fmt.Sprintf("[%s] Assistant:", ts)))
			b.WriteString("\n")
			b.WriteString(msg.Content)
			b.WriteString("\n\n")
		}
	}

	// Show streaming text if active
	if m.isStreaming && m.streamingText != "" {
		b.WriteString(assistantMsgStyle.Render("[...] Assistant:"))
		b.WriteString("\n")
		b.WriteString(m.streamingText)
		b.WriteString("▌") // Cursor
		b.WriteString("\n")
	} else if m.isStreaming {
		b.WriteString(labelStyle.Render("Thinking..."))
		b.WriteString("\n")
	}

	return b.String()
}

// === VIEW ===

func (m tuiModel) View() string {
	if !m.ready {
		return "Loading..."
	}

	if m.viewMode == "chat" {
		return m.renderChatView()
	}

	return m.renderDashboardView()
}

func (m tuiModel) renderDashboardView() string {
	// Build layout
	leftWidth := m.width / 2
	rightWidth := m.width - leftWidth - 2

	// Left column: TAA + Identity + Coherence
	taaPanel := m.renderTAAPanel(leftWidth - 2)
	identityPanel := m.renderIdentityPanel(leftWidth - 2)
	coherencePanel := m.renderCoherencePanel(leftWidth - 2)

	leftCol := lipgloss.JoinVertical(lipgloss.Left,
		taaPanel,
		identityPanel,
		coherencePanel,
	)

	// Right column: Pipeline + Events + Ontology
	pipelinePanel := m.renderPipelinePanel(rightWidth - 2)
	eventsPanel := m.renderEventsPanel(rightWidth - 2)
	ontologyPanel := m.renderOntologyPanel(rightWidth - 2)

	rightCol := lipgloss.JoinVertical(lipgloss.Left,
		pipelinePanel,
		eventsPanel,
		ontologyPanel,
	)

	// Combine columns
	main := lipgloss.JoinHorizontal(lipgloss.Top, leftCol, rightCol)

	// Footer
	footer := lipgloss.NewStyle().
		Foreground(mutedColor).
		Render("  q: quit | c: chat | tab: switch panel | r: refresh")

	return lipgloss.JoinVertical(lipgloss.Left, main, footer)
}

func (m tuiModel) renderChatView() string {
	var b strings.Builder

	// Header
	header := titleStyle.Render(fmt.Sprintf("Chat - %s", m.currentModel))
	if m.isStreaming {
		header += "  " + goodStyle.Render("● streaming")
	}
	b.WriteString(header)
	b.WriteString("\n")

	// Chat history viewport
	chatPanel := panelStyle.
		Width(m.width - 4).
		Height(m.height - 12).
		Render(m.chatViewport.View())
	b.WriteString(chatPanel)
	b.WriteString("\n")

	// Input area
	inputPanel := chatInputStyle.
		Width(m.width - 4).
		Render(m.chatInput.View())
	b.WriteString(inputPanel)
	b.WriteString("\n")

	// Footer
	var footerText string
	if m.isStreaming {
		footerText = "  Streaming response..."
	} else {
		footerText = "  Enter: send | Esc: dashboard | Ctrl+C: quit"
	}
	footer := lipgloss.NewStyle().
		Foreground(mutedColor).
		Render(footerText)
	b.WriteString(footer)

	return b.String()
}

func (m tuiModel) renderTAAPanel(width int) string {
	var b strings.Builder

	b.WriteString(titleStyle.Render("TAA Budget Allocation"))
	b.WriteString("\n")

	if m.taaConfig == nil {
		b.WriteString(labelStyle.Render("Loading..."))
		return panelStyle.Width(width).Render(b.String())
	}

	cfg := m.taaConfig

	// Token budget bar
	b.WriteString(fmt.Sprintf("%s %s\n",
		labelStyle.Render("Total:"),
		valueStyle.Render(fmt.Sprintf("%dk tokens", cfg.TotalTokens/1000))))

	// Tier bars
	b.WriteString(m.renderTierBar("T1 Identity", cfg.Tier1Percent, cfg.Tier1Budget, width-4))
	b.WriteString(m.renderTierBar("T2 Temporal", cfg.Tier2Percent, cfg.Tier2Budget, width-4))
	b.WriteString(m.renderTierBar("T3 Present ", cfg.Tier3Percent, cfg.Tier3Budget, width-4))
	b.WriteString(m.renderTierBar("T4 Semantic", cfg.Tier4Percent, cfg.Tier4Budget, width-4))

	// Ranking weights
	b.WriteString("\n")
	b.WriteString(labelStyle.Render("Ranking: "))
	b.WriteString(fmt.Sprintf("BM25=%.1f Sub=%.1f Rec=%.1f",
		cfg.BM25Weight, cfg.SubstanceWeight, cfg.RecencyWeight))

	return panelStyle.Width(width).Render(b.String())
}

func (m tuiModel) renderTierBar(label string, percent, tokens, width int) string {
	barWidth := width - 25
	if barWidth < 10 {
		barWidth = 10
	}

	filled := barWidth * percent / 100
	empty := barWidth - filled

	bar := barFullStyle.Render(strings.Repeat("█", filled)) +
		barEmptyStyle.Render(strings.Repeat("░", empty))

	return fmt.Sprintf("%s %s %s\n",
		labelStyle.Render(label+":"),
		bar,
		valueStyle.Render(fmt.Sprintf("%2d%% %dk", percent, tokens/1000)))
}

func (m tuiModel) renderIdentityPanel(width int) string {
	var b strings.Builder

	b.WriteString(titleStyle.Render("Identity"))
	b.WriteString("\n")

	if m.identity == nil {
		b.WriteString(labelStyle.Render("Loading..."))
		return panelStyle.Width(width).Render(b.String())
	}

	id := m.identity

	status := goodStyle.Render("●")
	if !id.Loaded {
		status = badStyle.Render("●")
	}

	b.WriteString(fmt.Sprintf("%s %s %s\n",
		status,
		labelStyle.Render("Name:"),
		valueStyle.Render(id.Name)))

	if id.Role != "" {
		b.WriteString(fmt.Sprintf("  %s %s\n",
			labelStyle.Render("Role:"),
			valueStyle.Render(id.Role)))
	}

	if id.ContextPlugin != "" {
		b.WriteString(fmt.Sprintf("  %s %s\n",
			labelStyle.Render("Plugin:"),
			valueStyle.Render(id.ContextPlugin)))
	}

	return panelStyle.Width(width).Render(b.String())
}

func (m tuiModel) renderCoherencePanel(width int) string {
	var b strings.Builder

	b.WriteString(titleStyle.Render("Coherence"))
	b.WriteString("\n")

	if m.coherence == nil {
		b.WriteString(labelStyle.Render("Loading..."))
		return panelStyle.Width(width).Render(b.String())
	}

	coh := m.coherence

	var status string
	if coh.Coherent {
		status = goodStyle.Render("● COHERENT")
	} else {
		status = badStyle.Render("● DRIFTED")
	}
	b.WriteString(status + "\n")

	// Hashes (truncated)
	canonShort := coh.CanonicalHash
	if len(canonShort) > 12 {
		canonShort = canonShort[:12]
	}
	currentShort := coh.CurrentHash
	if len(currentShort) > 12 {
		currentShort = currentShort[:12]
	}

	hashStyle := valueStyle
	if !coh.Coherent {
		hashStyle = warnStyle
	}

	b.WriteString(fmt.Sprintf("%s %s\n",
		labelStyle.Render("Canonical:"),
		hashStyle.Render(canonShort)))
	b.WriteString(fmt.Sprintf("%s %s\n",
		labelStyle.Render("Current:  "),
		hashStyle.Render(currentShort)))

	if !coh.LastCheck.IsZero() {
		ago := time.Since(coh.LastCheck).Round(time.Second)
		b.WriteString(fmt.Sprintf("%s %s ago\n",
			labelStyle.Render("Checked:"),
			valueStyle.Render(ago.String())))
	}

	return panelStyle.Width(width).Render(b.String())
}

func (m tuiModel) renderPipelinePanel(width int) string {
	var b strings.Builder

	b.WriteString(titleStyle.Render("Pipeline"))
	b.WriteString("\n")

	if m.pipelineState == nil {
		b.WriteString(labelStyle.Render("Idle"))
		return panelStyle.Width(width).Render(b.String())
	}

	ps := m.pipelineState

	// Stage indicator
	stages := []string{"idle", "tier1", "tier2", "tier3", "tier4", "inference", "streaming"}
	stageIdx := 0
	for i, s := range stages {
		if s == ps.Stage {
			stageIdx = i
			break
		}
	}

	var stageBar strings.Builder
	for i := range stages {
		if i == stageIdx {
			stageBar.WriteString(goodStyle.Render("●"))
		} else if i < stageIdx {
			stageBar.WriteString(lipgloss.NewStyle().Foreground(mutedColor).Render("●"))
		} else {
			stageBar.WriteString(labelStyle.Render("○"))
		}
		if i < len(stages)-1 {
			stageBar.WriteString("─")
		}
	}
	b.WriteString(stageBar.String() + "\n")
	b.WriteString(labelStyle.Render("Stage: ") + valueStyle.Render(ps.Stage) + "\n")

	// Token usage
	if ps.TotalTokens > 0 {
		b.WriteString(fmt.Sprintf("%s T1:%d T2:%d T3:%d T4:%d = %d\n",
			labelStyle.Render("Tokens:"),
			ps.Tier1Tokens, ps.Tier2Tokens, ps.Tier3Tokens, ps.Tier4Tokens,
			ps.TotalTokens))
	}

	// Anchor/Goal
	if ps.Anchor != "" {
		anchor := ps.Anchor
		if len(anchor) > 30 {
			anchor = anchor[:30] + "..."
		}
		b.WriteString(fmt.Sprintf("%s %s\n",
			labelStyle.Render("Anchor:"),
			valueStyle.Render(anchor)))
	}

	if ps.Goal != "" {
		goal := ps.Goal
		if len(goal) > 30 {
			goal = goal[:30] + "..."
		}
		b.WriteString(fmt.Sprintf("%s %s\n",
			labelStyle.Render("Goal:"),
			valueStyle.Render(goal)))
	}

	return panelStyle.Width(width).Render(b.String())
}

func (m tuiModel) renderEventsPanel(width int) string {
	var b strings.Builder

	b.WriteString(titleStyle.Render("Events"))
	b.WriteString("\n")

	b.WriteString(m.renderEvents())

	return panelStyle.Width(width).Height(m.height/3).Render(b.String())
}

func (m tuiModel) renderEvents() string {
	if len(m.events) == 0 {
		return labelStyle.Render("No events")
	}

	var b strings.Builder

	// Show last 10 events
	start := 0
	if len(m.events) > 10 {
		start = len(m.events) - 10
	}

	for _, evt := range m.events[start:] {
		ts := evt.Timestamp.Format("15:04:05")

		var icon string
		switch evt.Level {
		case "error":
			icon = badStyle.Render("✗")
		case "warn":
			icon = warnStyle.Render("!")
		default:
			icon = goodStyle.Render("●")
		}

		evtType := evt.Type
		if len(evtType) > 15 {
			evtType = evtType[:15]
		}

		b.WriteString(fmt.Sprintf("%s %s %s\n",
			labelStyle.Render(ts),
			icon,
			valueStyle.Render(evtType)))
	}

	return b.String()
}

func (m tuiModel) renderOntologyPanel(width int) string {
	var b strings.Builder

	b.WriteString(titleStyle.Render("Ontology"))
	b.WriteString("\n")

	if m.ontology == nil {
		b.WriteString(labelStyle.Render("Loading..."))
		return panelStyle.Width(width).Render(b.String())
	}

	ont := m.ontology

	b.WriteString(fmt.Sprintf("%s %s v%s\n",
		goodStyle.Render("●"),
		valueStyle.Render(ont.ID),
		valueStyle.Render(ont.Version)))

	b.WriteString(fmt.Sprintf("%s %d  %s %d  %s %d\n",
		labelStyle.Render("Primitives:"),
		ont.Primitives,
		labelStyle.Render("Projections:"),
		ont.Projections,
		labelStyle.Render("Types:"),
		ont.CogdocTypes))

	b.WriteString(fmt.Sprintf("%s %d  %s %d/%d\n",
		labelStyle.Render("Relations:"),
		ont.Relations,
		labelStyle.Render("Tracked/Excluded:"),
		ont.TrackedPaths,
		ont.ExcludedPaths))

	return panelStyle.Width(width).Render(b.String())
}

// === COMMAND ===

func cmdTUI(args []string) int {
	cogRoot, err := findCogRoot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: not in a cog workspace\n")
		return 1
	}

	workspaceRoot := filepath.Dir(cogRoot)

	// Load ontology into cache for TUI use
	if _, err := getOntology(cogRoot); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not load ontology: %v\n", err)
	}

	m := initialTUIModel(cogRoot, workspaceRoot)
	m.loadData()

	p := tea.NewProgram(m, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error running TUI: %v\n", err)
		return 1
	}

	return 0
}

// findCogRoot finds the .cog directory
func findCogRoot() (string, error) {
	// Check COG_ROOT env
	if root := os.Getenv("COG_ROOT"); root != "" {
		cogDir := filepath.Join(root, ".cog")
		if _, err := os.Stat(cogDir); err == nil {
			return cogDir, nil
		}
	}

	// Walk up from cwd
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}

	for {
		cogDir := filepath.Join(dir, ".cog")
		if _, err := os.Stat(cogDir); err == nil {
			return cogDir, nil
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}

	return "", fmt.Errorf("no .cog directory found")
}

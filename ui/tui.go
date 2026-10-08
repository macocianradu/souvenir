package ui

import (
	"context"
	"log/slog"
	"strings"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"git.estatecloud.org/radumaco/souvenir/config"
	"git.estatecloud.org/radumaco/souvenir/db/history"
	"git.estatecloud.org/radumaco/souvenir/db/search"
	llm "git.estatecloud.org/radumaco/souvenir/llm"
	"git.estatecloud.org/radumaco/souvenir/model"
	"git.estatecloud.org/radumaco/souvenir/tools"
)

type focusState int

const (
	focusChat focusState = iota
	focusModels
	focusHistory
	focusSearch
)

type uiModel struct {
	logger         slog.Logger
	viewport       viewport.Model
	spinner        spinner.Model
	conversation   model.Conversation
	textarea       textarea.Model
	focus          focusState
	picker         picker
	thinkingBuffer *strings.Builder
	answerBuffer   *strings.Builder
	senderStyle    lipgloss.Style
	agentStyle     lipgloss.Style
	errorStyle     lipgloss.Style
	statusStyle    lipgloss.Style
	client         llm.ChatClient
	statusMessage  string
	commands       commandList
	history        history.DbClient
	searcher       *search.Searcher
	tools          *tools.Registry
	streamCh       <-chan llm.StreamEvent
	streamCtx      context.Context
	cancelStream   context.CancelFunc
	ctx            context.Context
	width          int
	height         int
	pending        int
	streaming      bool
	saving         bool
	saveQueued     bool
	titling        bool
	summarizing    bool
	convGen        int
	toolRounds     int
	toolTrail      []model.Message
}

type streamEventMessage struct {
	event llm.StreamEvent
}

type streamClosedMessage struct {
	err error
}

type streamStartedMessage struct {
	ch <-chan llm.StreamEvent
}

type conversationSavedMessage struct {
	conversation model.Conversation
	err          error
}

type summarizedMessage struct {
	summary *model.ContextSummary
	err     error
	gen     int
}

type toolsDoneMessage struct {
	results []model.Message
}

type conversationRenamedMessage struct {
	title   string
	summary string
	err     error
	gen     int
	auto    bool
}

func InitialModel(ctx context.Context, config config.Config, client history.DbClient, searcher *search.Searcher, registry *tools.Registry) uiModel {
	ta := textarea.New()
	ta.Placeholder = "Send a message..."
	ta.SetVirtualCursor(false)
	ta.Focus()

	ta.Prompt = "│ "

	ta.SetWidth(30)
	ta.SetHeight(3)

	s := ta.Styles()
	s.Focused.CursorLine = lipgloss.NewStyle()
	ta.SetStyles(s)

	ta.ShowLineNumbers = false

	vp := viewport.New(viewport.WithWidth(30), viewport.WithHeight(5))
	vp.SetContent(renderLanding(vp.Width(), vp.Height()))
	vp.KeyMap.Left.SetEnabled(false)
	vp.KeyMap.Right.SetEnabled(false)

	ta.KeyMap.InsertNewline.SetEnabled(false)

	sp := spinner.New()
	sp.Spinner = spinner.Dot

	ui := uiModel{
		textarea:       ta,
		conversation:   model.Conversation{},
		viewport:       vp,
		spinner:        sp,
		senderStyle:    lipgloss.NewStyle().Foreground(lipgloss.Color("5")),
		errorStyle:     lipgloss.NewStyle().Foreground(lipgloss.Color("9")),
		agentStyle:     lipgloss.NewStyle().Foreground(lipgloss.Color("86")),
		thinkingBuffer: &strings.Builder{},
		answerBuffer:   &strings.Builder{},
		client:         *llm.NewLLMClient(config),
		focus:          focusChat,
		history:        client,
		searcher:       searcher,
		tools:          registry,
		ctx:            ctx,
		logger:         *slog.Default().With("Component", "TUI"),
	}
	ui.picker = newPicker()
	ui.commands = newCommandList(30)
	ui.commands.setAvailable(ui.buildCommands())
	return ui
}

func (m uiModel) Init() tea.Cmd {
	return textarea.Blink
}

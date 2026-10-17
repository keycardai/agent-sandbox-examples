package page

import (
	"cmp"
	"slices"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/keycardai/agent-sandbox-examples/internal/sandbox"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/component"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/state"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/style"
)

const (
	railWidth = 30
	// noteCount is how many of the wizard's notes the question shows.
	noteCount = 5
	// railIndent is the width left of an answer under a step.
	railIndent = 3
	// reviewRailWidth is the width of the steps on the review.
	reviewRailWidth = 46
	// launchNumber is the width of a launch step's number and its space.
	launchNumber = 3
	// railGap is the space between the wizard's steps and its question, and
	// minPanel the narrowest question panel the steps leave room for.
	railGap  = 6
	minPanel = 50
	// maxSelectHeight bounds the rows a choice shows at once. huh counts a
	// title row in a choice's height, even without a title.
	maxSelectHeight, selectTitleRows = 10, 1
)

// RailState is where one step of the wizard is.
type RailState int

// The states of a step of the wizard.
const (
	RailPending RailState = iota
	RailCurrent
	RailDone
	// RailKept is a step setup does not ask, because its settings are fine.
	RailKept
)

// RailStep is one step of the wizard's rail, with what was chosen there.
type RailStep struct {
	ID, Title, About string
	State            RailState
	Answers          []string
}

// QuestionState is where a question is after a message.
type QuestionState int

// The states of a question.
const (
	// Asking waits for the answer.
	Asking QuestionState = iota
	// Answered has the answer, which Reply returns.
	Answered
	// Stopped stops the setup.
	Stopped
)

// asking is the question that shows, its form, and where the form keeps
// the answer, by kind.
type asking struct {
	question *Question
	form     *huh.Form
	text     *string
	yes      *bool
}

// Setup is the setup wizard and the review after it.
type Setup struct {
	view

	Rail []*RailStep
	// current is the step being asked, and notesFrom where its notes start.
	current   string
	notesFrom int
	ask       *asking
	// last is the answer to the last question answered.
	last answer
}

// NewSetup returns the setup wizard and its review.
func NewSetup(ui *UI, sandbox *state.Sandbox) *Setup {
	return &Setup{ui: ui, state: sandbox}
}

// RailSteps are every step of the wizard.
func RailSteps() []*RailStep {
	sections := sandbox.Sections()
	steps := make([]*RailStep, 0, len(sections))
	for _, section := range sections {
		steps = append(steps, &RailStep{ID: section.ID, Title: section.Title, About: section.About})
	}
	return steps
}

// PlannedSteps are the steps a first run's wizard shows from the start.
// Zone policy and Save join only when setup asks them.
func PlannedSteps() []*RailStep {
	return slices.DeleteFunc(RailSteps(), func(step *RailStep) bool {
		return step.ID == sandbox.SectionPolicy || step.ID == sandbox.SectionSave
	})
}

// Begin lays the rail out for the sections setup asks, or all when none.
func (s *Setup) Begin(ids ...string) {
	asked := map[string]bool{}
	for _, id := range ids {
		asked[id] = true
	}
	all := len(ids) == 0
	describe := s.state.Setup.Describe
	s.Rail, s.current, s.notesFrom = nil, "", 0
	for _, step := range RailSteps() {
		switch {
		case step.ID == sandbox.SectionPolicy, step.ID == sandbox.SectionSave:
			continue // Each joins the rail only when setup asks it.
		case all || asked[step.ID] || (step.ID == sandbox.SectionCredentials && asked[sandbox.SectionScenario]):
			step.State = RailPending
		case step.ID == sandbox.SectionCredentials:
			continue
		default:
			step.State = RailKept
			step.Answers = []string{describe[step.ID]}
		}
		s.Rail = append(s.Rail, step)
	}
}

// Asked reports whether this setup asked the section id, not kept it.
func (s *Setup) Asked(id string) bool {
	return slices.ContainsFunc(s.Rail, func(step *RailStep) bool { return step.ID == id && step.State != RailKept })
}

// Asking reports whether a question waits for an answer.
func (s *Setup) Asking() bool { return s.ask != nil }

// OnSection marks the step setup left as done, and msg's step as current.
func (s *Setup) OnSection(msg SectionMsg) {
	s.notesFrom = msg.NotesFrom
	for _, step := range s.Rail {
		if step.State == RailCurrent {
			step.State = RailDone
		}
	}
	index := slices.IndexFunc(s.Rail, func(step *RailStep) bool { return step.ID == msg.ID })
	if index < 0 {
		for _, step := range RailSteps() {
			if step.ID == msg.ID {
				s.Rail = append(s.Rail, step)
				index = len(s.Rail) - 1
			}
		}
	}
	if index >= 0 {
		s.Rail[index].State, s.Rail[index].Answers = RailCurrent, nil
	}
	s.current = msg.ID
}

// OnPreset puts a preset answer under the current step.
func (s *Setup) OnPreset(msg PresetMsg) {
	s.recordAnswer(msg.Question, answer{text: msg.Text})
}

// Finish marks the step setup was on as done.
func (s *Setup) Finish() {
	for _, step := range s.Rail {
		if step.State == RailCurrent {
			step.State = RailDone
		}
	}
}

// Ask shows the wizard's question as a form.
func (s *Setup) Ask(quest *Question) tea.Cmd {
	text, yes := quest.Value, quest.Yes
	width := s.panelWidth()
	// Esc stops the setup, so a list has no filter for esc to clear.
	keys := huh.NewDefaultKeyMap()
	keys.Select.Filter.SetEnabled(false)
	// Set the theme first: the width leaves out its border and padding.
	form := huh.NewForm(huh.NewGroup(questionField(quest, &text, &yes, width))).
		WithKeyMap(keys).
		WithShowHelp(false).
		WithTheme(s.ui.Styles.FormTheme(width, quest.Kind != AskConfirm)).
		WithWidth(width)
	// Sizing the form now draws the whole field from the first frame.
	next, _ := form.Update(tea.WindowSizeMsg{Width: s.ui.Width, Height: s.ui.Height})
	if sized, ok := next.(*huh.Form); ok {
		form = sized
	}
	s.ask = &asking{question: quest, form: form, text: &text, yes: &yes}
	return form.Init()
}

// Resize rebuilds the question. A form keeps the width it started with.
func (s *Setup) Resize() tea.Cmd {
	if s.ask == nil {
		return nil
	}
	quest := s.ask.question
	quest.Value, quest.Yes = *s.ask.text, *s.ask.yes
	return s.Ask(quest)
}

// Answer passes msg to the question's form, and answers the wizard when
// the form is done. Esc stops the setup.
func (s *Setup) Answer(msg tea.Msg) (tea.Cmd, QuestionState) {
	if key, ok := msg.(tea.KeyPressMsg); ok && key.String() == component.KeyEsc {
		s.ask = nil
		return nil, Stopped
	}
	next, cmd := s.ask.form.Update(msg)
	if form, ok := next.(*huh.Form); ok {
		s.ask.form = form
	}
	switch s.ask.form.State {
	case huh.StateCompleted:
		quest := s.ask.question
		got := answer{text: *s.ask.text, yes: *s.ask.yes}
		s.recordAnswer(quest, got)
		s.last = got
		s.ask = nil
		return cmd, Answered
	case huh.StateAborted:
		s.ask = nil
		return cmd, Stopped
	case huh.StateNormal:
	}
	return cmd, Asking
}

// Reply returns the last answer: its text, and whether it is yes.
func (s *Setup) Reply() (string, bool) { return s.last.text, s.last.yes }

// Drop takes the question away without an answer.
func (s *Setup) Drop() { s.ask = nil }

// Abandon forgets the steps, so the review shows the settings as they are.
func (s *Setup) Abandon() { s.Rail = nil }

// WizardView draws the steps, when they fit, beside the current question.
func (s *Setup) WizardView() string {
	body := lipgloss.NewStyle().Width(s.panelWidth()).Render(lipgloss.JoinVertical(lipgloss.Left,
		s.questionPanel()...))
	if s.railFits() {
		body = lipgloss.JoinHorizontal(lipgloss.Top, s.railView(railWidth, false), strings.Repeat(" ", railGap), body)
	}
	return s.ui.Page(s.progress(), s.anchored(body), s.wizardHints())
}

// ReviewView shows the steps with what setup chose, beside the launch.
func (s *Setup) ReviewView() string {
	width := s.ui.Width - component.PageIndent*half
	beside := width - reviewRailWidth - railGap
	var body string
	switch {
	case len(s.Rail) > 0 && beside >= minPanel:
		panel := s.launchPanel(min(beside, readWidth))
		body = lipgloss.JoinHorizontal(lipgloss.Top, s.railView(reviewRailWidth, true), strings.Repeat(" ", railGap),
			panel)
	default:
		width = min(width, readWidth)
		body = lipgloss.JoinVertical(lipgloss.Left, s.reviewLines(width), "", s.launchPanel(width))
	}
	var hints []component.Hint
	if len(s.state.Setup.Problems) == 0 {
		hints = append(hints, component.Hint{Key: component.KeyEnter, Desc: "launch"})
	}
	return s.ui.Page("setup · review", s.centered(body), append(hints,
		component.Hint{Key: "e", Desc: "change a setting"},
		component.Hint{Key: component.KeyEsc, Desc: "dashboard"},
		component.Hint{Key: "q", Desc: wordQuit},
	))
}

// recordAnswer puts the answer under the current step, readable on its own.
func (s *Setup) recordAnswer(quest *Question, got answer) {
	shown := got.text
	switch quest.Kind {
	case AskSecret:
		shown = shortPrompt(quest.Title) + " entered"
	case AskConfirm:
		asks, _ := splitQuestion(quest.Title)
		shown = asks + " " + map[bool]string{true: "Yes", false: "No"}[got.yes]
	case AskChoose:
		shown = chosenLabel(quest, got.text)
	case AskInput:
	}
	for _, step := range s.Rail {
		if step.ID == s.current && shown != "" {
			step.Answers = append(step.Answers, shown)
		}
	}
}

// railFits reports whether the steps fit beside a question panel.
func (s *Setup) railFits() bool {
	return s.ui.Width-component.PageIndent*half-railWidth-railGap >= minPanel
}

// panelWidth is the question panel's width, at most readWidth.
func (s *Setup) panelWidth() int {
	width := s.ui.Width - component.PageIndent*half
	if s.railFits() {
		width -= railWidth + railGap
	}
	return max(min(width, readWidth), 1)
}

func (s *Setup) wizardHints() []component.Hint {
	stop, next := component.Hint{Key: component.KeyEsc, Desc: "stop setup"},
		component.Hint{Key: component.KeyEnter, Desc: "continue"}
	if s.ask == nil {
		return []component.Hint{stop}
	}
	switch s.ask.question.Kind {
	case AskChoose:
		return []component.Hint{{Key: "↑↓", Desc: wordChoose}, next, stop}
	case AskConfirm:
		return []component.Hint{{Key: "←→", Desc: wordChoose}, next, stop}
	case AskInput, AskSecret:
	}
	return []component.Hint{next, stop}
}

// progress says which step of how many the wizard is on.
func (s *Setup) progress() string {
	current := 0
	for i, step := range s.Rail {
		if step.State == RailCurrent {
			current = i + 1
		}
	}
	if current == 0 {
		return "setup"
	}
	return "setup · step " + strconv.Itoa(min(current, len(s.Rail))) + " of " + strconv.Itoa(len(s.Rail))
}

// railView draws the steps width wide, and wraps answers or cuts them.
func (s *Setup) railView(width int, wrap bool) string {
	theme := s.ui.Styles
	link := theme.Faint.Render("│")
	lines := make([]string, 0, len(s.Rail)*half)
	for i, step := range s.Rail {
		if i > 0 {
			lines = append(lines, link)
		}
		lines = append(lines, s.railMark(step)+"  "+s.railTitle(step))
		under := link
		if i == len(s.Rail)-1 {
			under = " "
		}
		for _, answer := range step.Answers {
			for _, line := range answerLines(answer, width-railIndent, wrap) {
				lines = append(lines, under+"  "+theme.Muted.Render(line))
			}
		}
	}
	return lipgloss.NewStyle().Width(width).Render(strings.Join(lines, "\n"))
}

// railMark is a step's mark. The current step spins while setup works.
func (s *Setup) railMark(step *RailStep) string {
	theme := s.ui.Styles
	switch step.State {
	case RailCurrent:
		if s.ask == nil {
			return theme.Running.Render(s.ui.spinner())
		}
		return theme.Accent.Render("●")
	case RailDone, RailKept:
		return theme.OK.Render("✓")
	case RailPending:
	}
	return theme.Faint.Render("○")
}

// railTitle is a step's title, brightest for the step that asks.
func (s *Setup) railTitle(step *RailStep) string {
	theme := s.ui.Styles
	switch step.State {
	case RailCurrent:
		return theme.SectionTitle.Render(step.Title)
	case RailDone:
		return theme.Text.Render(step.Title)
	case RailKept:
		return theme.Muted.Render(step.Title)
	case RailPending:
	}
	return theme.Dim.Render(step.Title)
}

// questionPanel draws the current step, its notes, and the question.
func (s *Setup) questionPanel() []string {
	theme := s.ui.Styles
	width := s.panelWidth()
	para := lipgloss.NewStyle().Width(width)
	parts := make([]string, 0, noteCount+half*half)
	if step := s.currentStep(); step != nil {
		parts = append(parts, theme.SectionTitle.Render(step.Title), para.Render(theme.Muted.Render(step.About)), "")
	}
	if notes := s.notes(); len(notes) > 0 {
		for i, note := range notes {
			noteStyle := theme.Dim
			if i == len(notes)-1 {
				noteStyle = theme.Muted
			}
			parts = append(parts, para.Render(theme.Accent.Render("› ")+noteStyle.Render(note)))
		}
		parts = append(parts, "")
	}
	if s.ask == nil {
		return append(parts, theme.Running.Render(s.ui.spinner())+theme.Muted.Render(" Working…"))
	}
	return append(parts, s.questionBlock(width))
}

func (s *Setup) currentStep() *RailStep {
	for _, step := range s.Rail {
		if step.ID == s.current {
			return step
		}
	}
	return nil
}

// questionBlock draws the question, its field, and how to answer.
func (s *Setup) questionBlock(width int) string {
	theme := s.ui.Styles
	quest := s.ask.question
	para := lipgloss.NewStyle().Width(width)
	title, about := splitQuestion(quest.Title)
	parts := []string{para.Render(theme.Title.Render(title))}
	if about != "" {
		parts = append(parts, para.Render(theme.Muted.Render(about)))
	}
	if quest.Kind == AskConfirm {
		parts = append(parts, "")
	}
	parts = append(parts, s.ask.form.View())
	if hint := s.questionHint(quest); hint != "" {
		parts = append(parts, para.Render(hint))
	}
	return lipgloss.JoinVertical(lipgloss.Left, parts...)
}

// questionHint says how to answer quest, with keys in the accent.
func (s *Setup) questionHint(quest *Question) string {
	key, text := s.ui.Styles.Accent.Render, s.ui.Styles.Dim.Render
	var hints []string
	switch quest.Kind {
	case AskSecret:
		hints = append(hints, text("Hidden as you type. "+s.secretGoes()))
	case AskInput:
		if quest.Value != "" {
			hints = append(hints, key(component.KeyEnter)+text(" keeps it. ")+key("ctrl+u")+
				text(" clears it to type another."))
		}
	case AskChoose:
		if quest.rows > maxSelectHeight {
			hints = append(hints, key("↑↓")+text(" scrolls through all "+strconv.Itoa(len(quest.Options))+"."))
		}
	case AskConfirm:
		hints = append(hints, key("y")+text(" or ")+key("n")+text(" answers it."))
	}
	if env, ok := map[string]string{
		sandbox.QuestionClientID:     sandbox.EnvClientID,
		sandbox.QuestionClientSecret: sandbox.EnvClientSecret,
	}[quest.Title]; ok {
		hints = append(hints, text("Set "+env+" to skip this question."))
	}
	return strings.Join(hints, text(" "))
}

func (s *Setup) secretGoes() string {
	switch saving := s.state.Saving(); {
	case !saving.CanSave:
		return "It stays in memory until you quit."
	case saving.Saved:
		return "Setup saves it in your OS keychain."
	}
	return "Saving the settings puts it in your OS keychain."
}

// notes returns the wizard's latest notes, without blank lines.
func (s *Setup) notes() []string {
	lines, _ := s.ui.Logs.Log(state.SourceSetup).Snapshot()
	notes := make([]string, 0, len(lines))
	for _, line := range lines[min(s.notesFrom, len(lines)):] {
		if line = strings.TrimSpace(line); line != "" {
			notes = append(notes, line)
		}
	}
	return notes[max(len(notes)-noteCount, 0):]
}

// reviewLines puts each setting on one line, cut to width.
func (s *Setup) reviewLines(width int) string {
	theme := s.ui.Styles
	lines := make([]string, 0, len(sections()))
	for _, step := range s.Rail {
		lines = append(lines, ansi.Truncate(s.railMark(step)+"  "+theme.Text.Render(component.Pad(step.Title,
			checkNameWidth))+theme.Muted.Render(strings.Join(step.Answers, " · ")), width, "…"))
	}
	if len(lines) > 0 {
		return strings.Join(lines, "\n")
	}
	for _, sec := range sections() {
		lines = append(lines, ansi.Truncate(theme.OK.Render("✓")+"  "+theme.Text.Render(component.Pad(sec.title,
			checkNameWidth))+theme.Muted.Render(cmp.Or(s.state.Setup.Describe[sec.id], "not set")), width, "…"))
	}
	return strings.Join(lines, "\n")
}

// launchPanel says what launching does and how, or what to fix first.
func (s *Setup) launchPanel(width int) string {
	theme := s.ui.Styles
	para := lipgloss.NewStyle().Width(width)
	if len(s.state.Setup.Problems) > 0 {
		return lipgloss.JoinVertical(lipgloss.Left, theme.Attention.Bold(true).Render("Something needs a fix"),
			"", s.problemList(width), "", theme.KeyChip.Render("e")+" "+theme.Title.Render("Change a setting"))
	}
	kept := "Setup saved your answers."
	if !s.state.Saving().Saved {
		kept = "Setup keeps your answers until you quit."
	}
	lines := []string{
		theme.Title.Render("Ready to launch"),
		para.Render(theme.Muted.Render(kept + " Launching sets the sandbox up with them.")), "",
		theme.SectionTitle.Render("Launching"),
	}
	step := lipgloss.NewStyle().Width(max(width-len(stepIndent)-launchNumber, 1))
	for i, what := range []string{
		s.launchClusterStep(),
		"Sets up your zone: the sandbox's app, access rules and credentials",
		"Starts the agent, and checks that it works",
	} {
		lines = append(lines, lipgloss.JoinHorizontal(lipgloss.Top, stepIndent+theme.Dim.Render(strconv.Itoa(i+1))+"  ",
			step.Render(theme.Text.Render(what))))
	}
	return lipgloss.JoinVertical(lipgloss.Left, append(lines,
		indentLines(lipgloss.NewStyle().Width(max(width-len(stepIndent), 1)).Render(theme.Dim.Render(
			"It takes two to four minutes the first time. You can watch each step.",
		)), stepIndent), "",
		theme.KeyChip.Render(component.KeyEnter)+" "+theme.Title.Render("Launch")+"   "+
			theme.KeyChip.Render("e")+" "+theme.Text.Render("Change a setting"))...)
}

func (s *Setup) launchClusterStep() string {
	switch {
	case s.state.Existing():
		return "Checks your cluster, and puts the sandbox in it"
	case len(s.state.Run.Steps) > 0 && s.state.Run.Steps[0].Step.ID != sandbox.StepTunnel:
		return "Creates or checks the local cluster, which Keycard reaches at your issuer's URL"
	case s.state.Reads.Status.ClusterExists:
		return "Checks the local cluster and the tunnel"
	}
	return "Creates a local cluster, and a tunnel so Keycard can reach it"
}

// chosenLabel names the option that value picked, or is "" for none.
func chosenLabel(quest *Question, value string) string {
	for _, option := range quest.Options {
		if option.Value == value {
			return option.Label
		}
	}
	return ""
}

// answerLines fits an answer to width: cut to its first line, or wrapped.
func answerLines(answer string, width int, wrap bool) []string {
	if !wrap {
		return []string{ansi.Truncate(component.FirstLine(answer), width, "…")}
	}
	return strings.Split(strings.TrimRight(lipgloss.NewStyle().Width(width).Render(answer), " "), "\n")
}

// splitQuestion splits the sentence that asks, as the title, from the rest.
func splitQuestion(text string) (string, string) {
	sentences := sentencesOf(text)
	asks := max(slices.IndexFunc(sentences, func(s string) bool { return strings.HasSuffix(s, "?") }), 0)
	rest := slices.Delete(slices.Clone(sentences), asks, asks+1)
	return sentences[asks], strings.Join(rest, " ")
}

// sentencesOf splits text after each full stop or question mark that a
// space follows, so that a URL stays whole.
func sentencesOf(text string) []string {
	var sentences []string
	start := 0
	for i := range len(text) - 1 {
		if (text[i] == '.' || text[i] == '?') && text[i+1] == ' ' {
			sentences = append(sentences, text[start:i+1])
			start = i + len(". ")
		}
	}
	return append(sentences, text[start:])
}

// questionField returns the form field for a question, without its words,
// which the setup page draws around it. The answer goes in text, or in yes
// for a confirmation.
//
//nolint:ireturn // huh builds a form from its Field interface.
func questionField(quest *Question, text *string, yes *bool, width int) huh.Field {
	options := make([]huh.Option[string], 0, len(quest.Options))
	for _, option := range quest.Options {
		options = append(options, huh.NewOption(option.Label, option.Value))
	}
	switch quest.Kind {
	case AskSecret:
		return huh.NewInput().Prompt("").EchoMode(huh.EchoModePassword).Value(text).
			Validate(validator(quest.Check))
	case AskChoose:
		quest.rows = optionRows(quest.Options, width-style.SelectorWidth)
		return huh.NewSelect[string]().Options(options...).Value(text).
			Height(min(quest.rows, maxSelectHeight) + selectTitleRows)
	case AskConfirm:
		return huh.NewConfirm().Value(yes).Affirmative("Yes").Negative("No")
	case AskInput:
	}
	return huh.NewInput().Prompt("").Value(text).Validate(validator(quest.Check))
}

// optionRows counts the rows options take in a field width wide.
func optionRows(options []sandbox.Option, width int) int {
	inner := max(width-style.FieldFrame, 1)
	rows := 0
	for _, option := range options {
		rows += lipgloss.Height(lipgloss.Wrap(option.Label, inner, ",.-; "))
	}
	return rows
}

func validator(check func(string) error) func(string) error {
	if check == nil {
		return func(string) error { return nil }
	}
	return check
}

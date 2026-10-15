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
	"github.com/keycardai/agent-sandbox-examples/internal/tui/style"
)

const (
	// railWidth is the width of the wizard's step rail.
	railWidth = 30
	// noteCount is how many of the wizard's notes the question shows.
	noteCount = 5
	// railIndent is what the line and the space take left of an answer.
	railIndent = 3
	// reviewRailWidth is the width of the steps on the review, which shows
	// each answer whole.
	reviewRailWidth = 46
	// launchNumber is what a launch step's number and its space take.
	launchNumber = 3
	// railGap is the space between the wizard's steps and its question, and
	// minPanel the narrowest question panel the steps leave room for.
	railGap  = 6
	minPanel = 50
	// maxSelectHeight bounds the rows a choice shows at once. A choice's
	// height also counts a row for its title, which huh counts whether or
	// not there is one.
	maxSelectHeight, selectTitleRows = 10, 1
	// modalParts is how many parts a question's modal has besides the
	// notes.
	modalParts = 5
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
	// Answered sent the answer to the wizard.
	Answered
	// Stopped stops the setup.
	Stopped
)

// asking is the question that shows, its form, where the form keeps the
// answer, by kind, and whether it shows on the wizard's screen.
type asking struct {
	question *Question
	form     *huh.Form
	text     *string
	yes      *bool
	inWizard bool
}

// Setup is the setup wizard and the review after it. The wizard shows its
// steps on a rail and asks the current step's questions beside it. The
// review shows what was chosen at each step, and what launching does.
type Setup struct {
	shared *Shared
	// Rail is the wizard's steps, with what was chosen at each.
	Rail []*RailStep
	// current is the step the wizard asks about, and notesFrom where its
	// notes start in the setup log. The notes before it belong to the steps
	// before.
	current   string
	notesFrom int
	ask       *asking
}

// NewSetup returns the setup wizard and its review.
func NewSetup(shared *Shared) *Setup { return &Setup{shared: shared} }

// RailSteps are every step of the wizard, in plain words for someone new
// to Keycard.
func RailSteps() []*RailStep {
	return []*RailStep{
		{ID: sandbox.SectionAccount, Title: "Keycard account", About: "Setup signs in to Keycard with your " +
			"organization's service account. It uses the account only to set up the sandbox."},
		{ID: sandbox.SectionZone, Title: "Zone and user", About: "A zone is a Keycard workspace with its own " +
			"users and apps. Pick one you use for testing, because the sandbox turns on its own access rules " +
			"there. Then pick who the agent acts as, usually you. Every demo has an agent for them. Only people " +
			"who have signed in to the zone are listed."},
		{ID: sandbox.SectionScenario, Title: "Demo", About: "Pick what the agent does, from the demos in " +
			"this checkout."},
		{ID: sandbox.SectionCredentials, Title: "Credentials", About: "The keys the demo needs, such as a GitHub " +
			"token. The agent never sees them. The egress proxy adds them to its requests on the way out."},
		{ID: sandbox.SectionCluster, Title: wordCluster, About: "Where the agent runs. The default runs it on " +
			"this machine, and needs no other accounts."},
		{ID: sandbox.SectionPolicy, Title: "Zone policy", About: "A zone uses one set of access rules, called " +
			"a policy set, at a time. The sandbox needs its own set turned on, so that the agent can work for " +
			"the user."},
		{ID: sandbox.SectionSave, Title: "Save", About: "Saved settings come back the next time you start the " +
			"sandbox. Without saving, setup asks for them again next time."},
	}
}

// PlannedSteps are the steps a first run's wizard shows from the start.
// Zone policy joins only when the zone has another active policy set, and
// Save when setup asks whether to save.
func PlannedSteps() []*RailStep {
	return slices.DeleteFunc(RailSteps(), func(step *RailStep) bool {
		return step.ID == sandbox.SectionPolicy || step.ID == sandbox.SectionSave
	})
}

// Begin lays the rail out for the sections setup asks, or for every
// section when there are none.
func (s *Setup) Begin(ids ...string) {
	asked := map[string]bool{}
	for _, id := range ids {
		asked[id] = true
	}
	all := len(ids) == 0
	describe := s.shared.Session.Describe()
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

// Asked reports whether this setup asked the section id, rather than kept
// it.
func (s *Setup) Asked(id string) bool {
	return slices.ContainsFunc(s.Rail, func(step *RailStep) bool { return step.ID == id && step.State != RailKept })
}

// Asking reports whether a question waits for an answer.
func (s *Setup) Asking() bool { return s.ask != nil }

// OnSection marks the step setup moved on from as done, and the new one as
// the current step. A step that was not on the rail joins it.
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
	s.recordAnswer(msg.question, answer{text: msg.text})
}

// Finish marks the step setup was on as done.
func (s *Setup) Finish() {
	for _, step := range s.Rail {
		if step.State == RailCurrent {
			step.State = RailDone
		}
	}
}

// ForgetNotes starts the notes over, for questions outside the wizard.
func (s *Setup) ForgetNotes() { s.notesFrom = 0 }

// Ask shows the wizard's question as a form: inWizard on the wizard's
// screen, and otherwise inside a modal.
func (s *Setup) Ask(quest *Question, inWizard bool) tea.Cmd {
	shared := s.shared
	text, yes := quest.Value, quest.Yes
	// Outside the wizard, the question draws inside a modal, within its
	// border and padding.
	width := component.ModalInnerWidth(shared.Width)
	if inWizard {
		width = s.panelWidth()
	}
	// The wizard takes esc to stop the setup, so a list does not filter:
	// esc would stop the setup where it means to clear the filter.
	keys := huh.NewDefaultKeyMap()
	keys.Select.Filter.SetEnabled(false)
	// A field fits its text to its width less its theme's border and
	// padding, so it takes the theme first. Longer text scrolls within one
	// row.
	form := huh.NewForm(huh.NewGroup(questionField(quest, &text, &yes, width))).
		WithKeyMap(keys).
		WithShowHelp(false).
		WithTheme(shared.Styles.FormTheme(width, quest.Kind != AskConfirm)).
		WithWidth(width)
	// The form lays its field out once it knows the window. Telling it now
	// draws the whole field from the first frame.
	next, _ := form.Update(tea.WindowSizeMsg{Width: shared.Width, Height: shared.Height})
	if sized, ok := next.(*huh.Form); ok {
		form = sized
	}
	s.ask = &asking{question: quest, form: form, text: &text, yes: &yes, inWizard: inWizard}
	return form.Init()
}

// Resize lays the question out again for the window's size, keeping what
// is typed or chosen so far. A form keeps the width it started with.
func (s *Setup) Resize() tea.Cmd {
	if s.ask == nil {
		return nil
	}
	quest := s.ask.question
	quest.Value, quest.Yes = *s.ask.text, *s.ask.yes
	return s.Ask(quest, s.ask.inWizard)
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
		quest.reply <- got
		s.ask = nil
		return cmd, Answered
	case huh.StateAborted:
		s.ask = nil
		return cmd, Stopped
	case huh.StateNormal:
	}
	return cmd, Asking
}

// Drop takes the question away without an answer.
func (s *Setup) Drop() { s.ask = nil }

// WizardView draws the steps on the left and the current question on the
// right, in the middle of the window. A narrow window leaves the steps out:
// the header still says which step of how many.
func (s *Setup) WizardView() string {
	body := lipgloss.NewStyle().Width(s.panelWidth()).Render(lipgloss.JoinVertical(lipgloss.Left,
		s.questionPanel()...))
	if s.railFits() {
		body = lipgloss.JoinHorizontal(lipgloss.Top, s.railView(railWidth, false), strings.Repeat(" ", railGap), body)
	}
	return s.shared.Page(s.progress(), s.shared.anchored(body), s.wizardHints())
}

// ReviewView shows what setup chose and what launching does, in the
// middle of the window. The steps setup went through stand on the left as
// the wizard showed them, each with what was chosen, and the launch on the
// right. A narrow window puts each step on one line, over the launch.
func (s *Setup) ReviewView() string {
	shared := s.shared
	width := shared.Width - component.PageIndent*half
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
	if len(shared.Problems) == 0 {
		hints = append(hints, component.Hint{Key: component.KeyEnter, Desc: "launch"})
	}
	return shared.Page("setup · review", shared.centered(body), append(hints,
		component.Hint{Key: "e", Desc: "change a setting"},
		component.Hint{Key: component.KeyEsc, Desc: "dashboard"},
		component.Hint{Key: "q", Desc: wordQuit},
	))
}

// QuestionModal draws a setup question outside the wizard, under the
// wizard's latest notes, titled title.
func (s *Setup) QuestionModal(title string) string {
	shared := s.shared
	theme := shared.Styles
	width := component.ModalInnerWidth(shared.Width)
	notes := s.notes()
	parts := make([]string, 0, len(notes)+modalParts)
	parts = append(parts, theme.ModalTitle.Render(cmp.Or(title, "Setup")))
	for _, note := range notes {
		parts = append(parts, theme.Muted.Render("› "+note))
	}
	parts = append(parts, "", s.questionBlock(width), "", theme.Dim.Render("esc stops without saving"))
	return theme.Modal.Width(component.ModalWidth(shared.Width)).Render(lipgloss.JoinVertical(lipgloss.Left,
		parts...))
}

// recordAnswer puts what the operator chose under the current step, in
// words that stand on their own: a secret by what it is, and a yes or no
// with its question.
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

// railFits reports whether the steps fit beside a question panel of at
// least minPanel columns.
func (s *Setup) railFits() bool {
	return s.shared.Width-component.PageIndent*half-railWidth-railGap >= minPanel
}

// panelWidth is the question panel's width: beside the steps when they
// fit, and never wider than a paragraph reads well.
func (s *Setup) panelWidth() int {
	width := s.shared.Width - component.PageIndent*half
	if s.railFits() {
		width -= railWidth + railGap
	}
	return max(min(width, readWidth), 1)
}

// wizardHints lists the keys the question takes.
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

// railView draws the steps width wide as a stepper: a mark for each step's
// state, a line joining them, and what was chosen under each step that is
// done, cut to a line, or wrapped whole.
func (s *Setup) railView(width int, wrap bool) string {
	theme := s.shared.Styles
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

// railMark is a step's mark: done, the one asking, the one waiting on
// Keycard, or to come.
func (s *Setup) railMark(step *RailStep) string {
	theme := s.shared.Styles
	switch step.State {
	case RailCurrent:
		if s.ask == nil {
			return theme.Running.Render(s.shared.spinner())
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
	theme := s.shared.Styles
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

// questionPanel draws what the current step is for, the wizard's latest
// notes, and the question, or a spinner while setup works.
func (s *Setup) questionPanel() []string {
	theme := s.shared.Styles
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
		return append(parts, theme.Running.Render(s.shared.spinner())+theme.Muted.Render(" Working…"))
	}
	return append(parts, s.questionBlock(width))
}

// currentStep returns the step the wizard asks about, or nil.
func (s *Setup) currentStep() *RailStep {
	for _, step := range s.Rail {
		if step.ID == s.current {
			return step
		}
	}
	return nil
}

// questionBlock draws the question: the sentence that asks as its title,
// any other sentences under it, the answer's field, and how to answer.
func (s *Setup) questionBlock(width int) string {
	theme := s.shared.Styles
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

// questionHint says how to answer: what enter does with a value already
// there, where a secret goes, how a long list scrolls, which keys answer
// yes or no, and how to skip the account questions. Keys show in the
// accent.
func (s *Setup) questionHint(quest *Question) string {
	key, text := s.shared.Styles.Accent.Render, s.shared.Styles.Dim.Render
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
		"Service account client ID":     sandbox.EnvClientID,
		"Service account client secret": sandbox.EnvClientSecret,
	}[quest.Title]; ok {
		hints = append(hints, text("Set "+env+" to skip this question."))
	}
	return strings.Join(hints, text(" "))
}

// secretGoes says where a typed secret goes: in the keychain when the
// settings are saved, else in memory until the operator quits.
func (s *Setup) secretGoes() string {
	store := s.shared.Session.Local().Store
	switch {
	case store.Saved():
		return "Setup saves it in your OS keychain."
	case store.CanSave():
		return "Saving the settings puts it in your OS keychain."
	}
	return "It stays in memory until you quit."
}

// notes returns the wizard's latest notes, such as which zone it created,
// without blank lines.
func (s *Setup) notes() []string {
	lines, _ := s.shared.Logs.Log(SourceSetup).Snapshot()
	notes := make([]string, 0, len(lines))
	for _, line := range lines[min(s.notesFrom, len(lines)):] {
		if line = strings.TrimSpace(line); line != "" {
			notes = append(notes, line)
		}
	}
	return notes[max(len(notes)-noteCount, 0):]
}

// reviewLines puts each setting on one line, cut to width: the wizard's
// steps with what was chosen, or else the saved settings.
func (s *Setup) reviewLines(width int) string {
	shared := s.shared
	theme := shared.Styles
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
			checkNameWidth))+theme.Muted.Render(cmp.Or(shared.Describe[sec.id], "not set")), width, "…"))
	}
	return strings.Join(lines, "\n")
}

// launchPanel says the sandbox is ready, what launching does, and how to
// launch. With a problem left, it says what to fix in their place.
func (s *Setup) launchPanel(width int) string {
	shared := s.shared
	theme := shared.Styles
	para := lipgloss.NewStyle().Width(width)
	if len(shared.Problems) > 0 {
		return lipgloss.JoinVertical(lipgloss.Left, theme.Attention.Bold(true).Render("Something needs a fix"),
			"", shared.problemList(width), "", theme.KeyChip.Render("e")+" "+theme.Title.Render("Change a setting"))
	}
	kept := "Setup saved your answers."
	if !shared.Session.Local().Store.Saved() {
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

// launchClusterStep says what launching does with the cluster.
func (s *Setup) launchClusterStep() string {
	shared := s.shared
	switch {
	case shared.Session.Local().Existing():
		return "Checks your cluster, and puts the sandbox in it"
	case len(shared.Steps) > 0 && shared.Steps[0].Step.ID != sandbox.StepTunnel:
		return "Creates or checks the local cluster, which Keycard reaches at your issuer's URL"
	case shared.Status.ClusterExists:
		return "Checks the local cluster and the tunnel"
	}
	return "Creates a local cluster, and a tunnel so Keycard can reach it"
}

// chosenLabel names the option a choice's answer picked, or is "" when
// no option has the answer.
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

// splitQuestion takes the sentence that asks out of a question, as its
// title, and leaves the other sentences to say more. Without a sentence
// that asks, the first sentence is the title.
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
			start = i + half
		}
	}
	return append(sentences, text[start:])
}

// questionField returns the form field for a question, keeping the answer
// in text or yes by its kind: the field alone, since the question's
// words are drawn around it.
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
			Validate(validator(quest.check))
	case AskChoose:
		quest.rows = optionRows(quest.Options, width-style.SelectorWidth)
		return huh.NewSelect[string]().Options(options...).Value(text).
			Height(min(quest.rows, maxSelectHeight) + selectTitleRows)
	case AskConfirm:
		return huh.NewConfirm().Value(yes).Affirmative("Yes").Negative("No")
	case AskInput:
	}
	return huh.NewInput().Prompt("").Value(text).Validate(validator(quest.check))
}

// optionRows counts the rows a choice's options take in a field width
// wide, each wrapped as the field wraps it.
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

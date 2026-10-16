// Package style holds the TUI's colors and the styles made from them, and
// draws the Keycard mark.
package style

import (
	"image/color"

	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
)

// AppName is the product's name as a sentence writes it, such as "Quit
// Keycard sandbox?".
const AppName = "Keycard sandbox"

const (
	// FieldFrame is what a boxed field's border and padding take across.
	FieldFrame = 4
	// SelectorWidth is what the cursor before a choice's option takes.
	SelectorWidth = 2
	// ModalPadY is the space inside a modal's border, above and below.
	ModalPadY = 1
	// ModalPadX is the space inside a modal's border, left and right.
	ModalPadX = 2
)

// Palette is the Keycard console's colors, from its design system
// (@keycardlabs/ui-lanyard): Radix iris is the accent, slate the neutral,
// and green, red, amber and sky the success, error, warning and info
// scales. Each color means one thing here: iris is the brand and what has
// focus, sky is work in progress, green is done or allowed, red failed or
// denied, and amber needs the operator. The light values are the same
// scales' light steps.
type Palette struct {
	// Accent is iris 11, for text, and AccentFill iris 9, which fills a key
	// chip as it fills the console's buttons. OnAccent is the text on it.
	Accent, AccentFill, OnAccent color.Color
	// Selection is iris 3, the console's active row.
	Selection               color.Color
	Info, OK, Fail, Warn    color.Color
	Text, Muted, Dim, Faint color.Color
	Line, Band              color.Color
	// Tile is the brand surface the console puts the Keycard mark on. The
	// TUI puts the mark on it on a light terminal.
	Tile color.Color
}

// Styles holds every style the TUI draws with, made from the palette.
type Styles struct {
	Colors Palette
	// Dark is set for a terminal with a dark background.
	Dark bool

	// The header band and its parts. BandTabOn is the tab that shows, and
	// BandKey a tab's key.
	Band, Brand, BandLabel, BandValue, BandUp, BandBusy, BandDown, BandFail, BandTabOn, BandKey lipgloss.Style
	// Panes and rows.
	PaneTitle, PaneTitleFocused lipgloss.Style
	// Text by meaning.
	Text, Muted, Dim, Faint, OK, Running, Failed, Attention, Warn, Accent, Allow lipgloss.Style
	// The footer's keys and notices.
	KeyChip, KeyDesc, Toast, ToastError lipgloss.Style
	// Overlays and the onboarding screens.
	Modal, ModalTitle, Title, Hint, SectionTitle lipgloss.Style
}

// New returns the styles for a dark or a light terminal.
func New(isDark bool) Styles {
	pal := newPalette(isDark)
	paint := func(col color.Color) lipgloss.Style { return lipgloss.NewStyle().Foreground(col) }
	onBand := func(col color.Color) lipgloss.Style { return paint(col).Background(pal.Band) }
	return Styles{
		Colors:           pal,
		Dark:             isDark,
		Band:             onBand(pal.Text),
		Brand:            onBand(pal.Text).Bold(true),
		BandLabel:        onBand(pal.Dim),
		BandValue:        onBand(pal.Muted),
		BandUp:           onBand(pal.OK).Bold(true),
		BandBusy:         onBand(pal.Info).Bold(true),
		BandDown:         onBand(pal.Dim).Bold(true),
		BandFail:         onBand(pal.Fail).Bold(true),
		BandTabOn:        paint(pal.OnAccent).Background(pal.AccentFill).Bold(true),
		BandKey:          onBand(pal.Accent).Bold(true),
		PaneTitle:        paint(pal.Dim),
		PaneTitleFocused: paint(pal.Text).Bold(true),
		Text:             paint(pal.Text),
		Muted:            paint(pal.Muted),
		Dim:              paint(pal.Dim),
		Faint:            paint(pal.Faint),
		OK:               paint(pal.OK),
		Running:          paint(pal.Info),
		Failed:           paint(pal.Fail),
		Attention:        paint(pal.Warn),
		Warn:             paint(pal.Warn),
		Accent:           paint(pal.Accent),
		Allow:            paint(pal.OK),
		KeyChip:          paint(pal.OnAccent).Background(pal.AccentFill).Bold(true).Padding(0, 1),
		KeyDesc:          paint(pal.Muted),
		Toast:            paint(pal.OK),
		ToastError:       paint(pal.Fail),
		Modal: lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(pal.AccentFill).
			Padding(ModalPadY, ModalPadX),
		ModalTitle:   paint(pal.Text).Bold(true),
		Title:        paint(pal.Text).Bold(true),
		Hint:         paint(pal.Warn),
		SectionTitle: paint(pal.Accent).Bold(true),
	}
}

// FormTheme styles a setup question's field of width columns to match the
// console: iris for the cursor, red for an error. A field sits in a rounded
// box in iris, the way the console draws a focused input. The option under
// a choice's cursor fills its row with the selection color, as the
// console's active row does.
//
//nolint:ireturn // huh takes a form's theme as its Theme interface.
func (st Styles) FormTheme(width int) huh.Theme {
	pal := st.Colors
	return huh.ThemeFunc(func(isDark bool) *huh.Styles {
		theme := huh.ThemeBase(isDark)
		focused := &theme.Focused
		focused.Base = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(pal.AccentFill).Padding(0, 1)
		focused.Title = focused.Title.Foreground(pal.Text).Bold(true)
		focused.Description = focused.Description.Foreground(pal.Muted)
		focused.ErrorIndicator = focused.ErrorIndicator.Foreground(pal.Fail)
		focused.ErrorMessage = focused.ErrorMessage.Foreground(pal.Fail)
		focused.SelectSelector = lipgloss.NewStyle().SetString("▸ ").Foreground(pal.Accent).Background(pal.Selection)
		focused.Option = focused.Option.Foreground(pal.Muted)
		focused.SelectedOption = lipgloss.NewStyle().Foreground(pal.Text).Background(pal.Selection).
			Width(max(width-FieldFrame-SelectorWidth, 1))
		focused.UnselectedOption = focused.UnselectedOption.Foreground(pal.Muted)
		focused.TextInput.Cursor = focused.TextInput.Cursor.Foreground(pal.Accent)
		focused.TextInput.Prompt = focused.TextInput.Prompt.Foreground(pal.Accent)
		focused.TextInput.Text = focused.TextInput.Text.Foreground(pal.Text)
		focused.TextInput.Placeholder = focused.TextInput.Placeholder.Foreground(pal.Faint)
		theme.Blurred = theme.Focused
		theme.Help.ShortKey = theme.Help.ShortKey.Foreground(pal.Accent)
		theme.Help.ShortDesc = theme.Help.ShortDesc.Foreground(pal.Dim)
		theme.Help.ShortSeparator = theme.Help.ShortSeparator.Foreground(pal.Faint)
		return theme
	})
}

// Wordmark is the product's name for the header band.
func (st Styles) Wordmark() string {
	return st.Brand.Render("keycard") + st.BandLabel.Render(" sandbox")
}

// newPalette returns the colors for a dark or a light terminal.
func newPalette(isDark bool) Palette {
	pick := lipgloss.LightDark(isDark)
	hex := lipgloss.Color
	return Palette{
		Accent:     pick(hex("#5753C6"), hex("#B1A9FF")),
		AccentFill: hex("#5B5BD6"),
		OnAccent:   hex("#FFFFFF"),
		Selection:  pick(hex("#F0F1FE"), hex("#202248")),
		Info:       pick(hex("#00749E"), hex("#75C7F0")),
		OK:         pick(hex("#218358"), hex("#3DD68C")),
		Fail:       pick(hex("#CE2C31"), hex("#FF9592")),
		Warn:       pick(hex("#AB6400"), hex("#FFCA16")),
		Text:       pick(hex("#1C2024"), hex("#EDEEF0")),
		Muted:      pick(hex("#60646C"), hex("#B0B4BA")),
		Dim:        pick(hex("#80838D"), hex("#777B84")),
		Faint:      pick(hex("#B9BBC6"), hex("#5A6169")),
		Line:       pick(hex("#D9D9E0"), hex("#363A3F")),
		Band:       pick(hex("#F0F0F3"), hex("#18191B")),
		Tile:       hex("#2B3038"),
	}
}

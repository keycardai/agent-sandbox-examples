package style

import (
	"image/color"
	"math"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// keycardMark is Keycard's mark, the line-art monogram the console's Logo
// masks (ui-lanyard lanyard-logo.css), as coverage: one hex digit a pixel,
// from blank for none to f for full. A terminal row holds two pixel rows,
// drawn with half blocks. The grid is 30x26 pixels. At that height the
// horizontal lines fall on whole pixels. The grid is a little wider than
// the mark's 24.3:21, because a half block is taller than it is wide in
// most terminal fonts. Each vertical bar sits on one column, and each
// diagonal is two pixels a row at coverage c, so that it looks as heavy as
// a straight line.
//
//nolint:gochecknoglobals,dupword // The mark is read only, and its rows repeat.
var keycardMark = []string{
	"a  b   b  b      cc   cc   cc ",
	"f  f   f  f     cc   cc   cc  ",
	"f  f   f  f    cc   cc   cc  2",
	"f  f   f  f   cc   cc   cc  cc",
	"f            cc   cc   cc  cc ",
	"f           cc   cc  cc   cc  ",
	"bfffffffffff8   cc  cc   cc   ",
	"              cc   cc   cc  cc",
	"             cc   cc   cc  cc ",
	"fffffffffffffc   cc  cc   cc  ",
	"                cc  cc   cc   ",
	"               cc  cc   cc    ",
	"effffffffffffff8   cc  cc     ",
	"               cc  cc   cc    ",
	"                cc  cc   cc   ",
	"fffffffffffffb   cc   cc  cc  ",
	"             cc   cc   cc  cc ",
	"              cc   cc   cc  cc",
	"cfffffffffff8   cc  cc   cc   ",
	"f           cc   cc  cc   cc  ",
	"f            cc   cc   cc  cc ",
	"f  f   f  f   cc   cc   cc  cc",
	"f  f   f  f    cc   cc   cc  2",
	"f  f   f  f     cc   cc   cc  ",
	"a  b   c  c      cc   cc   cc ",
	"                              ",
}

// iridescent is the console's holographic paint, --lanyard-iridescent: a
// conic gradient around the mark's center, from conicFrom. Each stop is an
// angle and a color.
//
//nolint:gochecknoglobals // The stops are read only.
var iridescent = []struct {
	deg float64
	rgb rgb
}{
	{0, hexRGB("#F1C6DD")},
	{50.06, hexRGB("#C1A4E8")},
	{85.94, hexRGB("#B8E2FB")},
	{134.97, hexRGB("#F2EFE8")},
	{172.05, hexRGB("#F9DCDD")},
	{200.75, hexRGB("#E1C1E5")},
	{224.67, hexRGB("#BDAFE3")},
	{259.36, hexRGB("#C7EDEB")},
	{298.82, hexRGB("#E7F5EB")},
	{328.72, hexRGB("#F2F0E7")},
	{360, hexRGB("#DDC1E1")},
}

const (
	// markWidth and markRows are the mark's size in cells.
	markWidth = 30
	markRows  = 13
	// logoPadX is the tile's padding left and right of the mark, on a light
	// terminal.
	logoPadX = 2
	// conicFrom is where the iridescent gradient starts, clockwise from
	// the top.
	conicFrom = 210
	// edgeGamma thickens the mark's thin diagonals: a pixel's coverage is
	// raised to it.
	edgeGamma = 0.6
	// coverageLevels is the coverage of a full pixel.
	coverageLevels = 15
	hexBase        = 16
	// halfway is a pixel's middle, and the middle of a gradient, and half
	// halves a size.
	halfway = 0.5
	half    = 2
	// fullCircle and halfCircle are turns in degrees, and radian is one
	// degree in radians.
	fullCircle = 360
	halfCircle = 180
	radian     = math.Pi / halfCircle
	// The console's loading sheen: a band of white at sheenAngle, which
	// fades out sheenBand from its middle, and sweeps across the mark from
	// sheenFrom to -sheenFrom of its width. The console's band is 0.1 wide.
	// The mark's pixels are coarse, so its band is wider.
	sheenAngle = 110
	sheenBand  = 0.14
	sheenFrom  = -1.01
	// sheenPeak is the band's white at its middle. The console's is 0.25
	// over a dark tile. The mark alone is pale, so it takes more.
	sheenPeak = 0.9
	// sheenSweep is how long one sweep takes, and sheenCycle how often one
	// starts while the machine checks run. sheenEvery is a frame.
	sheenSweep = 700 * time.Millisecond
	sheenCycle = 2 * time.Second
	sheenEvery = time.Second / 30
)

// rgb is a color as 0 to 255 for each channel.
type rgb struct{ r, g, b float64 }

func hexRGB(hex string) rgb { return toRGB(lipgloss.Color(hex)) }

// toRGB reads a color.Color, which holds 16 bits a channel.
func toRGB(c color.Color) rgb {
	const shift = 8
	r, g, b, _ := c.RGBA()
	return rgb{float64(r >> shift), float64(g >> shift), float64(b >> shift)}
}

// mix goes from a to b by t, 0 to 1.
func (a rgb) mix(b rgb, t float64) rgb {
	return rgb{a.r + (b.r-a.r)*t, a.g + (b.g-a.g)*t, a.b + (b.b-a.b)*t}
}

func (a rgb) color() color.Color {
	return color.RGBA{R: channel(a.r), G: channel(a.g), B: channel(a.b), A: math.MaxUint8}
}

func channel(v float64) uint8 {
	return uint8(math.Round(math.Max(0, math.Min(v, math.MaxUint8))))
}

// Glint is the loading sheen across the mark: when the sweep started, and
// where it is, as a fraction of the mark's width. It shows only while on.
type Glint struct {
	start time.Time
	at    float64
	on    bool
}

// GlintMsg moves the sheen.
type GlintMsg struct{ Now time.Time }

// Start sweeps the sheen across the mark, as the console does while a page
// loads.
func (g *Glint) Start() tea.Cmd {
	if g.on {
		return nil
	}
	*g = Glint{start: time.Now(), at: sheenFrom, on: true}
	return glintTick(sheenEvery)
}

// Move moves the sheen to where it is at now. While again is set, it
// sweeps again every sheenCycle. Without it, it stops once a sweep is done.
func (g *Glint) Move(now time.Time, again bool) tea.Cmd {
	if !g.on {
		return nil
	}
	elapsed := now.Sub(g.start)
	if elapsed >= sheenCycle && again {
		g.start, elapsed = now, 0
	}
	if elapsed < sheenSweep {
		progress := float64(elapsed) / float64(sheenSweep)
		g.at = sheenFrom * (1 - half*easeInOut(progress))
		return glintTick(sheenEvery)
	}
	g.at = -sheenFrom
	if !again {
		g.on = false
		return nil
	}
	return glintTick(g.start.Add(sheenCycle).Sub(now))
}

func glintTick(after time.Duration) tea.Cmd {
	return tea.Tick(after, func(now time.Time) tea.Msg { return GlintMsg{Now: now} })
}

// easeInOut is CSS's ease-in-out, near enough: slow at both ends.
func easeInOut(t float64) float64 {
	const a, b = 3, 2
	return t * t * (a - b*t)
}

// Logo draws the mark in the console's iridescent paint, in styles, with
// the sheen where g is. On a dark terminal it sits on the page. Once the
// terminal reports its background, the mark's edges blend into it. Until
// then, a pixel shows when it is at least half covered. On a
// light terminal the pale mark sits on the brand's dark tile, as the
// console's favicon does on a light browser.
func Logo(styles Styles, background color.Color, g Glint) string {
	painter := markPainter{glint: g}
	switch {
	case !styles.Dark:
		painter.base, painter.known, painter.tile = toRGB(styles.Colors.Tile), true, true
	case background != nil:
		painter.base, painter.known = toRGB(background), true
	}
	lines := make([]string, 0, markRows)
	for row := range markRows {
		var line strings.Builder
		for col := range markWidth {
			line.WriteString(painter.cell(col, row))
		}
		lines = append(lines, line.String())
	}
	if !painter.tile {
		return strings.Join(lines, "\n")
	}
	tile := lipgloss.NewStyle().Background(styles.Colors.Tile)
	pad := tile.Render(strings.Repeat(" ", logoPadX))
	blank := tile.Render(strings.Repeat(" ", markWidth+logoPadX*half))
	for i, line := range lines {
		lines[i] = pad + line + pad
	}
	return strings.Join(append(append([]string{blank}, lines...), blank), "\n")
}

// LogoWidth is the logo's width in cells, with the tile's padding on a
// light terminal.
func LogoWidth(styles Styles) int {
	if styles.Dark {
		return markWidth
	}
	return markWidth + logoPadX*half
}

// markPainter colors the mark's pixels. base is what the mark is drawn
// on, when known. tile paints base behind every cell.
type markPainter struct {
	base        rgb
	known, tile bool
	glint       Glint
}

// cell draws two pixels, one above the other, as one half block.
func (p markPainter) cell(col, row int) string {
	top, topOn := p.pixel(col, row*half)
	bottom, bottomOn := p.pixel(col, row*half+1)
	style := lipgloss.NewStyle()
	if p.tile {
		style = style.Background(p.base.color())
	}
	switch {
	case topOn && bottomOn:
		return style.Foreground(top.color()).Background(bottom.color()).Render("▀")
	case topOn:
		return style.Foreground(top.color()).Render("▀")
	case bottomOn:
		return style.Foreground(bottom.color()).Render("▄")
	}
	return style.Render(" ")
}

// pixel returns the color of the mark at col, line, and whether the mark
// covers it at all.
func (p markPainter) pixel(col, line int) (rgb, bool) {
	level, err := strconv.ParseInt(string(keycardMark[line][col]), hexBase, 0)
	if err != nil || level == 0 {
		return rgb{}, false
	}
	cover := math.Pow(float64(level)/coverageLevels, edgeGamma)
	if !p.known {
		if cover < halfway {
			return rgb{}, false
		}
		cover = 1
	}
	paint := p.paint(float64(col)+halfway, float64(line)+halfway)
	return p.base.mix(paint, cover), true
}

// paint is the iridescent gradient at a point across and down the mark,
// with the sheen over it.
func (p markPainter) paint(across, down float64) rgb {
	width, height := float64(markWidth), float64(markRows*half)
	deg := math.Atan2(across-width*halfway, height*halfway-down) / radian
	deg = math.Mod(deg-conicFrom+fullCircle*half, fullCircle)
	paint := iridescent[len(iridescent)-1].rgb
	for i := 1; i < len(iridescent); i++ {
		if from, to := iridescent[i-1], iridescent[i]; deg <= to.deg {
			paint = from.rgb.mix(to.rgb, (deg-from.deg)/(to.deg-from.deg))
			break
		}
	}
	if p.glint.on {
		paint = paint.mix(hexRGB("#FFFFFF"), sheenAt(across-p.glint.at*width, down, width, height))
	}
	return paint
}

// sheenAt is the sheen's white at x, y of a box: CSS's linear-gradient at
// sheenAngle, clear until 50%-sheenBand, sheenPeak at 50%, and clear again
// from 50%+sheenBand.
func sheenAt(x, y, width, height float64) float64 {
	dx, dy := math.Sin(sheenAngle*radian), -math.Cos(sheenAngle*radian)
	length := width*math.Abs(dx) + height*math.Abs(dy)
	along := ((x-width*halfway)*dx+(y-height*halfway)*dy)/length + halfway
	return sheenPeak * math.Max(0, 1-math.Abs(along-halfway)/sheenBand)
}

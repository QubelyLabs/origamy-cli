// Package ui provides a small, dependency-free terminal UI toolkit: ANSI
// styling and an animated spinner. Everything degrades gracefully when stdout
// is not a TTY (e.g. piped through `install.sh | sh`) or when NO_COLOR is set —
// no escape codes, no carriage-return animation, just plain readable lines.
package ui

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// ── color / TTY detection ───────────────────────────────────────────────────

var (
	isTTY   = detectTTY()
	noColor = os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb"
	enabled = isTTY && !noColor
)

func detectTTY() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

// ANSI codes, applied only when color is enabled.
const (
	cReset  = "\033[0m"
	cBold   = "\033[1m"
	cDim    = "\033[2m"
	cRed    = "\033[31m"
	cGreen  = "\033[32m"
	cYellow = "\033[33m"
	cBlue   = "\033[34m"
	cCyan   = "\033[36m"
	cGray   = "\033[90m"
)

func paint(code, s string) string {
	if !enabled {
		return s
	}
	return code + s + cReset
}

// Style helpers (safe to use in any output; no-op without color).
func Bold(s string) string   { return paint(cBold, s) }
func Dim(s string) string    { return paint(cDim, s) }
func Red(s string) string    { return paint(cRed, s) }
func Green(s string) string  { return paint(cGreen, s) }
func Yellow(s string) string { return paint(cYellow, s) }
func Blue(s string) string   { return paint(cBlue, s) }
func Cyan(s string) string   { return paint(cCyan, s) }
func Gray(s string) string   { return paint(cGray, s) }

// ── one-shot line helpers ───────────────────────────────────────────────────

// Title prints a bold heading with a blank line above it.
func Title(format string, a ...any) {
	fmt.Printf("\n%s\n", Bold(fmt.Sprintf(format, a...)))
}

// Step prints a dimmed bullet line (informational, non-animated).
func Step(format string, a ...any) {
	fmt.Printf("  %s %s\n", Gray("•"), fmt.Sprintf(format, a...))
}

// Success prints a green check line.
func Success(format string, a ...any) {
	fmt.Printf("  %s %s\n", Green("✓"), fmt.Sprintf(format, a...))
}

// Warn prints a yellow warning line.
func Warn(format string, a ...any) {
	fmt.Printf("  %s %s\n", Yellow("▲"), fmt.Sprintf(format, a...))
}

// Fail prints a red cross line.
func Fail(format string, a ...any) {
	fmt.Printf("  %s %s\n", Red("✗"), fmt.Sprintf(format, a...))
}

// Detail prints an indented, dimmed continuation line (under a step).
func Detail(format string, a ...any) {
	fmt.Printf("      %s\n", Gray(fmt.Sprintf(format, a...)))
}

// KV prints an aligned key/value line for the intro block.
func KV(key, value string) {
	fmt.Printf("  %s  %s\n", Gray(fmt.Sprintf("%-12s", key)), value)
}

// ── spinner ─────────────────────────────────────────────────────────────────

var spinFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// Spinner is an animated single-line progress indicator. On a non-TTY it prints
// a single static line on Start and a result line on Stop — no animation.
type Spinner struct {
	mu     sync.Mutex
	msg    string
	suffix string
	active bool
	stop   chan struct{}
	done   chan struct{}
}

// Start begins a spinner with the given message.
func Start(format string, a ...any) *Spinner {
	s := &Spinner{msg: fmt.Sprintf(format, a...)}
	if !enabled {
		fmt.Printf("  %s %s ...\n", Gray("•"), s.msg)
		return s
	}
	s.active = true
	s.stop = make(chan struct{})
	s.done = make(chan struct{})
	go s.loop()
	return s
}

func (s *Spinner) loop() {
	defer close(s.done)
	t := time.NewTicker(80 * time.Millisecond)
	defer t.Stop()
	i := 0
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			s.mu.Lock()
			frame := spinFrames[i%len(spinFrames)]
			line := fmt.Sprintf("\r  %s %s", Cyan(frame), s.msg)
			if s.suffix != "" {
				line += " " + Gray(s.suffix)
			}
			fmt.Print(line + "\033[K") // clear to end of line
			s.mu.Unlock()
			i++
		}
	}
}

// Suffix updates the trailing progress text (e.g. "12/16 ready").
func (s *Spinner) Suffix(format string, a ...any) {
	s.mu.Lock()
	s.suffix = fmt.Sprintf(format, a...)
	s.mu.Unlock()
}

// Message replaces the spinner's main message.
func (s *Spinner) Message(format string, a ...any) {
	s.mu.Lock()
	s.msg = fmt.Sprintf(format, a...)
	s.mu.Unlock()
}

func (s *Spinner) finish(symbol, text string) {
	if !enabled {
		fmt.Printf("  %s %s\n", symbol, text)
		return
	}
	if s.active {
		close(s.stop)
		<-s.done
		s.active = false
	}
	fmt.Printf("\r  %s %s\033[K\n", symbol, text)
}

// Success stops the spinner with a green check and a final message.
func (s *Spinner) Success(format string, a ...any) {
	s.finish(Green("✓"), fmt.Sprintf(format, a...))
}

// Fail stops the spinner with a red cross and a final message.
func (s *Spinner) Fail(format string, a ...any) {
	s.finish(Red("✗"), fmt.Sprintf(format, a...))
}

// Warn stops the spinner with a yellow marker and a final message.
func (s *Spinner) Warn(format string, a ...any) {
	s.finish(Yellow("▲"), fmt.Sprintf(format, a...))
}

// ── boxed summary ───────────────────────────────────────────────────────────

// Box prints a titled, bordered panel of lines. Falls back to plain indented
// lines without color/TTY.
func Box(title string, lines []string) {
	if !enabled {
		fmt.Printf("\n%s\n", title)
		for _, l := range lines {
			fmt.Printf("  %s\n", stripANSI(l))
		}
		fmt.Println()
		return
	}
	width := len(title)
	for _, l := range lines {
		if w := visibleLen(l); w > width {
			width = w
		}
	}
	width += 2
	top := "╭" + strings.Repeat("─", width) + "╮"
	bot := "╰" + strings.Repeat("─", width) + "╯"
	fmt.Printf("\n%s\n", Gray(top))
	fmt.Printf("%s %s%s %s\n", Gray("│"), Bold(title), strings.Repeat(" ", width-len(title)-1), Gray("│"))
	for _, l := range lines {
		pad := width - visibleLen(l) - 1
		if pad < 0 {
			pad = 0
		}
		fmt.Printf("%s %s%s %s\n", Gray("│"), l, strings.Repeat(" ", pad), Gray("│"))
	}
	fmt.Printf("%s\n\n", Gray(bot))
}

// visibleLen returns the printable width of a string, ignoring ANSI codes.
func visibleLen(s string) int { return len([]rune(stripANSI(s))) }

func stripANSI(s string) string {
	var b strings.Builder
	inEsc := false
	for _, r := range s {
		if r == '\033' {
			inEsc = true
			continue
		}
		if inEsc {
			if r == 'm' {
				inEsc = false
			}
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

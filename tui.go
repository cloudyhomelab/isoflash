package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/charmbracelet/bubbles/progress"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

const (
	// Maximum width of the progress bar in cells.
	barWidth = 50
	// Cells kept free beside the progress bar on narrow terminals.
	barMargin = 4
	// How often the drive list is rescanned, so a drive plugged in later shows up.
	rescanInterval = 2 * time.Second
	// Path completions visible under the ISO input at once.
	maxMatches = 8
)

var (
	// Title text, light on the accent background.
	colorTitleText = lipgloss.Color("230")
	// Title background, active step, cursor and spinner.
	colorAccent = lipgloss.Color("63")
	// Erase warning and failures.
	colorDanger = lipgloss.Color("196")
	// Success.
	colorOK = lipgloss.Color("42")
	// Notices the user must act on.
	colorWarn = lipgloss.Color("214")
	// Help line and secondary text.
	colorMuted = lipgloss.Color("241")
)

var (
	titleStyle  = lipgloss.NewStyle().Bold(true).Foreground(colorTitleText).Background(colorAccent).Padding(0, 1)
	accentStyle = lipgloss.NewStyle().Bold(true).Foreground(colorAccent)
	mutedStyle  = lipgloss.NewStyle().Foreground(colorMuted)
	warnStyle   = lipgloss.NewStyle().Foreground(colorWarn)
	okStyle     = lipgloss.NewStyle().Bold(true).Foreground(colorOK)
	dangerStyle = lipgloss.NewStyle().Bold(true).Foreground(colorDanger)
	dangerBox   = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colorDanger).Padding(0, 2)
)

type step int

const (
	stepDrive step = iota
	stepISO
	stepConfirm
	stepWriting
	stepDone
	stepFailed
)

var stepNames = []string{"Drive", "Image", "Confirm", "Write"}

type disksMsg struct {
	disks []disk
	err   error
}

type match struct {
	value   string
	label   string
	isDir   bool
	isImage bool
}

type model struct {
	step   step
	notice string

	disks   []disk
	cursor  int
	scanErr error
	drive   disk

	isoInput    textinput.Model
	matches     []match
	matchCursor int
	iso         string
	isoSize     int64

	confirmInput textinput.Model

	bar        progress.Model
	spin       spinner.Model
	progress   chan tea.Msg
	cancel     context.CancelFunc
	started    time.Time
	elapsed    time.Duration
	written    int64
	flushing   bool
	abortArmed bool
	aborting   bool
	err        error
}

func newModel() model {
	iso := textinput.New()
	iso.Prompt = "› "
	iso.Placeholder = "~/Downloads/image.iso"

	confirm := textinput.New()
	confirm.Prompt = "› "
	confirm.Placeholder = "/dev/…"

	bar := progress.New(progress.WithDefaultGradient(), progress.WithWidth(barWidth))
	spin := spinner.New(spinner.WithSpinner(spinner.Dot), spinner.WithStyle(accentStyle))
	return model{isoInput: iso, confirmInput: confirm, bar: bar, spin: spin}
}

func scanDisks() tea.Msg {
	disks, err := listUSBDisks()
	return disksMsg{disks: disks, err: err}
}

func waitFor(ch <-chan tea.Msg) tea.Cmd {
	return func() tea.Msg { return <-ch }
}

func (m model) Init() tea.Cmd {
	return tea.Batch(scanDisks, m.spin.Tick)
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.bar.Width = min(barWidth, msg.Width-barMargin)
		return m, nil
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return m, cmd
	case disksMsg:
		return m.onDisks(msg)
	case progressMsg:
		m.written, m.flushing = msg.written, msg.flushing
		return m, waitFor(m.progress)
	case writeDoneMsg:
		m.written = msg.written
		m.elapsed = time.Since(m.started)
		m.notice = ""
		m.err = msg.err
		m.step = stepDone
		if msg.err != nil {
			m.step = stepFailed
		}
		return m, tea.Quit
	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			return m.onCtrlC()
		}
		switch m.step {
		case stepDrive:
			return m.updateDrive(msg)
		case stepISO:
			return m.updateISO(msg)
		case stepConfirm:
			return m.updateConfirm(msg)
		case stepWriting:
			m.abortArmed = false
			m.notice = ""
		}
		return m, nil
	}
	return m.updateInputs(msg)
}

// updateInputs forwards non-key messages, such as cursor blinks, to the focused input.
func (m model) updateInputs(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	switch m.step {
	case stepISO:
		m.isoInput, cmd = m.isoInput.Update(msg)
	case stepConfirm:
		m.confirmInput, cmd = m.confirmInput.Update(msg)
	}
	return m, cmd
}

func (m model) onDisks(msg disksMsg) (tea.Model, tea.Cmd) {
	m.scanErr = msg.err
	if msg.err == nil {
		selected := ""
		if m.cursor < len(m.disks) {
			selected = m.disks[m.cursor].Path
		}
		m.disks = msg.disks
		m.cursor = 0
		for i, d := range m.disks {
			if d.Path == selected {
				m.cursor = i
			}
		}
	}
	rescan := tea.Tick(rescanInterval, func(time.Time) tea.Msg { return scanDisks() })
	return m, rescan
}

func (m model) onCtrlC() (tea.Model, tea.Cmd) {
	if m.step != stepWriting {
		return m, tea.Quit
	}
	if m.aborting {
		return m, nil
	}
	if !m.abortArmed {
		m.abortArmed = true
		m.notice = "Press Ctrl+C again to abort. The drive will be unusable until it is written again."
		return m, nil
	}
	m.aborting = true
	m.notice = "Aborting…"
	m.cancel()
	return m, nil
}

func (m model) updateDrive(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up", "k":
		m.cursor = max(m.cursor-1, 0)
		m.notice = ""
	case "down", "j":
		m.cursor = max(min(m.cursor+1, len(m.disks)-1), 0)
		m.notice = ""
	case "q", "esc":
		return m, tea.Quit
	case "enter":
		if len(m.disks) == 0 {
			return m, nil
		}
		d := m.disks[m.cursor]
		if len(d.Mounts) > 0 {
			m.notice = fmt.Sprintf("%s is mounted at %s. Unmount it first.", d.Path, strings.Join(d.Mounts, ", "))
			return m, nil
		}
		m.drive = d
		m.step = stepISO
		m.notice = ""
		m.refreshMatches()
		return m, m.isoInput.Focus()
	}
	return m, nil
}

func (m model) updateISO(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.step = stepDrive
		m.notice = ""
		m.isoInput.Blur()
		return m, nil
	case "up":
		m.matchCursor = max(m.matchCursor-1, 0)
		return m, nil
	case "down":
		m.matchCursor = max(min(m.matchCursor+1, len(m.matches)-1), 0)
		return m, nil
	case "tab":
		m.acceptMatch()
		return m, nil
	case "enter":
		return m.submitISO()
	}
	before := m.isoInput.Value()
	var cmd tea.Cmd
	m.isoInput, cmd = m.isoInput.Update(msg)
	if m.isoInput.Value() != before {
		m.notice = ""
		m.refreshMatches()
	}
	return m, cmd
}

func (m *model) acceptMatch() {
	if len(m.matches) == 0 {
		return
	}
	m.isoInput.SetValue(m.matches[m.matchCursor].value)
	m.isoInput.CursorEnd()
	m.notice = ""
	m.refreshMatches()
}

func (m model) submitISO() (tea.Model, tea.Cmd) {
	path := expandUser(strings.TrimSpace(m.isoInput.Value()))
	info, err := os.Stat(path)
	switch {
	case path == "":
		m.notice = "Type the path to an ISO image."
	case errors.Is(err, fs.ErrNotExist):
		m.notice = "No such file: " + path
	case err != nil:
		m.notice = err.Error()
	case info.IsDir():
		// Enter on a folder steps into it, like Tab.
		m.acceptMatch()
	case !info.Mode().IsRegular():
		m.notice = path + " is not a regular file."
	case info.Size() > m.drive.Size:
		m.notice = fmt.Sprintf("The image (%s) is larger than the drive (%s).",
			formatBytes(info.Size()), formatBytes(m.drive.Size))
	default:
		m.iso = path
		m.isoSize = info.Size()
		m.step = stepConfirm
		m.notice = ""
		m.isoInput.Blur()
		m.confirmInput.Reset()
		return m, m.confirmInput.Focus()
	}
	return m, nil
}

func (m *model) refreshMatches() {
	m.matches = completePath(m.isoInput.Value())
	m.matchCursor = 0
}

// completePath lists entries matching the typed prefix: images first, then
// folders, then other files. Values keep the user's spelling (e.g. "~/").
func completePath(typed string) []match {
	if typed == "~" {
		return []match{{value: "~/", label: "~/", isDir: true}}
	}
	expanded := expandUser(typed)
	dir, prefix := filepath.Split(expanded)
	typedDir := typed[:len(typed)-len(prefix)]
	readDir := dir
	if readDir == "" {
		readDir = "."
	}
	entries, err := os.ReadDir(readDir)
	if err != nil {
		return nil
	}

	var images, dirs, others []match
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, prefix) || (strings.HasPrefix(name, ".") && !strings.HasPrefix(prefix, ".")) {
			continue
		}
		// Stat, not e.Info(), so symlinks to folders count as folders.
		info, err := os.Stat(filepath.Join(readDir, name))
		if err != nil {
			continue
		}
		switch {
		case info.IsDir():
			dirs = append(dirs, match{value: typedDir + name + "/", label: name + "/", isDir: true})
		case isImage(name):
			images = append(images, match{value: typedDir + name, label: name, isImage: true})
		default:
			others = append(others, match{value: typedDir + name, label: name})
		}
	}
	return append(append(images, dirs...), others...)
}

func isImage(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	return ext == ".iso" || ext == ".img"
}

func (m model) updateConfirm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.step = stepISO
		m.notice = ""
		m.confirmInput.Blur()
		return m, m.isoInput.Focus()
	case "enter":
		if strings.TrimSpace(m.confirmInput.Value()) != m.drive.Path {
			m.notice = fmt.Sprintf("That doesn't match %s.", m.drive.Path)
			return m, nil
		}
		return m.startWrite()
	}
	var cmd tea.Cmd
	m.confirmInput, cmd = m.confirmInput.Update(msg)
	m.notice = ""
	return m, cmd
}

func (m model) startWrite() (tea.Model, tea.Cmd) {
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.progress = make(chan tea.Msg)
	m.started = time.Now()
	m.step = stepWriting
	m.notice = ""
	m.confirmInput.Blur()
	go writeImage(ctx, m.iso, m.drive.Path, m.progress)
	return m, waitFor(m.progress)
}

func (m model) View() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("write-iso-usb") + "  " + m.stepsView() + "\n\n")
	switch m.step {
	case stepDrive:
		b.WriteString(m.driveView())
	case stepISO:
		b.WriteString(m.isoView())
	case stepConfirm:
		b.WriteString(m.confirmView())
	case stepWriting:
		b.WriteString(m.writingView())
	case stepDone:
		b.WriteString(m.doneView())
	case stepFailed:
		b.WriteString(m.failedView())
	}
	if m.notice != "" {
		b.WriteString("\n" + warnStyle.Render(m.notice) + "\n")
	}
	if help := m.help(); help != "" {
		b.WriteString("\n" + mutedStyle.Render(help) + "\n")
	}
	return b.String()
}

func (m model) stepsView() string {
	current := min(int(m.step), len(stepNames)-1)
	parts := make([]string, len(stepNames))
	for i, name := range stepNames {
		label := fmt.Sprintf("%d %s", i+1, name)
		if i == current {
			parts[i] = accentStyle.Render(label)
		} else {
			parts[i] = mutedStyle.Render(label)
		}
	}
	return strings.Join(parts, mutedStyle.Render(" › "))
}

func (m model) help() string {
	switch m.step {
	case stepDrive:
		return "↑/↓ move · enter choose · q quit"
	case stepISO:
		return "tab complete · ↑/↓ pick · enter next · esc back · ctrl+c quit"
	case stepConfirm:
		return "enter start writing · esc back · ctrl+c quit"
	case stepWriting:
		return "ctrl+c abort"
	}
	return ""
}

func driveSummary(d disk) string {
	name := d.Model
	if name == "" {
		name = "unknown model"
	}
	return fmt.Sprintf("%s · %s · %s", d.Path, formatBytes(d.Size), name)
}

func (m model) driveView() string {
	var b strings.Builder
	if m.scanErr != nil {
		b.WriteString(dangerStyle.Render("Can't list drives: "+m.scanErr.Error()) + "\n")
	}
	if len(m.disks) == 0 {
		b.WriteString(m.spin.View() + " Waiting for a USB drive… plug one in.\n")
		return b.String()
	}
	b.WriteString("Choose the USB drive to write to:\n\n")
	for i, d := range m.disks {
		row := "  " + driveSummary(d)
		if i == m.cursor {
			row = accentStyle.Render("› " + driveSummary(d))
		}
		if len(d.Mounts) > 0 {
			row += warnStyle.Render("  (mounted)")
		}
		b.WriteString(row + "\n")
	}
	b.WriteString("\n" + mutedStyle.Render("Only USB and removable drives are listed. New drives appear automatically.") + "\n")
	return b.String()
}

func (m model) isoView() string {
	var b strings.Builder
	b.WriteString(mutedStyle.Render("Drive  ") + driveSummary(m.drive) + "\n\n")
	b.WriteString("Path to the ISO image:\n")
	b.WriteString(m.isoInput.View() + "\n")

	start := max(m.matchCursor-maxMatches+1, 0)
	end := min(start+maxMatches, len(m.matches))
	for i := start; i < end; i++ {
		mt := m.matches[i]
		label := mt.label
		switch {
		case i == m.matchCursor:
			label = accentStyle.Render("› " + label)
		case mt.isImage:
			label = "  " + lipgloss.NewStyle().Bold(true).Render(label)
		case mt.isDir:
			label = "  " + label
		default:
			label = "  " + mutedStyle.Render(label)
		}
		b.WriteString("  " + label + "\n")
	}
	if rest := len(m.matches) - end; rest > 0 {
		b.WriteString(mutedStyle.Render(fmt.Sprintf("    … %d more", rest)) + "\n")
	}
	return b.String()
}

func (m model) confirmView() string {
	box := dangerBox.Render(
		dangerStyle.Render("ALL DATA ON THIS DRIVE WILL BE ERASED") + "\n\n" +
			mutedStyle.Render("Drive  ") + driveSummary(m.drive) + "\n" +
			mutedStyle.Render("Image  ") + filepath.Base(m.iso) + " · " + formatBytes(m.isoSize))
	return box + "\n\nType the drive path to confirm:\n" + m.confirmInput.View() + "\n"
}

func (m model) writingView() string {
	fraction := 1.0
	if m.isoSize > 0 {
		fraction = float64(m.written) / float64(m.isoSize)
	}
	elapsed := time.Since(m.started)
	speed := float64(m.written) / max(elapsed.Seconds(), 1e-3)
	eta := time.Duration(0)
	if speed > 0 {
		eta = time.Duration(float64(m.isoSize-m.written) / speed * float64(time.Second))
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("Writing %s → %s\n\n", filepath.Base(m.iso), m.drive.Path))
	b.WriteString(m.bar.ViewAs(fraction) + "\n\n")
	b.WriteString(fmt.Sprintf("%s of %s  ·  %s/s  ·  %s elapsed  ·  %s left\n",
		formatBytes(m.written), formatBytes(m.isoSize), formatBytes(int64(speed)),
		formatDuration(elapsed), formatDuration(eta)))
	if m.flushing {
		b.WriteString("\n" + m.spin.View() + " Flushing the last data to the drive. Don't unplug it.\n")
	}
	return b.String()
}

func (m model) doneView() string {
	speed := float64(m.written) / max(m.elapsed.Seconds(), 1e-3)
	return okStyle.Render("✓ Done.") + fmt.Sprintf(" Wrote %s to %s in %s (%s/s).\n",
		formatBytes(m.written), m.drive.Path, formatDuration(m.elapsed), formatBytes(int64(speed))) +
		"The drive is safe to remove.\n"
}

func (m model) failedView() string {
	s := dangerStyle.Render("✗ Write failed: ") + friendlyError(m.err, m.drive.Path) + "\n"
	if m.written > 0 {
		s += "The drive was partly written and probably won't boot. Run this again to rewrite it.\n"
	}
	return s
}

func friendlyError(err error, devPath string) string {
	switch {
	case errors.Is(err, errAborted):
		return "aborted."
	case errors.Is(err, syscall.EBUSY):
		return devPath + " is in use. Is one of its partitions mounted?"
	case errors.Is(err, fs.ErrNotExist):
		return devPath + " is gone. Was the drive unplugged?"
	}
	return err.Error()
}

func formatBytes(n int64) string {
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	value := float64(n)
	unit := 0
	for value >= 1024 && unit < len(units)-1 {
		value /= 1024
		unit++
	}
	if unit == 0 {
		return fmt.Sprintf("%d B", n)
	}
	return fmt.Sprintf("%.1f %s", value, units[unit])
}

func formatDuration(d time.Duration) string {
	s := int(max(d, 0).Seconds())
	if s >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", s/3600, s%3600/60, s%60)
	}
	return fmt.Sprintf("%02d:%02d", s/60, s%60)
}

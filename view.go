package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// subview is a full-screen view stacked over the dashboard: command output, a GitHub run, a job's log.
type subview interface {
	ID() int
	Init() tea.Cmd
	// Update returns false when the view should close.
	Update(tea.Msg) (bool, tea.Cmd)
	View(w, h int) string
	Close()
}

// viewMsg is a message for one view, dropped if that view has closed.
type viewMsg interface{ viewID() int }

type pushViewMsg struct{ v subview }

var (
	viewSeqMu sync.Mutex
	viewSeq   int
)

func nextViewID() int {
	viewSeqMu.Lock()
	defer viewSeqMu.Unlock()
	viewSeq++
	return viewSeq
}

func pushView(v subview) tea.Cmd { return func() tea.Msg { return pushViewMsg{v} } }

// ---- command output ----

const maxLines = 10000

type linesMsg struct {
	id    int
	gen   int // which run of the command, so a rerun ignores the last one's tail
	lines []string
	done  bool
	err   error
}

func (m linesMsg) viewID() int { return m.id }

// commandView runs a command and shows its output, streaming, with scrolling, follow and a filter.
type commandView struct {
	id      int
	title   string
	dir     string
	command string
	// argv, when set, is run directly instead of command, with no shell to interpret what is in it.
	argv []string
	// guard asks before closing while the command still runs, for deploys that must not be cut off.
	guard bool
	// ingest turns a raw output line into display lines; format styles one of them.
	ingest func(string) []string
	format func(string) styled

	lines   []string
	running bool
	exitErr error
	started time.Time
	ended   time.Time

	gen    int
	ch     chan string
	stop   chan struct{}
	cancel context.CancelFunc
	errMu  sync.Mutex
	waitEr error

	table *tableState // a table being collected from the output

	scrollBack int // rows above the bottom; 0 follows new output
	wrap       bool
	filter     string
	typing     bool
	draft      string
	confirming *confirmation // asking before stopping a deploy midway
	lastWidth  int
}

func newCommandView(title, dir, command string) *commandView {
	return &commandView{id: nextViewID(), title: title, dir: dir, command: command, wrap: true,
		ingest: func(s string) []string { return []string{s} }, format: formatLogLine}
}

func (v *commandView) ID() int { return v.id }

func (v *commandView) Init() tea.Cmd { return v.start() }

func (v *commandView) start() tea.Cmd {
	v.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "sh", "-c", v.command)
	if len(v.argv) > 0 {
		cmd = exec.CommandContext(ctx, v.argv[0], v.argv[1:]...)
	}
	cmd.Dir = v.dir
	// Its own process group, so closing the view stops whatever the shell started too.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		time.AfterFunc(2*time.Second, func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
		return nil
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return func() tea.Msg { return linesMsg{id: v.id, gen: v.gen, done: true, err: err} }
	}
	cmd.Stderr = cmd.Stdout
	v.gen++
	v.running, v.exitErr, v.started = true, nil, time.Now()
	v.errMu.Lock()
	v.waitEr = nil
	v.errMu.Unlock()
	shown := v.command
	if len(v.argv) > 0 {
		quoted := make([]string, len(v.argv))
		for i, a := range v.argv {
			quoted[i] = a
			if strings.ContainsAny(a, " '\"$`\\;&|<>(){}*?!#~") {
				quoted[i] = shellQuote(a)
			}
		}
		shown = strings.Join(quoted, " ")
	}
	v.lines = append(v.lines[:0], "$ "+shown)
	v.table = nil
	if err := cmd.Start(); err != nil {
		cancel()
		v.running = false
		gen := v.gen
		return func() tea.Msg { return linesMsg{id: v.id, gen: gen, done: true, err: err} }
	}
	ch, stop := make(chan string, 1024), make(chan struct{})
	v.ch, v.stop, v.cancel = ch, stop, cancel
	go func() {
		defer close(ch)
		r := bufio.NewReaderSize(out, 64*1024)
		for {
			line, err := readLine(r)
			if err == nil || line != "" {
				select {
				case ch <- line:
				case <-stop:
					_ = cmd.Wait()
					return
				}
			}
			if err != nil {
				if err != io.EOF {
					select {
					case ch <- "read error: " + err.Error():
					case <-stop:
					}
				}
				break
			}
		}
		werr := cmd.Wait()
		v.errMu.Lock()
		v.waitEr = werr
		v.errMu.Unlock()
	}()
	return v.next()
}

// next collects whatever output is waiting, up to a frame's worth, so a fast stream redraws in batches.
func (v *commandView) next() tea.Cmd {
	ch, id, gen := v.ch, v.id, v.gen
	return func() tea.Msg {
		line, ok := <-ch
		if !ok {
			return v.finished(gen, nil)
		}
		lines := []string{line}
		deadline := time.After(40 * time.Millisecond)
		for len(lines) < 2000 {
			select {
			case l, ok := <-ch:
				if !ok {
					return v.finished(gen, lines)
				}
				lines = append(lines, l)
			case <-deadline:
				return linesMsg{id: id, gen: gen, lines: lines}
			}
		}
		return linesMsg{id: id, gen: gen, lines: lines}
	}
}

// maxLineBytes caps one line of output, so a single endless line cannot use up memory.
const maxLineBytes = 64 << 10

// readLine reads up to a newline, keeping at most maxLineBytes and discarding the rest of a longer line.
func readLine(r *bufio.Reader) (string, error) {
	var buf []byte
	cut := false
	for {
		chunk, err := r.ReadSlice('\n')
		if room := maxLineBytes - len(buf); room > 0 {
			if len(chunk) > room {
				chunk, cut = chunk[:room], true
			}
			buf = append(buf, chunk...)
		} else if len(chunk) > 0 {
			cut = true
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		line := strings.TrimRight(string(buf), "\r\n")
		if cut {
			line = truncateBytes(line, maxLineBytes) + " … (line cut at 64 KB)"
		}
		return line, err
	}
}

func (v *commandView) finished(gen int, lines []string) linesMsg {
	v.errMu.Lock()
	defer v.errMu.Unlock()
	return linesMsg{id: v.id, gen: gen, lines: lines, done: true, err: v.waitEr}
}

func (v *commandView) Close() {
	if v.stop != nil {
		close(v.stop)
		v.stop = nil
	}
	if v.cancel != nil {
		v.cancel()
		v.cancel = nil
	}
}

func (v *commandView) Update(msg tea.Msg) (bool, tea.Cmd) {
	switch msg := msg.(type) {
	case linesMsg:
		if msg.gen != v.gen {
			return true, nil
		}
		added := 0
		add := func(l string) {
			v.lines = append(v.lines, l)
			if v.matches(l) {
				added += v.rowCount(l)
			}
		}
		for _, raw := range msg.lines {
			if noise(raw) {
				continue
			}
			for _, l := range v.ingest(stripANSI(raw)) {
				for _, out := range v.tablePush(l) {
					add(out)
				}
			}
		}
		if msg.done {
			for _, out := range v.tableFlush() {
				add(out)
			}
		}
		if len(v.lines) > maxLines {
			v.lines = v.lines[len(v.lines)-maxLines:]
		}
		if v.scrollBack > 0 {
			v.scrollBack += added // hold the view still while reading back
		}
		if msg.done {
			v.running, v.exitErr, v.ended = false, msg.err, time.Now()
			return true, nil
		}
		return true, v.next()
	case tea.KeyMsg:
		return v.key(msg)
	}
	return true, nil
}

func (v *commandView) key(msg tea.KeyMsg) (bool, tea.Cmd) {
	if v.typing {
		switch msg.Type {
		case tea.KeyEnter:
			v.typing, v.filter, v.scrollBack = false, v.draft, 0
		case tea.KeyEsc:
			v.typing = false
		case tea.KeyBackspace:
			if r := []rune(v.draft); len(r) > 0 {
				v.draft = string(r[:len(r)-1])
			}
		case tea.KeyRunes, tea.KeySpace:
			v.draft += string(msg.Runes)
		}
		return true, nil
	}
	if v.confirming != nil {
		done, ok := v.confirming.key(msg)
		if done {
			v.confirming = nil
		}
		if ok {
			v.Close()
			return false, nil
		}
		return true, nil
	}
	page := 20
	switch msg.String() {
	case "esc", "q":
		if v.filter != "" && msg.String() == "esc" {
			v.filter, v.scrollBack = "", 0
			return true, nil
		}
		if v.guard && v.running {
			v.confirming = newConfirmation("Still running. Stop it and close?", nil, nil)
			return true, nil
		}
		v.Close()
		return false, nil
	case "up", "k":
		v.scrollBack++
	case "down", "j":
		v.scrollBack = max(0, v.scrollBack-1)
	case "pgup", "b", "ctrl+u":
		v.scrollBack += page
	case "pgdown", " ", "ctrl+d":
		v.scrollBack = max(0, v.scrollBack-page)
	case "g", "home":
		v.scrollBack = 1 << 30 // clamped to the top when drawn
	case "G", "end", "f":
		v.scrollBack = 0
	case "w":
		v.wrap = !v.wrap
	case "/":
		v.typing, v.draft = true, v.filter
	case "r":
		if !v.running {
			v.scrollBack = 0
			return true, v.start()
		}
	}
	return true, nil
}

// noise is output nobody reads: gcdeck's own health checks showing up in the app's request log, and ssh's warning
// that it has no terminal, which every non-interactive remote command prints.
func noise(line string) bool {
	return strings.Contains(line, healthUserAgent) ||
		strings.HasPrefix(line, "Pseudo-terminal will not be allocated")
}

func (v *commandView) matches(l string) bool {
	return v.filter == "" || strings.Contains(strings.ToLower(l), strings.ToLower(v.filter))
}

func (v *commandView) rowCount(l string) int {
	if !v.wrap || v.lastWidth <= 0 || strings.HasPrefix(l, markTable) {
		return 1
	}
	return len(wrapStyled(v.format(l), v.lastWidth))
}

func (v *commandView) View(w, h int) string {
	return v.render(w, h-1) + "\n" + v.footer()
}

// render draws the status line and h-1 rows of output, without the key help, so another view can embed it.
func (v *commandView) render(w, h int) string {
	v.lastWidth = w
	status := ""
	switch {
	case v.running:
		status = sWarn.Render("● running " + time.Since(v.started).Round(time.Second).String())
	case v.exitErr != nil:
		status = sBad.Render("✗ " + describeExit(v.exitErr))
	default:
		status = sGood.Render("✓ finished") + sMuted.Render(" in "+v.ended.Sub(v.started).Round(time.Second).String())
	}
	head := sTitle.Render(v.title) + "  " + status
	if v.filter != "" {
		head += sWarn.Render("  filter: " + v.filter)
	}
	if v.scrollBack == 0 {
		head += sMuted.Render("  following")
	}

	bodyH := max(1, h-1)
	// Walk up from the newest line, collecting display rows, newest first.
	need := v.scrollBack + bodyH
	var rows []string
	for i := len(v.lines) - 1; i >= 0 && len(rows) < need; i-- {
		if !v.matches(v.lines[i]) {
			continue
		}
		var r []string
		if v.wrap && !strings.HasPrefix(v.lines[i], markTable) {
			r = wrapStyled(v.format(v.lines[i]), w)
		} else {
			r = []string{truncStyled(v.format(v.lines[i]), w)}
		}
		for j := len(r) - 1; j >= 0; j-- {
			rows = append(rows, r[j])
		}
	}
	if v.scrollBack > len(rows)-bodyH {
		v.scrollBack = max(0, len(rows)-bodyH)
	}
	end := min(len(rows), v.scrollBack+bodyH)
	visible := rows[v.scrollBack:end]
	rowsOut := make([]string, 0, bodyH+1)
	rowsOut = append(rowsOut, head)
	for i := bodyH - 1; i >= 0; i-- {
		if i < len(visible) {
			rowsOut = append(rowsOut, visible[i])
		} else {
			rowsOut = append(rowsOut, "")
		}
	}
	return strings.Join(rowsOut, "\n")
}

func (v *commandView) footer() string {
	if v.typing {
		return sWarn.Render(" filter: ") + v.draft + "█" + sMuted.Render("  enter apply · esc cancel")
	}
	if v.confirming != nil {
		return v.confirming.view()
	}
	keys := [][2]string{{"↑↓ pgup pgdn", "scroll"}, {"g G", "top/bottom"}, {"/", "filter"}, {"w", "wrap"}}
	if !v.running {
		keys = append(keys, [2]string{"r", "run again"})
	}
	keys = append(keys, [2]string{"esc", "back"})
	return keyHelp(keys)
}

func keyHelp(keys [][2]string) string {
	var parts []string
	for _, k := range keys {
		parts = append(parts, sTitle.Render(k[0])+" "+sMuted.Render(k[1]))
	}
	return " " + strings.Join(parts, sMuted.Render(" · "))
}

// ---- styled lines ----

// styled is one display line: a dim prefix (a time, say) and text in one style.
type styled struct {
	prefix string
	text   string
	style  lipgloss.Style
}

func wrapStyled(s styled, width int) []string {
	pw := lipgloss.Width(s.prefix)
	avail := width - pw
	if avail < 10 {
		avail, pw = width, 0
		s.text = s.prefix + s.text
		s.prefix = ""
	}
	runes := []rune(s.text)
	var rows []string
	for first := true; first || len(runes) > 0; first = false {
		n := min(avail, len(runes))
		chunk := string(runes[:n])
		runes = runes[n:]
		lead := strings.Repeat(" ", pw)
		if first {
			lead = sMuted.Render(s.prefix)
		}
		rows = append(rows, lead+s.style.Render(chunk))
	}
	return rows
}

func truncStyled(s styled, width int) string {
	avail := max(1, width-lipgloss.Width(s.prefix))
	return sMuted.Render(s.prefix) + s.style.Render(truncate(s.text, avail))
}

// ---- pane-free action helpers ----

func viewAction(title, dir, command string, guard bool) tea.Cmd {
	v := newCommandView(title, dir, command)
	v.guard = guard
	return pushView(v)
}

func describeExit(err error) string {
	if ee, ok := err.(*exec.ExitError); ok {
		return fmt.Sprintf("exit %d", ee.ExitCode())
	}
	return errText(err)
}

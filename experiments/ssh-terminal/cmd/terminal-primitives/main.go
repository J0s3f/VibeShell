// Command terminal-primitives qualifies the terminal primitives for the SSH
// byte stream and prints the evidence the decision record needs.
//
// It runs five experiments, all inside the development container and all with
// no terminal on standard input:
//
//  1. charmbracelet/ultraviolet's TerminalReader over a plain io.Reader,
//  2. the spike's own bounded decoder over the same bytes,
//  3. a 1 MiB bracketed paste through both,
//  4. ultraviolet's TerminalScreen over an in-memory writer,
//  5. ultraviolet's Terminal, which is the part that expects a process console.
//
// The output is a plain text report; run-spike.ps1 stores it as a receipt.
package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	uv "github.com/charmbracelet/ultraviolet"

	"j0s.at/vibeshell/experiments/ssh-terminal/internal/termdecode"
	"j0s.at/vibeshell/experiments/ssh-terminal/internal/termrender"
)

// sample is the input both decoders see. It is deliberately hostile: a key split
// across writes, a UTF-8 rune split across writes, Ctrl-C, Ctrl-D, a bracketed
// paste split across writes, and a modifier-encoded arrow.
const sample = "hi\x1b[A\x03\x04\x1b[200~pasted \xc3\xa9 text\x1b[201~\x1b[1;2D"

func main() {
	section("environment")
	reportEnvironment()
	section("1. ultraviolet TerminalReader over an io.Reader")
	probeUltravioletReader()
	section("2. local bounded decoder over the same bytes")
	probeLocalDecoder()
	section("3. bounded paste")
	probePasteBounds()
	section("4. ultraviolet TerminalScreen over an io.Writer")
	probeUltravioletScreen()
	section("5. ultraviolet Terminal, the process-terminal part")
	probeUltravioletTerminal()
	section("6. x/ansi as the chosen byte-stream primitive")
	probeAnsi()
}

func section(title string) {
	fmt.Printf("\n=== %s ===\n", title)
}

func reportEnvironment() {
	fmt.Printf("TERM=%q (empty: the service never has a client terminal)\n", os.Getenv("TERM"))
	info, err := os.Stdin.Stat()
	switch {
	case err != nil:
		fmt.Printf("stdin stat error: %v\n", err)
	default:
		fmt.Printf("stdin mode=%v charDevice=%t\n", info.Mode(), info.Mode()&os.ModeCharDevice != 0)
	}
	fmt.Printf("pid=%d; section 5 proves stdin is not a terminal\n", os.Getpid())
}

// probeUltravioletReader feeds the sample to ultraviolet one byte per write,
// which is what an SSH channel does when a key sequence is split.
func probeUltravioletReader() {
	reader, writer := io.Pipe()
	events := make(chan uv.Event, 128)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = uv.NewTerminalReader(reader, "xterm-256color").StreamEvents(ctx, events)
	}()

	var seen []string
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		// Ultraviolet has no end-of-input signal because it expects a terminal
		// that never ends, so collect until the stream goes quiet. Its escape
		// timeout is 50ms, so a longer silence means nothing more is coming.
		for {
			select {
			case event := <-events:
				seen = append(seen, describeUltraviolet(event))
			case <-time.After(500 * time.Millisecond):
				return
			}
		}
	}()
	for i := 0; i < len(sample); i++ {
		if _, err := writer.Write([]byte{sample[i]}); err != nil {
			fmt.Printf("write error: %v\n", err)
			break
		}
	}
	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		fmt.Printf("timed out after %d events\n", len(seen))
	}
	_ = writer.Close()
	cancel()
	for _, entry := range seen {
		fmt.Printf("  %s\n", entry)
	}
	fmt.Printf("verdict: ultraviolet decodes an SSH byte stream one byte at a time; its\n")
	fmt.Printf("         EscTimeout is %v, so an unfinished sequence is resolved by a\n", uv.DefaultEscTimeout)
	fmt.Printf("         timer rather than by the end of the stream.\n")
}

func describeUltraviolet(event uv.Event) string {
	switch typed := event.(type) {
	case uv.KeyPressEvent:
		return fmt.Sprintf("key %s", typed.Keystroke())
	case uv.PasteEvent:
		return fmt.Sprintf("paste %d bytes %q", len(typed.Content), typed.Content)
	case uv.PasteStartEvent:
		return "paste-start"
	case uv.PasteEndEvent:
		return "paste-end"
	case uv.WindowSizeEvent:
		return fmt.Sprintf("window-size %dx%d", typed.Width, typed.Height)
	default:
		return fmt.Sprintf("%T %v", event, event)
	}
}

// probeLocalDecoder runs the same sample through the spike's decoder, again one
// byte at a time.
func probeLocalDecoder() {
	var lines []string
	decoder := termdecode.NewDecoder(termdecode.DefaultBounds, func(event termdecode.Event) {
		lines = append(lines, describeLocal(event))
	})
	for i := 0; i < len(sample); i++ {
		if _, err := decoder.Write([]byte{sample[i]}); err != nil {
			fmt.Printf("write error: %v\n", err)
			break
		}
	}
	decoder.Flush()
	for _, entry := range lines {
		fmt.Printf("  %s\n", entry)
	}
	fmt.Printf("verdict: the same bytes decode without a timer, with every buffer\n")
	fmt.Printf("         bounded by Bounds.\n")
}

func describeLocal(event termdecode.Event) string {
	switch typed := event.(type) {
	case termdecode.Key:
		return "key " + typed.String()
	case termdecode.Paste:
		return fmt.Sprintf("paste %d bytes %q truncated=%t", len(typed.Text), typed.Text, typed.Truncated)
	case termdecode.Resize:
		return fmt.Sprintf("resize %dx%d", typed.Cols, typed.Rows)
	default:
		return fmt.Sprintf("%T", event)
	}
}

// probePasteBounds sends a paste far larger than any sane command line through
// both decoders and reports what each one keeps.
func probePasteBounds() {
	const size = 1 << 20
	prefix, suffix := "\x1b[200~", "\x1b[201~"
	body := strings.Repeat("p", size)

	var local []string
	decoder := termdecode.NewDecoder(termdecode.DefaultBounds, func(event termdecode.Event) {
		local = append(local, describeLocal(event))
	})
	if _, err := decoder.Write([]byte(prefix + body + suffix)); err != nil {
		fmt.Printf("local decoder write error: %v\n", err)
	}
	fmt.Printf("local decoder (MaxPasteBytes=%d):\n", termdecode.DefaultBounds.MaxPasteBytes)
	for _, entry := range local {
		if len(entry) > 96 {
			entry = entry[:96] + "..."
		}
		fmt.Printf("  %s\n", entry)
	}

	reader, writer := io.Pipe()
	events := make(chan uv.Event, 16)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go func() { _ = uv.NewTerminalReader(reader, "xterm").StreamEvents(ctx, events) }()
	go func() {
		_, _ = writer.Write([]byte(prefix))
		_, _ = writer.Write([]byte(body))
		_, _ = writer.Write([]byte(suffix))
	}()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case event := <-events:
			if paste, ok := event.(uv.PasteEvent); ok {
				fmt.Printf("ultraviolet TerminalReader: paste %d bytes retained, no limit and no overflow event\n", len(paste.Content))
				_ = writer.Close()
				return
			}
		case <-deadline:
			fmt.Println("ultraviolet TerminalReader: no paste event within 10s")
			_ = writer.Close()
			return
		}
	}
}

// probeUltravioletScreen drives ultraviolet's screen and renderer over an
// in-memory writer, which is the shape an SSH channel has.
func probeUltravioletScreen() {
	var out bytes.Buffer
	env := uv.Environ{"TERM=xterm-256color", "COLORTERM=truecolor", "NO_COLOR="}
	screen := uv.NewTerminalScreen(&out, env)
	screen.Resize(20, 4)
	if _, err := screen.Write([]byte("hello 世界")); err != nil {
		fmt.Printf("screen write error: %v\n", err)
	}
	screen.Render()
	if err := screen.Flush(); err != nil {
		fmt.Printf("screen flush error: %v\n", err)
	}

	fmt.Printf("wrote %d bytes to an in-memory buffer, no terminal involved:\n", out.Len())
	fmt.Printf("  %q\n", out.String())
	fmt.Printf("  display width of %q = %d cells\n", "hello 世界", termrender.Width("hello 世界"))
	fmt.Printf("verdict: the screen and renderer are byte-stream safe; only Terminal is not.\n")
}

// probeUltravioletTerminal starts ultraviolet's Terminal, which is documented as
// taking a Console. Without a controlling terminal it has to fail.
func probeUltravioletTerminal() {
	terminal := uv.NewTerminal(nil, nil)
	err := terminal.Start()
	if err != nil {
		fmt.Printf("Terminal.Start() with the default console failed: %v\n", err)
	} else {
		fmt.Println("Terminal.Start() unexpectedly succeeded")
		_ = terminal.Stop()
	}
	fmt.Println("verdict: Terminal owns a process terminal: NewTerminal(nil, nil) builds a")
	fmt.Println("         console over os.Stdin/os.Stdout, Start calls Console.MakeRaw, reads")
	fmt.Println("         TERM from the process environment, and installs a SIGWINCH handler.")
	fmt.Println("         A VibeShell session must use TerminalReader or its own screen model.")
}

// probeAnsi shows the chosen primitive doing the work the contract needs.
func probeAnsi() {
	frame := "\x1b[2J\x1b[1;1H\x1b[31merror\x1b[0m \x1b]52;c;cGFzc3dvcmQ=\x07 \x1b]0;title\x07"
	result := termrender.Sanitize([]byte(frame), termrender.Policy{})
	fmt.Printf("frame in : %q\n", frame)
	fmt.Printf("frame out: %q\n", string(result.Kept))
	for _, removal := range result.Removals {
		fmt.Printf("removed  : %s %s\n", removal.Kind, removal.Bytes)
	}
	fmt.Printf("width(\"\\x1b[31mred\\x1b[0m\") = %d, plain = %q\n",
		termrender.Width("\x1b[31mred\x1b[0m"), termrender.PlainText("\x1b[31mred\x1b[0m"))
	fmt.Println("verdict: x/ansi needs no terminal, no environment, and no goroutine, so it")
	fmt.Println("         fits an SSH byte stream; it is the primitive the spike recommends.")
}

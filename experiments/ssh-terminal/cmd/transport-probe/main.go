// Command transport-probe measures the SSH transport behaviour the service
// depends on: how long one keypress round trip takes over a loopback
// connection, how quickly a disconnect cancels a session, how long a session
// takes to start, and what a client that stops reading costs.
//
// It runs against an in-process server inside the development container and
// prints a table; run-spike.ps1 stores the table as a receipt. The numbers are
// loopback figures for a spike, not service targets.
package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"

	"j0s.at/vibeshell/experiments/ssh-terminal/internal/sshserver"
	"j0s.at/vibeshell/experiments/ssh-terminal/internal/termdecode"
)

const (
	keypressRounds = 200
	sessionStarts  = 50
	disconnects    = 20
)

func main() {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server, err := sshserver.NewServer(sshserver.Options{
		Mode:         sshserver.ModePublic,
		ShellHandler: echoHandler,
		Logger:       logger,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "server: %v\n", err)
		os.Exit(1)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintf(os.Stderr, "listen: %v\n", err)
		os.Exit(1)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- server.Serve(ctx, listener) }()
	defer func() {
		cancel()
		_ = listener.Close()
		<-served
	}()

	address := listener.Addr().String()
	fmt.Println("transport probe (loopback, in-process server, Go SSH client)")
	fmt.Printf("address=%s rounds=%d session_starts=%d disconnects=%d\n\n",
		address, keypressRounds, sessionStarts, disconnects)

	client := dial(address)
	reportKeypressLatency(client)
	reportSessionStart(address)
	reportDisconnectLatency(address)
	reportBackPressure()
}

// echoHandler echoes every rune and ignores everything else, so the output
// stream is exactly one byte per keypress.
func echoHandler(session *sshserver.Session) {
	for {
		select {
		case <-session.Context().Done():
			return
		case <-session.InputClosed():
			_ = session.Exit(0)
			return
		case event := <-session.Events():
			key, ok := event.(termdecode.Key)
			if !ok || key.Name != termdecode.KeyRune {
				continue
			}
			_, _ = session.Write([]byte{byte(key.Rune)})
		}
	}
}

func dial(address string) *ssh.Client {
	client, err := ssh.Dial("tcp", address, &ssh.ClientConfig{
		User:            "probe",
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         10 * time.Second,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "dial: %v\n", err)
		os.Exit(1)
	}
	return client
}

// reportKeypressLatency measures a write, decode, and echo round trip.
func reportKeypressLatency(client *ssh.Client) {
	session, err := client.NewSession()
	if err != nil {
		fmt.Fprintf(os.Stderr, "session: %v\n", err)
		os.Exit(1)
	}
	defer session.Close()
	if err := session.RequestPty("xterm-256color", 24, 80, ssh.TerminalModes{}); err != nil {
		fmt.Fprintf(os.Stderr, "pty: %v\n", err)
		os.Exit(1)
	}
	stdout, err := session.StdoutPipe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "stdout pipe: %v\n", err)
		os.Exit(1)
	}
	stdin, err := session.StdinPipe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "stdin pipe: %v\n", err)
		os.Exit(1)
	}
	reader := bufio.NewReader(stdout)
	if err := session.Shell(); err != nil {
		fmt.Fprintf(os.Stderr, "shell: %v\n", err)
		os.Exit(1)
	}

	samples := make([]time.Duration, 0, keypressRounds)
	for i := 0; i < keypressRounds; i++ {
		letter := byte('a' + i%26)
		started := time.Now()
		if _, err := stdin.Write([]byte{letter}); err != nil {
			fmt.Fprintf(os.Stderr, "write: %v\n", err)
			os.Exit(1)
		}
		echoed, err := reader.ReadByte()
		if err != nil {
			fmt.Fprintf(os.Stderr, "read: %v\n", err)
			os.Exit(1)
		}
		if echoed != letter {
			fmt.Fprintf(os.Stderr, "echo %q, want %q\n", echoed, letter)
			os.Exit(1)
		}
		samples = append(samples, time.Since(started))
	}
	printDistribution("keypress round trip", samples)
}

// reportSessionStart measures opening a full session: channel, pty, shell.
func reportSessionStart(address string) {
	client := dial(address)
	defer client.Close()
	samples := make([]time.Duration, 0, sessionStarts)
	for i := 0; i < sessionStarts; i++ {
		started := time.Now()
		session, err := client.NewSession()
		if err != nil {
			fmt.Fprintf(os.Stderr, "session: %v\n", err)
			os.Exit(1)
		}
		if err := session.RequestPty("xterm-256color", 24, 80, ssh.TerminalModes{}); err != nil {
			fmt.Fprintf(os.Stderr, "pty: %v\n", err)
			os.Exit(1)
		}
		if err := session.Shell(); err != nil {
			fmt.Fprintf(os.Stderr, "shell: %v\n", err)
			os.Exit(1)
		}
		samples = append(samples, time.Since(started))
		_ = session.Close()
	}
	printDistribution("session start (channel+pty+shell)", samples)
}

// reportDisconnectLatency measures how long the server takes to cancel a
// session after the client disappears without a clean close.
func reportDisconnectLatency(address string) {
	samples := make([]time.Duration, 0, disconnects)
	for i := 0; i < disconnects; i++ {
		// The handler runs on its own goroutine, so the moment the client is
		// closed travels through an atomic rather than a captured variable.
		var closedAt atomic.Int64
		cancelled := make(chan time.Duration, 1)
		ready := make(chan struct{})
		server := mustServer(func(session *sshserver.Session) {
			close(ready)
			<-session.Context().Done()
			cancelled <- time.Since(time.Unix(0, closedAt.Load()))
		})
		listener := listen()
		served := make(chan error, 1)
		go func() { served <- server.Serve(context.Background(), listener) }()

		client := dial(listener.Addr().String())
		session, err := client.NewSession()
		if err != nil {
			fmt.Fprintf(os.Stderr, "session: %v\n", err)
			os.Exit(1)
		}
		if err := session.RequestPty("xterm", 24, 80, ssh.TerminalModes{}); err != nil {
			fmt.Fprintf(os.Stderr, "pty: %v\n", err)
			os.Exit(1)
		}
		if err := session.Shell(); err != nil {
			fmt.Fprintf(os.Stderr, "shell: %v\n", err)
			os.Exit(1)
		}
		<-ready
		closedAt.Store(time.Now().UnixNano())
		if err := client.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "close: %v\n", err)
			os.Exit(1)
		}
		samples = append(samples, <-cancelled)
		_ = listener.Close()
		<-served
	}
	printDistribution("disconnect to cancelled session context", samples)
}

// reportBackPressure measures what a client that stops reading costs: the
// session must stay bounded and must not block its own writer.
func reportBackPressure() {
	const (
		flood = 8 << 20
		limit = 64 << 10
	)
	type floodResult struct {
		written int64
		dropped int64
		elapsed time.Duration
	}
	result := make(chan floodResult, 1)
	server := mustServer(func(session *sshserver.Session) {
		go func() {
			payload := strings.Repeat("s", 32<<10)
			startedAt := time.Now()
			var total int64
			for i := 0; i < flood/len(payload); i++ {
				n, _ := session.Write([]byte(payload))
				total += int64(n)
			}
			result <- floodResult{
				written: total,
				dropped: session.DroppedBytes(),
				elapsed: time.Since(startedAt),
			}
		}()
		<-session.Context().Done()
	})
	listener := listen()
	served := make(chan error, 1)
	go func() { served <- server.Serve(context.Background(), listener) }()

	client := dial(listener.Addr().String())
	session, err := client.NewSession()
	if err != nil {
		fmt.Fprintf(os.Stderr, "session: %v\n", err)
		os.Exit(1)
	}
	if err := session.RequestPty("xterm", 24, 80, ssh.TerminalModes{}); err != nil {
		fmt.Fprintf(os.Stderr, "pty: %v\n", err)
		os.Exit(1)
	}
	if err := session.Shell(); err != nil {
		fmt.Fprintf(os.Stderr, "shell: %v\n", err)
		os.Exit(1)
	}
	// The client never reads the session.
	select {
	case measured := <-result:
		fmt.Printf("%-40s wrote=%-9d dropped=%-9d in %v with a %d byte queue\n",
			"output back pressure", measured.written, measured.dropped, measured.elapsed, limit)
	case <-time.After(30 * time.Second):
		fmt.Println("output back pressure: the session writer blocked, which the bound must prevent")
	}
	_ = client.Close()
	_ = listener.Close()
	<-served
}

func mustServer(handler sshserver.ShellHandler) *sshserver.Server {
	server, err := sshserver.NewServer(sshserver.Options{
		Mode:         sshserver.ModePublic,
		ShellHandler: handler,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "server: %v\n", err)
		os.Exit(1)
	}
	return server
}

func listen() net.Listener {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintf(os.Stderr, "listen: %v\n", err)
		os.Exit(1)
	}
	return listener
}

// printDistribution reports the median and tail of a set of durations.
func printDistribution(label string, samples []time.Duration) {
	sorted := slices.Clone(samples)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	quantile := func(fraction float64) time.Duration {
		index := int(float64(len(sorted)-1) * fraction)
		return sorted[index]
	}
	fmt.Printf("%-40s n=%3d min=%-10v p50=%-10v p95=%-10v p99=%-10v max=%-10v\n",
		label, len(sorted), sorted[0], quantile(0.50), quantile(0.95), quantile(0.99), sorted[len(sorted)-1])
}

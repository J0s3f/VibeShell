// Command testserver runs a VibeShell SSH adapter with an echo handler for
// the scripted OpenSSH end-to-end test (testdata/ssh_e2e.sh). It is a test
// helper, not part of the service: the handler greets the session, echoes
// stdin back to stdout until EOF, and exits 0. It never starts a process or
// interprets commands.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"

	adapter "j0s.at/vibeshell/internal/adapters/ssh"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:2222", "listen address")
	hostKeyPath := flag.String("hostkey", "", "host key file (created when absent; empty uses an ephemeral key)")
	mode := flag.String("mode", "public", "authentication mode: public or password")
	user := flag.String("user", "", "single accepted username in password mode")
	pass := flag.String("pass", "", "password for -user in password mode")
	flag.Parse()

	signer, err := adapter.LoadOrGenerateHostKey(*hostKeyPath)
	if err != nil {
		log.Fatalf("host key: %v", err)
	}

	opts := adapter.Options{
		HostKey: signer,
		Handler: adapter.HandlerFunc(func(_ context.Context, sess *adapter.Session) {
			fmt.Fprintf(sess, "vibeshell-echo ready user=%s window=%dx%d\n",
				sess.Principal().Username, sess.Terminal().Cols, sess.Terminal().Rows)
			buf := make([]byte, 32<<10)
			for {
				n, rerr := sess.Read(buf)
				if n > 0 {
					if _, werr := sess.Write(buf[:n]); werr != nil {
						return
					}
				}
				if rerr != nil {
					_ = sess.Exit(0)
					return
				}
			}
		}),
	}
	switch *mode {
	case "public":
		opts.Mode = adapter.ModePublic
	case "password":
		if *user == "" {
			log.Fatal("password mode requires -user and -pass")
		}
		password, username := *pass, *user
		opts.Mode = adapter.ModePassword
		opts.Passwords = adapter.PasswordAuthenticatorFunc(
			func(gotUser, gotPass string) (adapter.Principal, error) {
				if gotUser != username || gotPass != password {
					return adapter.Principal{}, fmt.Errorf("%w: bad credentials", adapter.ErrUnauthenticated)
				}
				return adapter.Principal{Username: gotUser}, nil
			})
	default:
		log.Fatalf("unknown -mode %q", *mode)
	}

	server, err := adapter.NewServer(opts)
	if err != nil {
		log.Fatalf("new server: %v", err)
	}
	listener, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	fmt.Printf("listening on %s mode=%s\n", listener.Addr(), *mode)
	if err := server.Serve(context.Background(), listener); err != nil {
		fmt.Fprintf(os.Stderr, "serve: %v\n", err)
		os.Exit(1)
	}
}

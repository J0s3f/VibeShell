// Command sample exports the canonical synthetic session through all
// three formats into receipts/sample/ for reviewer inspection.
package main

import (
	"fmt"
	"os"

	exportformats "j0s.at/vibeshell/experiments/export-formats"
)

func main() {
	dir := "receipts/sample"
	if len(os.Args) > 1 {
		dir = os.Args[1]
	}
	s := exportformats.CanonicalStream()
	b, err := exportformats.NewBundle(dir + "/bundle")
	must(err)
	tr, err := exportformats.NewTranscript(dir + "/transcript.txt")
	must(err)
	ac, err := exportformats.NewAsciicast(dir+"/session.cast", 80, 24, s.StartedAt/1000, "xterm-256color")
	must(err)
	for _, r := range s.Records {
		must(b.Write(r))
		must(tr.Write(r))
		must(ac.Write(r))
	}
	_, err = b.Close(s)
	must(err)
	must(tr.Close(s))
	must(ac.Close())
	fmt.Println("wrote", dir)
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

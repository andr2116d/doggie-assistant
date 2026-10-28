package console

import (
	"os"

	"golang.org/x/term"
)

// fileTerminal is the process's terminal, in raw mode while the console
// runs.
type fileTerminal struct {
	in, out    *os.File
	state      *term.State
	resized    <-chan struct{}
	stopResize func()
}

func openTerminal(in, out *os.File) (*fileTerminal, error) {
	state, err := term.MakeRaw(int(in.Fd()))
	if err != nil {
		return nil, err
	}
	t := &fileTerminal{in: in, out: out, state: state}
	t.resized, t.stopResize = watchResize()
	return t, nil
}

func (t *fileTerminal) Read(p []byte) (int, error)  { return t.in.Read(p) }
func (t *fileTerminal) Write(p []byte) (int, error) { return t.out.Write(p) }

func (t *fileTerminal) Width() (int, error) {
	w, _, err := term.GetSize(int(t.out.Fd()))
	return w, err
}

func (t *fileTerminal) Height() (int, error) {
	_, h, err := term.GetSize(int(t.out.Fd()))
	return h, err
}

func (t *fileTerminal) Resized() <-chan struct{} { return t.resized }

func (t *fileTerminal) Restore() error {
	t.stopResize()
	return term.Restore(int(t.in.Fd()), t.state)
}
